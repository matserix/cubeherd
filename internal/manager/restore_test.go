package manager

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"cubeherd/internal/server"
)

// The definition is written before the compose file, so a failure there must not leave the new version behind.
func TestCommitRestoreRevertsDefinition(t *testing.T) {
	m, s := healthFixture(t, daily(), time.Now())
	s.Kind, s.Type, s.Version = server.KindSoftware, "PAPER", "1.21.4"
	if err := m.Store.Save(s, m.globals()); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(m.Store.Dir(s.ID), "compose.yml")
	if err := os.Remove(compose); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(compose, "blocked"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := &server.Snapshot{Kind: server.KindSoftware, Type: "PAPER", Version: "1.20.4"}
	if _, err := m.commitRestore(s.ID, s, old); err == nil {
		t.Fatal("no error although compose.yml cannot be written")
	}
	got, err := m.Store.Get(s.ID)
	if err != nil || got.Version != "1.21.4" {
		t.Fatalf("definition = %v, %v; want the version from before", got, err)
	}
}

// A line too long for the scanner must not make the caller go on while the command still runs.
func TestComposeWaitsForCommandAfterLongLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	m, s := healthFixture(t, daily(), time.Now())
	bin := t.TempDir()
	done := filepath.Join(bin, "done")
	script := "#!/bin/sh\nhead -c 200000 /dev/zero | tr '\\0' x\necho\nsleep 1\necho finished\ntouch '" + done + "'\nexit ${FAKE_EXIT:-0}\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, exit := range []string{"0", "3"} {
		os.Remove(done)
		t.Setenv("FAKE_EXIT", exit)
		err := m.compose(context.Background(), s, testOp(), "run")
		if _, serr := os.Stat(done); serr != nil {
			t.Fatalf("exit %s: returned before the command ended", exit)
		}
		if (err != nil) != (exit != "0") {
			t.Errorf("exit %s: err = %v", exit, err)
		}
	}
}
