package agent_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cubeherd/internal/agent"
	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/docker"
	"cubeherd/internal/manager"
	"cubeherd/internal/remote"
	"cubeherd/internal/server"
)

// realNode runs an agent on top of a real manager with a temporary data directory and no Docker daemon.
// Everything that only touches the server folder (files, uploads, archives, settings) works that way.
func realNode(t *testing.T) (*remote.Client, *manager.Manager, *server.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := server.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := docker.New()
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	m := manager.New(dc, store, cfg, nil)
	s := &server.Server{ID: "survival", Name: "Survival", Kind: server.KindSoftware, Type: "PAPER", Version: "1.21.4", JavaTag: "java21", Port: 25565, MemoryGB: 4}
	if err := store.Save(s, server.Globals{}); err != nil {
		t.Fatal(err)
	}

	cert, err := agent.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := agent.New(m, agent.NewToken("tok"), "test", dir).Server("", cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	t.Cleanup(func() { srv.Close() })
	c, err := remote.New(ln.Addr().String(), "tok", api.Fingerprint(cert.Certificate[0]))
	if err != nil {
		t.Fatal(err)
	}
	return c, m, s
}

func TestFilesOverTheWire(t *testing.T) {
	c, m, s := realNode(t)
	ctx := context.Background()
	data := filepath.Join(m.ServerDir(s.ID), "data")

	if err := c.WriteFile(ctx, s, "server.properties", strings.NewReader("motd=hi\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFile(ctx, s, "plugins/readme.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "server.properties")); string(b) != "motd=hi\n" {
		t.Fatalf("file on the agent = %q", b)
	}
	entries, truncated, err := c.List(ctx, s, "")
	if err != nil || truncated || len(entries) != 2 || !entries[0].Dir || entries[0].Name != "plugins" {
		t.Fatalf("List = %+v %v %v", entries, truncated, err)
	}
	st, err := c.Stat(ctx, s, "server.properties")
	if err != nil || st.Dir || st.Size != 8 {
		t.Fatalf("Stat = %+v %v", st, err)
	}
	txt, isText, err := c.ReadText(ctx, s, "server.properties")
	if err != nil || !isText || txt != "motd=hi\n" {
		t.Fatalf("ReadText = %q %v %v", txt, isText, err)
	}
	var buf bytes.Buffer
	if err := c.Open(ctx, s, "server.properties", &buf); err != nil || buf.String() != "motd=hi\n" {
		t.Fatalf("Open = %q %v", buf.String(), err)
	}
	if b, err := c.ReadDataFile(s, "server.properties"); err != nil || string(b) != "motd=hi\n" {
		t.Fatalf("ReadDataFile = %q %v", b, err)
	}
	if err := c.Remove(ctx, s, "plugins/readme.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, s, "plugins/readme.txt"); err == nil {
		t.Fatal("file still there after Remove")
	}
	// A missing file keeps its meaning across the wire.
	if _, err := c.Stat(ctx, s, "nope.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file = %v", err)
	}
	// Paths that climb out of /data are refused by the agent, whatever the dashboard sends.
	if err := c.WriteFile(ctx, s, "../../escape.txt", strings.NewReader("x")); err != nil {
		t.Logf("write outside: %v", err) // cleaned to a path below /data, not an escape
	}
	if _, err := os.Stat(filepath.Join(m.ServerDir(s.ID), "escape.txt")); err == nil {
		t.Fatal("a path with .. escaped /data")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.ServerDir(s.ID)), "escape.txt")); err == nil {
		t.Fatal("a path with .. escaped the server folder")
	}
	p := c.Paths(s)
	if p.Dir != m.ServerDir(s.ID) || p.DataDir != data {
		t.Fatalf("Paths = %+v", p)
	}
	if got := c.Paths(&server.Server{ID: "unsaved"}); got.Dir != m.ServerDir("unsaved") {
		t.Fatalf("Paths of an unsaved server = %+v", got)
	}
}

func TestLargeUploadAndDownloadStream(t *testing.T) {
	c, _, s := realNode(t)
	ctx := context.Background()
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB
	if err := c.WriteFile(ctx, s, "world/big.bin", bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.Open(ctx, s, "world/big.bin", &out); err != nil || !bytes.Equal(out.Bytes(), big) {
		t.Fatalf("round trip: %v, %d bytes", err, out.Len())
	}
}

func TestAddonsUpdateTheCallersDefinition(t *testing.T) {
	c, m, s := realNode(t)
	ctx := context.Background()
	if err := c.UploadAddon(ctx, s, "cool.jar", strings.NewReader("jar")); err != nil {
		t.Fatal(err)
	}
	if len(s.Uploads) != 1 || s.Uploads[0] != "cool.jar" {
		t.Fatalf("caller's definition not updated: %v", s.Uploads)
	}
	if saved, _ := m.Server(s.ID); len(saved.Uploads) != 1 {
		t.Fatalf("agent did not save: %v", saved.Uploads)
	}
	files, err := c.AddonFiles(ctx, s)
	if err != nil || len(files) != 1 || files[0].Name != "cool.jar" || !files[0].Uploaded {
		t.Fatalf("AddonFiles = %+v %v", files, err)
	}
	if err := c.UploadAddon(ctx, s, "../evil.jar", strings.NewReader("x")); err == nil {
		t.Fatal("invalid jar name accepted")
	}
	if err := c.DeleteUpload(ctx, s, "cool.jar"); err != nil || len(s.Uploads) != 0 {
		t.Fatalf("DeleteUpload: %v %v", err, s.Uploads)
	}
}

func TestWorldImportNeedsAStagedFile(t *testing.T) {
	c, m, s := realNode(t)
	if err := c.ImportWorld(s, "/etc/passwd"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import without staged file = %v", err)
	}
	// A staged ZIP without level.dat is rejected with the manager's own error.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("no world here"))
	zw.Close()
	path, err := c.StageWorld(s, &zb)
	if err != nil || path != filepath.Join(m.ServerDir(s.ID), ".world-upload.zip") {
		t.Fatalf("StageWorld = %q %v", path, err)
	}
	if err := c.ImportWorld(s, "ignored"); !errors.Is(err, manager.ErrNoWorld) {
		t.Fatalf("import of a ZIP without level.dat = %v", err)
	}
}

func TestBackupsOverTheWire(t *testing.T) {
	c, m, s := realNode(t)
	ctx := context.Background()
	dir := m.BackupDir(s)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "20260101-040000_manual.tar.zst"
	body := bytes.Repeat([]byte("archive"), 100000)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := c.BackupsAll(ctx, s)
	if err != nil || len(list) != 1 || list[0].Name != name || !list[0].Local || list[0].Reason != manager.ReasonManual || list[0].Label() == "" {
		t.Fatalf("BackupsAll = %+v %v", list, err)
	}
	rc, size, mod, err := c.OpenBackup(ctx, s, name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) || size != int64(len(body)) || mod.IsZero() {
		t.Fatalf("OpenBackup: %v, %d bytes, size %d, mod %v", err, len(got), size, mod)
	}
	if _, _, _, err := c.OpenBackup(ctx, s, "20260101-050000_manual.tar.zst"); err == nil {
		t.Fatal("missing archive opened")
	}
	if h := c.BackupHealth(s, false); h.Level == "" {
		t.Fatalf("BackupHealth = %+v", h)
	}
	if c.HasRemote() {
		t.Fatal("no external storage is configured")
	}
	if err := c.DeleteBackupEverywhere(ctx, s, name); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.BackupsAll(ctx, s); len(list) != 0 {
		t.Fatalf("archive still listed: %+v", list)
	}
}

func TestPushConfig(t *testing.T) {
	c, m, _ := realNode(t)
	if err := c.PushConfig(context.Background(), api.Config{Timezone: "Europe/Berlin", Language: "en", CFAPIKey: "cf-key"}); err != nil {
		t.Fatal(err)
	}
	got := m.Config.Get()
	if got.Timezone != "Europe/Berlin" || got.Language != "en" || got.CFAPIKey != "cf-key" {
		t.Fatalf("agent settings = %+v", got)
	}
	if got.PasswordHash != "" {
		t.Fatal("a password hash appeared on the agent")
	}
	if m.Loc().String() != "Europe/Berlin" {
		t.Fatalf("Loc = %v", m.Loc())
	}
}
