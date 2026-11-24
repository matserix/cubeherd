package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"cubeherd/internal/docker"
	"cubeherd/internal/server"
)

// Every archive starts with a small file that describes the server software at the time of the backup
// (server.Snapshot). Because listing must not read archives, the same facts are kept in
// servers/<id>/backup-meta.json, keyed by archive name. The archive is the source of truth.

// snapshotFile is the member name inside an archive. It is never extracted into /data.
const snapshotFile = ".cubeherd-backup.json"

// snapshotLimit bounds what is read back from an archive; a real snapshot is well below 1 kB.
const snapshotLimit = 64 << 10

// archiveScript packs /data with the snapshot (from $SNAPSHOT) as first member. The arguments are the tar
// exclude options. If the snapshot cannot be written, the data is still backed up.
const archiveScript = `d=$(mktemp -d) && printf '%s' "$SNAPSHOT" >"$d/` + snapshotFile + `" && set -- "$@" -C "$d" ./` + snapshotFile + `
ionice -c3 tar -I 'zstd -3 -T1' -cf - "$@" -C /data .
rc=$?
if [ -n "$d" ]; then rm -rf "$d"; fi
exit $rc`

// installedSnapshot describes the software whose files are in /data. It returns nil when the definition cannot be
// trusted to name it: extra variables decide the version, the container was made for another one (a failed
// update), or the Modrinth pack on disk is a different version.
func (m *Manager) installedSnapshot(ctx context.Context, s *server.Server, c *docker.ContainerJSON) *server.Snapshot {
	if s.VersionOverridden() || (c != nil && !s.RunsAs(c.Config.Env, m.globals())) {
		return nil
	}
	if s.Kind == server.KindModrinth && s.Pack != nil && s.Pack.Version != "" {
		if text, ok, err := m.ReadText(ctx, s, ".modrinth-modpack-manifest.json"); err == nil && ok {
			var installed struct {
				VersionID string `json:"versionId"`
			}
			if json.Unmarshal([]byte(text), &installed) == nil && installed.VersionID != "" && installed.VersionID != s.Pack.Version {
				return nil
			}
		}
	}
	return s.Snapshot()
}

// snapshotScript prints the snapshot of an archive and stops reading right after it.
const snapshotScript = `tar -I zstd --occurrence=1 -xOf "/b/$F" ./` + snapshotFile

func (m *Manager) metaPath(id string) string {
	return filepath.Join(m.Store.Dir(id), "backup-meta.json")
}

func (m *Manager) loadMeta(id string) map[string]*server.Snapshot {
	out := map[string]*server.Snapshot{}
	if b, err := os.ReadFile(m.metaPath(id)); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (m *Manager) updateMeta(id string, f func(map[string]*server.Snapshot)) {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	meta := m.loadMeta(id)
	f(meta)
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(m.metaPath(id)), 0o755)
		_ = os.WriteFile(m.metaPath(id), b, 0o644)
	}
}

func (m *Manager) recordSnapshot(id, name string, sn *server.Snapshot) {
	m.updateMeta(id, func(meta map[string]*server.Snapshot) { meta[name] = sn })
}

// forgetSnapshot drops the index entry once no copy of the archive is left.
func (m *Manager) forgetSnapshot(s *server.Server, name string) {
	if _, err := m.BackupPath(s, name); err == nil || m.hasRemoteBackup(s, name) {
		return
	}
	if _, ok := m.loadMeta(s.ID)[name]; ok {
		m.updateMeta(s.ID, func(meta map[string]*server.Snapshot) { delete(meta, name) })
	}
}

func parseSnapshot(b []byte) (*server.Snapshot, error) {
	var sn server.Snapshot
	if err := json.Unmarshal(b, &sn); err != nil {
		return nil, err
	}
	if sn.Label() == "" {
		return nil, errors.New("empty snapshot")
	}
	return &sn, nil
}

// readSnapshot reads the snapshot out of a local archive. Archives from before the feature have none.
func (m *Manager) readSnapshot(ctx context.Context, s *server.Server, dir, name string) (*server.Snapshot, error) {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-e", "F="+name, "-v", dir+":/b:ro",
		"--entrypoint", "sh", s.ImageRef(), "-c", snapshotScript)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	_, readErr := io.Copy(&buf, io.LimitReader(out, snapshotLimit))
	_, _ = io.Copy(io.Discard, out)
	if err := errors.Join(readErr, cmd.Wait()); err != nil {
		return nil, err
	}
	return parseSnapshot(buf.Bytes())
}
