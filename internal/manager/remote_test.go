package manager

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cubeherd/internal/config"
	"cubeherd/internal/s3"
	"cubeherd/internal/server"
)

type fakeObj struct {
	data []byte
	mod  time.Time
}

// fakeBucket is a tiny path-style S3 (single PUT, list, get, delete).
type fakeBucket struct {
	mu   sync.Mutex
	objs map[string]fakeObj
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/bkt/")
	switch {
	case r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
		var keys []string
		for k := range f.objs {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>`,
				k, len(f.objs[k].data), f.objs[k].mod.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		fmt.Fprint(w, `</ListBucketResult>`)
	case r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = fakeObj{b, time.Now().Add(time.Duration(len(f.objs)) * time.Hour)}
	case r.Method == "GET":
		o, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Write(o.data)
	case r.Method == "DELETE":
		delete(f.objs, key)
		w.WriteHeader(204)
	}
}

func remoteFixture(t *testing.T) (*Manager, *server.Server, *fakeBucket) {
	dir := t.TempDir()
	st, err := server.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBucket{objs: map[string]fakeObj{}}
	srv := httptest.NewServer(fb)
	t.Cleanup(srv.Close)
	cfg.Update(func(s *config.Settings) {
		s.Remote = &s3.Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s", Prefix: "cubeherd", PathStyle: true}
	})
	m := New(nil, st, cfg, nil)
	s := &server.Server{ID: "srv", Name: "Srv", Backup: &server.BackupPlan{Interval: "daily", Time: "04:00", Keep: 1, Remote: true}}
	return m, s, fb
}

func (f *fakeBucket) put(key string, data string, mod time.Time) {
	f.mu.Lock()
	f.objs[key] = fakeObj{[]byte(data), mod}
	f.mu.Unlock()
}

func testOp() *Op { return &Op{notify: make(chan struct{})} }

func TestBackupsAllMergesLocalAndRemote(t *testing.T) {
	m, s, fb := remoteFixture(t)
	now := time.Now()
	os.MkdirAll(m.BackupDir(s), 0o755)
	os.WriteFile(filepath.Join(m.BackupDir(s), "20260101-040000_schedule.tar.zst"), []byte("local"), 0o644)
	fb.put("cubeherd/srv/20260101-040000_schedule.tar.zst", "local", now)
	fb.put("cubeherd/srv/20250101-040000_manual.tar.zst", "remote-only", now.Add(-24*time.Hour))
	fb.put("cubeherd/srv/notes.txt", "ignored", now)

	list, err := m.BackupsAll(context.Background(), s)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if list[0].Name != "20260101-040000_schedule.tar.zst" || !list[0].Local || !list[0].Remote {
		t.Fatalf("first entry: %+v", list[0])
	}
	if list[1].Local || !list[1].Remote || list[1].Reason != ReasonManual || list[1].Size != 11 {
		t.Fatalf("second entry: %+v", list[1])
	}
}

func TestUploadPrunesOldAutomaticBackups(t *testing.T) {
	m, s, fb := remoteFixture(t)
	old := time.Now().Add(-72 * time.Hour)
	fb.put("cubeherd/srv/20260101-040000_schedule.tar.zst", "a", old)
	fb.put("cubeherd/srv/20260102-040000_update.tar.zst", "b", old.Add(time.Hour))
	fb.put("cubeherd/srv/20250101-040000_manual.tar.zst", "m", old.Add(-time.Hour))
	os.MkdirAll(m.BackupDir(s), 0o755)
	name := "20260105-040000_schedule.tar.zst"
	os.WriteFile(filepath.Join(m.BackupDir(s), name), []byte("new"), 0o644)

	if err := m.upload(context.Background(), s, testOp(), name); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	var keys []string
	for k := range fb.objs {
		keys = append(keys, strings.TrimPrefix(k, "cubeherd/srv/"))
	}
	sort.Strings(keys)
	// Keep=1: only the newest automatic archive stays, the manual one is never pruned.
	want := "20250101-040000_manual.tar.zst,20260105-040000_schedule.tar.zst"
	if strings.Join(keys, ",") != want {
		t.Fatalf("remote keys %v", keys)
	}
}

func TestFetchAndDeleteEverywhere(t *testing.T) {
	m, s, fb := remoteFixture(t)
	name := "20260101-040000_manual.tar.zst"
	fb.put("cubeherd/srv/"+name, "payload", time.Now())

	if !m.hasRemoteBackup(s, name) {
		t.Fatal("remote archive not found")
	}
	if err := m.fetch(context.Background(), s, testOp(), name); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(m.BackupDir(s), name))
	if err != nil || string(got) != "payload" {
		t.Fatalf("fetched file: %q %v", got, err)
	}
	if entries, _ := os.ReadDir(m.BackupDir(s)); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
	if err := m.DeleteBackupEverywhere(context.Background(), s, name); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BackupPath(s, name); err == nil {
		t.Fatal("local copy still exists")
	}
	if m.hasRemoteBackup(s, name) {
		t.Fatal("remote copy still exists")
	}
	if err := m.fetch(context.Background(), s, testOp(), "../evil.tar.zst"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestNoRemoteConfigured(t *testing.T) {
	dir := t.TempDir()
	st, _ := server.NewStore(dir)
	cfg, _ := config.Load(dir)
	m := New(nil, st, cfg, nil)
	if m.HasRemote() {
		t.Fatal("remote reported without configuration")
	}
	s := &server.Server{ID: "srv"}
	if list, err := m.BackupsAll(context.Background(), s); err != nil || len(list) != 0 {
		t.Fatalf("%v %v", list, err)
	}
}
