package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"cubeherd/internal/server"
)

// TestHostLeafRaceClosed repeats, against hostLeaf/openRegularAt, the race the pre-fix hostPath/openRegular
// had: a symlink planted in /data between the containment check and the later open (the server's own
// container owns /data and can run concurrently). It must never read the file the symlink points to.
func TestHostLeafRaceClosed(t *testing.T) {
	tmp := t.TempDir()
	st, err := server.NewStore(filepath.Join(tmp, "cube"))
	if err != nil {
		t.Fatal(err)
	}
	s := &server.Server{ID: "victim"}
	data := st.DataDir(s)
	os.MkdirAll(data, 0o755)
	secret := filepath.Join(tmp, "outside-secret.txt")
	os.WriteFile(secret, []byte("TOP-SECRET-HOST-FILE"), 0o600)
	os.WriteFile(filepath.Join(data, "x.txt"), []byte("harmless"), 0o644)
	m := &Manager{Store: st}

	var stop atomic.Bool
	go func() {
		link := filepath.Join(data, "x.txt")
		for !stop.Load() {
			os.Remove(link)
			os.Symlink(secret, link)
			os.Remove(link)
			os.WriteFile(link, []byte("harmless"), 0o644)
		}
	}()
	hits := 0
	const N = 20000
	for i := 0; i < N; i++ {
		dir, leaf, err := m.hostLeaf(s, "x.txt")
		if err != nil {
			continue
		}
		f, err := openRegularAt(dir, leaf)
		dir.Close()
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) == "TOP-SECRET-HOST-FILE" {
			hits++
		}
	}
	stop.Store(true)
	fmt.Printf("read the file outside /data in %d of %d attempts\n", hits, N)
	if hits != 0 {
		t.Fatalf("the race is still open: escaped /data %d times", hits)
	}
}

// TestWriteFileAncestorRaceClosed races a symlink swapped in for a parent directory against WriteFile. The
// write must either land inside /data or fail outright; it must never land in the directory the symlink
// pointed at outside /data.
func TestWriteFileAncestorRaceClosed(t *testing.T) {
	tmp := t.TempDir()
	st, err := server.NewStore(filepath.Join(tmp, "cube"))
	if err != nil {
		t.Fatal(err)
	}
	s := &server.Server{ID: "victim"}
	data := st.DataDir(s)
	sub := filepath.Join(data, "sub")
	os.MkdirAll(sub, 0o755)
	outside := filepath.Join(tmp, "outside")
	os.MkdirAll(outside, 0o755)
	m := &Manager{Store: st}
	ctx := context.Background()

	var stop atomic.Bool
	go func() {
		for !stop.Load() {
			os.RemoveAll(sub)
			os.Symlink(outside, sub)
			os.Remove(sub)
			os.MkdirAll(sub, 0o755)
		}
	}()
	const N = 5000
	for i := 0; i < N; i++ {
		_ = m.WriteFile(ctx, s, "sub/file.txt", strings.NewReader("payload"))
	}
	stop.Store(true)

	escaped, _ := filepath.Glob(filepath.Join(outside, "file.txt*"))
	if len(escaped) != 0 {
		t.Fatalf("the write escaped /data into %v", escaped)
	}
}
