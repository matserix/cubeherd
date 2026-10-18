package manager

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"cubeherd/internal/server"
)

// ReasonUpload marks the backup taken before a world upload.
const ReasonUpload = "upload"

func init() { reasonLabel[ReasonUpload] = "Vor Welt-Upload" }

// LevelName reads level-name from server.properties (default "world").
func (m *Manager) LevelName(ctx context.Context, s *server.Server) string {
	if txt, ok, err := m.ReadText(ctx, s, "server.properties"); err == nil && ok {
		for _, l := range strings.Split(txt, "\n") {
			if v, found := strings.CutPrefix(strings.TrimSpace(l), "level-name="); found {
				if n, err := CleanRel(strings.TrimSpace(v)); err == nil && n != "" && !strings.Contains(n, "/") {
					return n
				}
			}
		}
	}
	return "world"
}

// WorldDirs returns the world folders that exist (Bukkit-style servers keep nether and end separately).
func (m *Manager) WorldDirs(ctx context.Context, s *server.Server) []string {
	name := m.LevelName(ctx, s)
	var out []string
	for _, d := range []string{name, name + "_nether", name + "_the_end"} {
		if st, err := m.Stat(ctx, s, d); err == nil && st.Dir {
			out = append(out, d)
		}
	}
	return out
}

// WorldZip streams the world folders as a ZIP. A running server flushes and pauses saving meanwhile.
func (m *Manager) WorldZip(ctx context.Context, s *server.Server, w io.Writer) error {
	dirs := m.WorldDirs(ctx, s)
	if len(dirs) == 0 {
		return errors.New(m.t("Keine Welt gefunden"))
	}
	if v := m.View(ctx, s, false); v.Status.Live() {
		if _, err := m.RCON(ctx, s, "save-off"); err == nil {
			defer m.RCON(context.Background(), s, "save-on")
			_, _ = m.RCON(ctx, s, "save-all flush")
		}
	}
	pr, pw := io.Pipe()
	go func() {
		script := "cd /data && exec nice -n 19 tar -cf - -- " + strings.Join(quoteAll(dirs), " ")
		pw.CloseWithError(m.dataShell(ctx, s, script, nil, nil, pw, true))
	}()
	return tarToZip(pr, w)
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return out
}

// tarToZip converts a tar stream into a ZIP. Region files are already compressed and stored as-is.
func tarToZip(r io.Reader, w io.Writer) error {
	tr := tar.NewReader(r)
	zw := zip.NewWriter(w)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean(h.Name), "./")
		switch h.Typeflag {
		case tar.TypeDir:
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Modified: h.ModTime}); err != nil {
				return err
			}
		case tar.TypeReg:
			method := zip.Deflate
			if strings.HasSuffix(name, ".mca") || strings.HasSuffix(name, ".dat") || strings.HasSuffix(name, ".gz") {
				method = zip.Store
			}
			fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: h.ModTime})
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, tr); err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// ErrNoWorld is returned for ZIPs without level.dat (translated where it is shown).
var ErrNoWorld = errors.New("Die ZIP-Datei enthält keine level.dat und ist deshalb keine Minecraft-Welt")

// WorldRoot finds the folder inside a ZIP that contains level.dat (e.g. "My World/" or "").
func WorldRoot(zr *zip.Reader) (string, error) {
	best := ""
	found := false
	for _, f := range zr.File {
		n := strings.ReplaceAll(f.Name, "\\", "/")
		if path.Base(n) != "level.dat" {
			continue
		}
		dir := path.Dir(n)
		if dir == "." {
			dir = ""
		}
		if !found || len(dir) < len(best) {
			best, found = dir, true
		}
	}
	if !found {
		return "", ErrNoWorld
	}
	if best != "" {
		best += "/"
	}
	return best, nil
}

// zipToTar writes the entries below root as a tar stream under target/.
func zipToTar(zr *zip.Reader, root, target string, w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, f := range zr.File {
		n := strings.ReplaceAll(f.Name, "\\", "/")
		if !strings.HasPrefix(n, root) {
			continue
		}
		rel, err := CleanRel(strings.TrimPrefix(n, root))
		if err != nil || rel == "" || strings.HasPrefix(rel, "__MACOSX") || path.Base(rel) == ".DS_Store" {
			continue
		}
		name := target + "/" + rel
		if f.FileInfo().IsDir() {
			if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: f.Modified, Uid: 1000, Gid: 1000}); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() && f.Mode()&os.ModeType != 0 {
			continue // no symlinks or devices from uploads
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(f.UncompressedSize64), ModTime: f.Modified, Uid: 1000, Gid: 1000})
		if err == nil {
			_, err = io.Copy(tw, rc)
		}
		rc.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// ImportWorld replaces the world with the ZIP at zipPath (removed afterwards). The current state is backed up first.
func (m *Manager) ImportWorld(s *server.Server, zipPath string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		os.Remove(zipPath)
		return errors.New(m.t("Die Datei ist kein gültiges ZIP-Archiv"))
	}
	root, err := WorldRoot(&zr.Reader)
	if err != nil {
		zr.Close()
		os.Remove(zipPath)
		return err
	}
	return m.run(s.ID, server.StatusImporting, func(o *Op) error {
		defer os.Remove(zipPath)
		defer zr.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		c := m.inspect(ctx, s)
		wasUp := c != nil && c.State.Running
		if wasUp {
			o.add("[cubeherd] " + m.t("Server wird gestoppt"))
			if err := m.Docker.Stop(ctx, s.Container(), time.Minute); err != nil {
				return err
			}
		}
		if _, err := m.backup(ctx, s, o, ReasonUpload, m.BackupDir(s), ""); err != nil {
			return fmt.Errorf(m.t("Sicherung der bisherigen Welt fehlgeschlagen, nichts geändert: %w"), err)
		}
		name := m.LevelName(ctx, s)
		o.add("[cubeherd] " + m.t("Ersetze Welt „%s“", name))
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(zipToTar(&zr.Reader, root, name, pw)) }()
		script := `set -e; cd /data; rm -rf -- "$N" "${N}_nether" "${N}_the_end"; tar -xf - --no-same-permissions; chown -R 1000:1000 "$N"`
		var out bytes.Buffer
		if err := m.dataShell(ctx, s, script, map[string]string{"N": name}, pr, &out, false); err != nil {
			return fmt.Errorf(m.t("Entpacken fehlgeschlagen: %w"), err)
		}
		o.add("[cubeherd] " + m.t("Welt eingespielt"))
		if wasUp {
			if err := m.Store.Save(s, m.globals()); err != nil {
				return err
			}
			return m.compose(ctx, s, o, "up", "-d")
		}
		return nil
	})
}
