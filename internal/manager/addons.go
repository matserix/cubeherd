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

	"forgecraft/internal/server"
)

// AddonFile is a JAR in the server's mods/plugins folder.
type AddonFile struct {
	Name     string
	Size     int64
	Uploaded bool // uploaded through Forgecraft (removable here); other files belong to itzg or the modpack
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
		entries, err := os.ReadDir(filepath.Join(dir, s.AddonDir()))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
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
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := p + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
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

func (m *Manager) removeDataFile(ctx context.Context, s *server.Server, rel string) error {
	if dir := m.Store.DataDir(s); dir != "" {
		err := os.Remove(filepath.Join(dir, rel))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-e", "P=/data/"+rel, "-v", s.DataVol+":/data",
		"--entrypoint", "sh", s.ImageRef(), "-c", `rm -f "$P"`)
	return cmd.Run()
}

// UploadAddon stores a JAR in the mods/plugins folder and remembers it as uploaded.
func (m *Manager) UploadAddon(ctx context.Context, s *server.Server, name string, r io.Reader) error {
	if !ValidJarName(name) {
		return errors.New("Ungültiger Dateiname; erlaubt sind .jar-Dateien ohne Pfad")
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
	return m.Store.Save(s, m.cfKey())
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
		return errors.New("Nur hochgeladene Dateien lassen sich hier löschen")
	}
	if err := m.removeDataFile(ctx, s, s.AddonDir()+"/"+name); err != nil {
		return err
	}
	s.Uploads = rest
	return m.Store.Save(s, m.cfKey())
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
