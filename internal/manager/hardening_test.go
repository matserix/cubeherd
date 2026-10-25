package manager

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"cubeherd/internal/server"
)

func hostServer(t *testing.T) (*Manager, *server.Server, string) {
	t.Helper()
	st, err := server.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &Manager{Store: st}, &server.Server{ID: "t", Type: "PAPER", DataDir: dir}, dir
}

// A container can plant "<name>.part" as a symlink; the save must not write through it.
func TestWriteDataFileIgnoresPlantedPartSymlink(t *testing.T) {
	m, s, dir := hostServer(t)
	outside := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "a.txt.part")); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile(context.Background(), s, "a.txt", strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Errorf("file outside the data folder was written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "new" {
		t.Errorf("a.txt = %q", b)
	}
}

// A mods/plugins folder replaced by a symlink must not redirect uploads, deletes or listings.
func TestAddonPathsRefuseSymlinkedFolder(t *testing.T) {
	m, s, dir := hostServer(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.jar"), []byte("j"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := m.UploadAddon(context.Background(), s, "y.jar", strings.NewReader("data")); err == nil {
		t.Error("upload through a symlinked plugins folder was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "y.jar")); err == nil {
		t.Error("jar landed outside the data folder")
	}
	s.Uploads = []string{"x.jar"}
	if err := m.DeleteUpload(context.Background(), s, "x.jar"); err == nil {
		t.Error("delete through a symlinked plugins folder was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.jar")); err != nil {
		t.Error("jar outside the data folder was removed")
	}
	if _, err := m.AddonFiles(context.Background(), s); err == nil {
		t.Error("listing followed the symlinked plugins folder")
	}
}

func TestReadTextRefusesOversizeFifoAndEscapingFiles(t *testing.T) {
	m, s, dir := hostServer(t)
	ctx := context.Background()
	big := bytes.Repeat([]byte("a"), MaxEditSize+10)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.ReadText(ctx, s, "big.txt"); ok || err != nil {
		t.Errorf("big file: ok=%v err=%v, want not ok", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if txt, ok, err := m.ReadText(ctx, s, "ok.txt"); !ok || err != nil || txt != "hi" {
		t.Errorf("small file: %q %v %v", txt, ok, err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	if _, ok, err := m.ReadText(ctx, s, "pipe"); ok || err == nil {
		t.Errorf("fifo: ok=%v err=%v, want an error without blocking", ok, err)
	}
	// ReadDataFile: no FIFOs, no symlinks out of the folder, bounded size.
	if _, err := m.ReadDataFile(s, "pipe"); err == nil {
		t.Error("ReadDataFile read a FIFO")
	}
	secret := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(secret, []byte(`[{"name":"x"}]`), 0o600)
	os.Symlink(secret, filepath.Join(dir, "banned-players.json"))
	if _, err := m.ReadDataFile(s, "banned-players.json"); err == nil {
		t.Error("ReadDataFile followed a symlink out of the data folder")
	}
	os.WriteFile(filepath.Join(dir, "whitelist.json"), big, 0o644)
	if _, err := m.ReadDataFile(s, "whitelist.json"); err == nil {
		t.Error("ReadDataFile read an oversized file")
	}
}
