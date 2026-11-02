package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

// Inspect returns the container's current state.
func (m *Manager) Inspect(ctx context.Context, container string) (*docker.ContainerJSON, error) {
	return m.Docker.Inspect(ctx, container)
}

// StatsStream calls fn for every stats sample of a running container until ctx ends.
func (m *Manager) StatsStream(ctx context.Context, container string, fn func(docker.Stats)) error {
	return m.Docker.StatsStream(ctx, container, fn)
}

// LogsSince streams log lines of a container (see docker.Client.LogsSince).
func (m *Manager) LogsSince(ctx context.Context, container string, follow bool, tail int, since int64, tty bool, fn func(line string)) error {
	return m.Docker.LogsSince(ctx, container, follow, tail, since, tty, fn)
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
