package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cubeherd/internal/server"
)

// AddonFile is a JAR in the server's mods/plugins folder.
type AddonFile struct {
	Name     string
	Size     int64
	Uploaded bool // uploaded through Cubeherd (removable here); other files belong to itzg or the modpack
}

var jarName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._+()\[\]-]{0,120}\.jar$`)

// ValidJarName rejects paths and odd names for uploads.
func ValidJarName(n string) bool { return jarName.MatchString(n) && !strings.Contains(n, "..") }

// AddonFiles lists JARs in the mods/plugins folder.
func (m *Manager) AddonFiles(ctx context.Context, s *server.Server) ([]AddonFile, error) {
	uploaded := map[string]bool{}
	for _, u := range s.Uploads {
		uploaded[u] = true
	}
	var out []AddonFile
	if dir := m.Store.DataDir(s); dir != "" {
		var entries []os.DirEntry
		if p, err := m.hostPath(s, s.AddonDir()); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		} else if entries, err = os.ReadDir(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jar") {
				continue
			}
			f := AddonFile{Name: e.Name(), Uploaded: uploaded[e.Name()]}
			if info, err := e.Info(); err == nil {
				f.Size = info.Size()
			}
			out = append(out, f)
		}
	} else {
		// Data in a Docker volume: list through the container (running) only.
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res, code, err := m.Docker.Exec(lctx, s.Container(), "sh", "-c", "cd /data/"+s.AddonDir()+" 2>/dev/null && stat -c '%s %n' *.jar 2>/dev/null")
		if err != nil || code != 0 {
			res = ""
		}
		for _, l := range strings.Split(strings.TrimSpace(res), "\n") {
			size, name, ok := strings.Cut(l, " ")
			if !ok {
				continue
			}
			var n int64
			fmt.Sscan(size, &n)
			out = append(out, AddonFile{Name: name, Size: n, Uploaded: uploaded[name]})
		}
	}
	// Uploads stay visible even when the folder cannot be listed (volume data, server stopped).
	listed := map[string]bool{}
	for _, f := range out {
		listed[f.Name] = true
	}
	for _, u := range s.Uploads {
		if !listed[u] {
			out = append(out, AddonFile{Name: u, Uploaded: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// writeDataFile writes into /data/<rel> on the host folder or, for volumes, through a throwaway container.
func (m *Manager) writeDataFile(ctx context.Context, s *server.Server, rel string, r io.Reader) error {
	if dir := m.Store.DataDir(s); dir != "" {
		// Every host-folder write goes through hostPath: the container can plant symlinks anywhere in /data
		// (also a mods or plugins folder), and this process may be root.
		p, err := m.hostPath(s, rel)
		if err != nil {
			return err
		}
		if rootReal, err := filepath.EvalSymlinks(dir); err == nil {
			dir = rootReal
		}
		if err := mkdirAllLike(filepath.Dir(p), dir); err != nil {
			return err
		}
		// The server runs as another user (itzg: 1000) than Cubeherd, so the new file keeps the owner and mode
		// of the one it replaces. Otherwise itzg can no longer rewrite files such as server.properties on start.
		mode, uid, gid := os.FileMode(0o644), -1, -1
		if fi, err := os.Stat(p); err == nil {
			mode = fi.Mode().Perm()
			uid, gid = ownerOf(fi)
		} else if fi, err := os.Stat(filepath.Dir(p)); err == nil {
			uid, gid = ownerOf(fi)
		}
		// The temp name sits in a folder the container can write. Remove whatever is there (a planted symlink is
		// removed itself, not followed) and create the file exclusively, so the open can never follow a link.
		// Mode and owner are set on the open handle, not by path.
		tmp := p + ".part"
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Chmod(mode); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if uid >= 0 {
			_ = f.Chown(uid, gid) // fails without privileges when the owner differs; the file is then still ours
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, p)
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "-e", "P=/data/"+rel, "-v", s.DataVol+":/data",
		"--entrypoint", "sh", s.ImageRef(), "-c", `mkdir -p "$(dirname "$P")" && cat > "$P.part" && mv "$P.part" "$P" && chown 1000:1000 "$P"`)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = r, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s", firstLine(stderr.String(), err))
	}
	return nil
}

// mkdirAllLike creates missing folders below root with the owner of their parent, see writeDataFile.
func mkdirAllLike(dir, root string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if dir != root {
		if err := mkdirAllLike(filepath.Dir(dir), root); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if fi, err := os.Stat(filepath.Dir(dir)); err == nil {
		if uid, gid := ownerOf(fi); uid >= 0 {
			_ = os.Chown(dir, uid, gid)
		}
	}
	return nil
}

func (m *Manager) removeDataFile(ctx context.Context, s *server.Server, rel string) error {
	if dir := m.Store.DataDir(s); dir != "" {
		p, err := m.hostPath(s, rel)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-e", "P=/data/"+rel, "-v", s.DataVol+":/data",
		"--entrypoint", "sh", s.ImageRef(), "-c", `rm -f "$P"`)
	return cmd.Run()
}

// UploadAddon stores a JAR in the mods/plugins folder and remembers it as uploaded.
func (m *Manager) UploadAddon(ctx context.Context, s *server.Server, name string, r io.Reader) error {
	if !ValidJarName(name) {
		return errors.New(m.t("Ungültiger Dateiname; erlaubt sind .jar-Dateien ohne Pfad"))
	}
	if err := m.writeDataFile(ctx, s, s.AddonDir()+"/"+name, r); err != nil {
		return err
	}
	for _, u := range s.Uploads {
		if u == name {
			return nil
		}
	}
	s.Uploads = append(s.Uploads, name)
	sort.Strings(s.Uploads)
	return m.Store.Save(s, m.globals())
}

// DeleteUpload removes a previously uploaded JAR.
func (m *Manager) DeleteUpload(ctx context.Context, s *server.Server, name string) error {
	found := false
	var rest []string
	for _, u := range s.Uploads {
		if u == name {
			found = true
			continue
		}
		rest = append(rest, u)
	}
	if !found || !ValidJarName(name) {
		return errors.New(m.t("Nur hochgeladene Dateien lassen sich hier löschen"))
	}
	if err := m.removeDataFile(ctx, s, s.AddonDir()+"/"+name); err != nil {
		return err
	}
	s.Uploads = rest
	return m.Store.Save(s, m.globals())
}

// AddonsPending reports whether the running container has older mod/plugin declarations than the definition.
func (m *Manager) AddonsPending(ctx context.Context, s *server.Server) bool {
	c := m.inspect(ctx, s)
	if c == nil {
		return false
	}
	have := c.EnvMap()
	want := map[string]string{}
	for _, kv := range s.AddonEnv() {
		want[kv.Key] = kv.Value
	}
	for _, k := range []string{"MODRINTH_PROJECTS", "CURSEFORGE_FILES", "SPIGET_RESOURCES"} {
		hv, hok := have[k]
		wv, wok := want[k]
		if hok != wok || hv != wv {
			return true
		}
	}
	return false
}
