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

	"cubeherd/internal/api"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
)

// Node is one host: everything the dashboard needs from it, split by feature so that an
// implementation can be built up (and tested) piece by piece.
type Node interface {
	Servers
	Console
	Live
	Files
	Addons
	Backups
	Host
	Metrics
}

// Servers covers definitions, status and lifecycle.
type Servers interface {
	Server(id string) (*server.Server, error)
	ServerExists(id string) bool
	View(ctx context.Context, s *server.Server, withStats bool) api.View
	Views(ctx context.Context, withStats bool) ([]api.View, error)
	Busy(id string) bool
	Forget(id string)
	DiskUsage(s *server.Server) int64
	NextPort(excludeID string) int
	PortTaken(port int, excludeID string) bool

	Create(s *server.Server) error
	Start(id string) error
	Stop(id string) error
	Restart(id string) error
	Apply(s *server.Server, pull, restart bool) error
	ApplyUpdate(s *server.Server, pull, restart, backupFirst bool) error
	Uninstall(id, mode string) error
	Discover(ctx context.Context) []api.Unmanaged
	Import(ctx context.Context, containerID, name string) (*server.Server, error)
}

// Console covers RCON, players and tick performance.
type Console interface {
	RCON(ctx context.Context, s *server.Server, cmd string) (string, error)
	Players(ctx context.Context, s *server.Server) ([]string, int, error)
	Perf(ctx context.Context, s *server.Server) (server.PerfSample, error)
}

// Live covers data that is followed while a page is open.
type Live interface {
	Inspect(ctx context.Context, s *server.Server) (*docker.ContainerJSON, error)
	StatsStream(ctx context.Context, s *server.Server, fn func(docker.Stats)) error
	LogsSince(ctx context.Context, s *server.Server, follow bool, tail int, since int64, fn func(line string)) error
	// FollowOps passes the output of Cubeherd actions (compose pull/up, stop) to fn until ctx ends.
	FollowOps(ctx context.Context, id string, fn func(line string))
}

// Files covers the file browser and worlds.
type Files interface {
	// Paths says where the server lives on its host; it also works for a definition that is not saved yet.
	Paths(s *server.Server) api.Paths
	ArchiveDir() string
	List(ctx context.Context, s *server.Server, rel string) ([]api.Entry, bool, error)
	Stat(ctx context.Context, s *server.Server, rel string) (api.Entry, error)
	Open(ctx context.Context, s *server.Server, rel string, w io.Writer) error
	ReadText(ctx context.Context, s *server.Server, rel string) (string, bool, error)
	ReadDataFile(s *server.Server, name string) ([]byte, error)
	WriteFile(ctx context.Context, s *server.Server, rel string, r io.Reader) error
	Remove(ctx context.Context, s *server.Server, rel string) error
	WorldZip(ctx context.Context, s *server.Server, w io.Writer) error
	StageWorld(s *server.Server, r io.Reader) (string, error)
	ImportWorld(s *server.Server, zipPath string) error
}

// Addons covers mods and plugins.
type Addons interface {
	AddonFiles(ctx context.Context, s *server.Server) ([]api.AddonFile, error)
	AddonsPending(ctx context.Context, s *server.Server) bool
	UploadAddon(ctx context.Context, s *server.Server, name string, r io.Reader) error
	DeleteUpload(ctx context.Context, s *server.Server, name string) error
}

// Backups covers archives, their health and the schedule.
type Backups interface {
	BackupsAll(ctx context.Context, s *server.Server) ([]manager.Backup, error)
	BackupNow(id, reason string) error
	BackupHealth(s *server.Server, running bool) api.Health
	DeleteBackupEverywhere(ctx context.Context, s *server.Server, name string) error
	VerifyBackup(id, name string) error
	UploadBackup(id, name string) error
	Restore(id, name string) error
	// OpenBackup opens an archive for download. A local file is an io.ReadSeeker, so it can be served with ranges.
	OpenBackup(ctx context.Context, s *server.Server, name string) (body io.ReadCloser, size int64, mod time.Time, err error)
	HasRemote() bool
	InvalidateRemote()
	NextBackupFor(s *server.Server) time.Time
	WakeScheduler()
}

// Metrics covers the optional statistics history. Every method works when recording is off: history that exists
// stays readable, and the answers are empty otherwise.
type Metrics interface {
	MetricsStatus(ctx context.Context) (api.MetricsStatus, error)
	NodeHistory(ctx context.Context, from, to time.Time, points int) (api.History, error)
	ServerHistory(ctx context.Context, id string, from, to time.Time, points int) (api.History, error)
	// MetricsLatest is the newest sample of the node ("node") and of each server (by id).
	MetricsLatest(ctx context.Context) (map[string]api.MetricPoint, error)
	// PurgeMetrics deletes the history of "node" or one server id; "" deletes all of it.
	PurgeMetrics(ctx context.Context, scope string) error
}

// Host covers the machine itself.
type Host interface {
	HostStats(ctx context.Context) host.Stats
	DockerInfo(ctx context.Context) (docker.Info, error)
	DockerHost() string
	Loc() *time.Location
}

var _ Node = (*manager.Manager)(nil)
