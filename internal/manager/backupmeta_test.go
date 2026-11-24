package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotIndex(t *testing.T) {
	m, s := healthFixture(t, daily(), time.Now())
	if err := os.MkdirAll(m.BackupDir(s), 0o755); err != nil {
		t.Fatal(err)
	}
	const name, old = "20260101-040000_manual.tar.zst", "20250101-040000_manual.tar.zst"
	for _, n := range []string{name, old} {
		if err := os.WriteFile(filepath.Join(m.BackupDir(s), n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.Type, s.Version = "PAPER", "1.21.4"
	sn := s.Snapshot()
	s.Version = "1.21.8" // a later change must not alter the snapshot
	m.recordSnapshot(s.ID, name, sn)

	list, err := m.Backups(s)
	if err != nil {
		t.Fatal(err)
	}
	m.annotate(s, list)
	got := map[string]string{}
	for _, b := range list {
		got[b.Name] = b.Snapshot.Label()
	}
	if got[name] != "Paper 1.21.4" || got[old] != "" {
		t.Fatalf("labels = %v", got)
	}

	m.forgetSnapshot(s, name) // the archive still exists
	if m.loadMeta(s.ID)[name] == nil {
		t.Fatal("entry of an existing archive was dropped")
	}
	if err := m.DeleteBackup(s, name); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.loadMeta(s.ID)[name]; ok {
		t.Fatal("entry of a deleted archive was kept")
	}
}

func TestParseSnapshot(t *testing.T) {
	if sn, err := parseSnapshot([]byte(`{"kind":"software","type":"FABRIC","version":"1.21.1"}`)); err != nil || sn.Label() != "Fabric 1.21.1" {
		t.Fatalf("sn=%v err=%v", sn, err)
	}
	for _, bad := range []string{"", "{}", "not json"} {
		if _, err := parseSnapshot([]byte(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
