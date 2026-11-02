package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/server"
)

// The methods below expose what the web layer used to reach through Manager.Store, Manager.Docker
// and the host reader, so that everything the dashboard needs from a host goes through node.Node.

// Server loads one server definition.
func (m *Manager) Server(id string) (*server.Server, error) { return m.Store.Get(id) }

// ServerExists reports whether a definition with this id exists.
func (m *Manager) ServerExists(id string) bool { return m.Store.Exists(id) }

// ServerDir is the server's folder on the host.
func (m *Manager) ServerDir(id string) string { return m.Store.Dir(id) }

// DataDir is the host path mounted at /data ("" when the data lives in a Docker volume).
func (m *Manager) DataDir(s *server.Server) string { return m.Store.DataDir(s) }

// Inspect returns the state of the server's container.
func (m *Manager) Inspect(ctx context.Context, s *server.Server) (*docker.ContainerJSON, error) {
	return m.Docker.Inspect(ctx, s.Container())
}

// StatsStream calls fn for every stats sample of the server's running container until ctx ends.
func (m *Manager) StatsStream(ctx context.Context, s *server.Server, fn func(docker.Stats)) error {
	return m.Docker.StatsStream(ctx, s.Container(), fn)
}

// LogsSince streams the server's log lines (see docker.Client.LogsSince); the terminal mode is looked up here.
func (m *Manager) LogsSince(ctx context.Context, s *server.Server, follow bool, tail int, since int64, fn func(line string)) error {
	c, err := m.Docker.Inspect(ctx, s.Container())
	if err != nil {
		return err
	}
	return m.Docker.LogsSince(ctx, s.Container(), follow, tail, since, c.Config.Tty, fn)
}

// FollowOps passes the output of Cubeherd actions (compose pull/up, stop) of one server to fn until ctx ends.
// Output of an action that ended before the call is skipped.
func (m *Manager) FollowOps(ctx context.Context, id string, fn func(line string)) {
	var cur *Op
	idx := 0
	for ctx.Err() == nil {
		o := m.Op(id)
		if o == nil {
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		if o != cur {
			cur, idx = o, 0
			if lines, _, done := o.Since(0); done {
				idx = len(lines)
			}
		}
		lines, changed, done := o.Since(idx)
		idx += len(lines)
		for _, l := range lines {
			if ctx.Err() != nil {
				return
			}
			fn(l)
		}
		if done {
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// DockerInfo returns the engine's /info.
func (m *Manager) DockerInfo(ctx context.Context) (docker.Info, error) { return m.Docker.Info(ctx) }

// DockerHost is the address the Docker engine is reached at.
func (m *Manager) DockerHost() string { return m.Docker.Host }

// HostStats reports the host's CPU, memory and disk usage.
func (m *Manager) HostStats(ctx context.Context) host.Stats { return m.Hoster.Read(ctx) }

// StageWorld streams an uploaded world ZIP to disk next to the server (not into /data),
// without buffering it in memory, and returns the path to hand to ImportWorld.
func (m *Manager) StageWorld(s *server.Server, r io.Reader) (string, error) {
	tmp := filepath.Join(m.Store.Dir(s.ID), ".world-upload.zip")
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("%w", err)
	}
	return tmp, nil
}

// OpenBackup opens an archive for download: the local file when there is one (an *os.File, so callers
// can serve ranges), otherwise the copy in the external storage. size is -1 when unknown.
func (m *Manager) OpenBackup(ctx context.Context, s *server.Server, name string) (body io.ReadCloser, size int64, mod time.Time, err error) {
	p, perr := m.BackupPath(s, name)
	if perr != nil {
		body, size, err = m.OpenRemote(ctx, s, name)
		return body, size, time.Time{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, time.Time{}, err
	}
	return f, st.Size(), st.ModTime(), nil
}

// ApplyConfig takes over the settings an agent needs from the dashboard and wakes what depends on them.
func (m *Manager) ApplyConfig(c api.Config) error {
	if err := m.Config.Update(func(s *config.Settings) {
		s.Timezone, s.Language, s.CFAPIKey, s.Remote = c.Timezone, c.Language, c.CFAPIKey, c.Remote
		if c.PortMin >= 1024 && c.PortMax >= c.PortMin && c.PortMax <= 65535 {
			s.PortMin, s.PortMax = c.PortMin, c.PortMax
		}
	}); err != nil {
		return err
	}
	m.InvalidateRemote()
	m.WakeScheduler()
	return nil
}

func (m *Manager) paths(id string, s *server.Server) api.Paths {
	return api.Paths{Dir: m.Store.Dir(id), DataDir: m.Store.DataDir(s)}
}

// Paths reports where the server lives on this host.
func (m *Manager) Paths(s *server.Server) api.Paths { return m.paths(s.ID, s) }
