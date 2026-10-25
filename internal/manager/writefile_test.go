package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cubeherd/internal/server"
)

func TestWriteDataFileKeepsModeAndOwner(t *testing.T) {
	dir := t.TempDir()
	st, err := server.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Store: st}
	s := &server.Server{ID: "t", DataDir: dir}
	p := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(p, []byte("a=1\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o664); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		// Cubeherd in a container runs as root, the Minecraft server as another user.
		if err := os.Chown(p, 1000, 1000); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, 1000, 1000); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.Stat(p)
	uid, gid := ownerOf(before)

	if err := m.writeDataFile(context.Background(), s, "server.properties", strings.NewReader("a=2\n")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o664 {
		t.Errorf("mode = %v, want 0664 (the old mode)", after.Mode().Perm())
	}
	if u, g := ownerOf(after); u != uid || g != gid {
		t.Errorf("owner = %d:%d, want %d:%d", u, g, uid, gid)
	}
	if b, _ := os.ReadFile(p); string(b) != "a=2\n" {
		t.Errorf("content = %q", b)
	}

	// New files in new folders get the owner of their parent.
	if err := m.writeDataFile(context.Background(), s, "mods/sub/x.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"mods", "mods/sub", "mods/sub/x.txt"} {
		fi, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if u, g := ownerOf(fi); u != uid || g != gid {
			t.Errorf("%s owner = %d:%d, want %d:%d", rel, u, g, uid, gid)
		}
	}
}
