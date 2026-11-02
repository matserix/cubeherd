// Package node defines what the dashboard needs from one host that runs Minecraft servers.
//
// Today the only implementation is the in-process *manager.Manager. A remote implementation
// that talks to an agent can satisfy the same interface, so the web layer never touches the
// host's Docker socket or the server folders directly.
package node

import (
	"context"
	"io"
	"time"

	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
)

// Node is one host: its servers, their data and the resources they run on.
type Node interface {
	// Servers and status.
	Server(id string) (*server.Server, error)
	ServerExists(id string) bool
	View(ctx context.Context, s *server.Server, withStats bool) manager.View
	Views(ctx context.Context, withStats bool) ([]manager.View, error)
	Op(id string) *manager.Op
	Busy(id string) bool
	Forget(id string)
	DiskUsage(s *server.Server) int64
	NextPort(excludeID string) int
	PortTaken(port int, excludeID string) bool

	// Lifecycle.
	Create(s *server.Server) error
	Start(id string) error
	Stop(id string) error
	Restart(id string) error
	Apply(s *server.Server, pull, restart bool) error
	ApplyUpdate(s *server.Server, pull, restart, backupFirst bool) error
	Uninstall(id, mode string) error
	Discover(ctx context.Context) []manager.Unmanaged
	Import(ctx context.Context, containerID, name string) (*server.Server, error)

	// Console, players, performance.
	RCON(ctx context.Context, s *server.Server, cmd string) (string, error)
	Players(ctx context.Context, s *server.Server) ([]string, int, error)
	Perf(ctx context.Context, s *server.Server) (server.PerfSample, error)

	// Live container data.
	Inspect(ctx context.Context, container string) (*docker.ContainerJSON, error)
	StatsStream(ctx context.Context, container string, fn func(docker.Stats)) error
	LogsSince(ctx context.Context, container string, follow bool, tail int, since int64, tty bool, fn func(line string)) error

	// Files, data and worlds.
	DataDir(s *server.Server) string
	ServerDir(id string) string
	ArchiveDir() string
	List(ctx context.Context, s *server.Server, rel string) ([]manager.Entry, bool, error)
	Stat(ctx context.Context, s *server.Server, rel string) (manager.Entry, error)
	Open(ctx context.Context, s *server.Server, rel string, w io.Writer) error
	ReadText(ctx context.Context, s *server.Server, rel string) (string, bool, error)
	ReadDataFile(s *server.Server, name string) ([]byte, error)
	WriteFile(ctx context.Context, s *server.Server, rel string, r io.Reader) error
	Remove(ctx context.Context, s *server.Server, rel string) error
	WorldZip(ctx context.Context, s *server.Server, w io.Writer) error
	StageWorld(s *server.Server, r io.Reader) (string, error)
	ImportWorld(s *server.Server, zipPath string) error

	// Mods and plugins.
	AddonFiles(ctx context.Context, s *server.Server) ([]manager.AddonFile, error)
	AddonsPending(ctx context.Context, s *server.Server) bool
	UploadAddon(ctx context.Context, s *server.Server, name string, r io.Reader) error
	DeleteUpload(ctx context.Context, s *server.Server, name string) error

	// Backups.
	BackupsAll(ctx context.Context, s *server.Server) ([]manager.Backup, error)
	BackupNow(id, reason string) error
	BackupPath(s *server.Server, name string) (string, error)
	BackupHealth(s *server.Server, running bool) manager.Health
	DeleteBackupEverywhere(ctx context.Context, s *server.Server, name string) error
	VerifyBackup(id, name string) error
	UploadBackup(id, name string) error
	Restore(id, name string) error
	OpenRemote(ctx context.Context, s *server.Server, name string) (io.ReadCloser, int64, error)
	HasRemote() bool
	InvalidateRemote()
	NextBackupFor(s *server.Server) time.Time
	WakeScheduler()

	// The host itself.
	HostStats(ctx context.Context) host.Stats
	DockerInfo(ctx context.Context) (docker.Info, error)
	DockerHost() string
	Loc() *time.Location
}

var _ Node = (*manager.Manager)(nil)
