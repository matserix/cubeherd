// Package manager orchestrates Docker, Compose and the server store.
package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"forgecraft/internal/config"
	"forgecraft/internal/docker"
	"forgecraft/internal/i18n"
	"forgecraft/internal/server"
	"forgecraft/internal/sources"
)

type Manager struct {
	// Lang returns the interface language for messages (set by main; German if nil).
	Lang    func() string
	Docker  *docker.Client
	Store   *server.Store
	Config  *config.Store
	Sources *sources.Client

	mu    sync.Mutex
	ops   map[string]*Op
	disk  map[string]diskEntry
	crash map[string]crashEntry
	cache map[string]cacheEntry
	phase map[string]*phaseEntry

	wake     chan struct{}        // scheduler: recompute next backup
	attempts map[string]time.Time // scheduler: last attempt per server (also skipped ones)
}

// phaseEntry tracks the startup progress of one container run by scanning new log lines.
type phaseEntry struct {
	started    time.Time
	until      int64
	checked    time.Time
	done       bool
	installing bool
}

// startupPhase reports whether the current container run has finished starting and, if not,
// whether itzg is still downloading/installing files. Only new log lines are read, at most every 4 s.
func (m *Manager) startupPhase(ctx context.Context, s *server.Server, c *docker.ContainerJSON) (done, installing bool) {
	m.mu.Lock()
	e := m.phase[s.ID]
	if e == nil || !e.started.Equal(c.State.StartedAt) {
		e = &phaseEntry{started: c.State.StartedAt, until: c.State.StartedAt.Unix(), installing: true}
		m.phase[s.ID] = e
	}
	if e.done || time.Since(e.checked) < 4*time.Second {
		done, installing = e.done, e.installing
		m.mu.Unlock()
		return
	}
	e.checked = time.Now()
	since := e.until
	m.mu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	now := time.Now().Unix()
	d, inst, seen := false, false, false
	_ = m.Docker.LogsSince(lctx, s.Container(), false, -1, since, c.Config.Tty, func(l string) {
		switch server.StartupLine(server.CleanLine(l)) {
		case "done":
			d = true
		case "install":
			inst, seen = true, true
		case "server":
			inst, seen = false, true
		}
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	e.until = now - 1
	e.done = e.done || d
	if seen {
		e.installing = inst
	}
	return e.done, e.installing
}

type cacheEntry struct {
	at      time.Time
	players []string
	max     int
	err     error
}

// cached returns a value computed at most once per ttl for the given key.
func (m *Manager) cached(key string, ttl time.Duration, fn func() cacheEntry) cacheEntry {
	m.mu.Lock()
	e, ok := m.cache[key]
	m.mu.Unlock()
	if ok && time.Since(e.at) < ttl {
		return e
	}
	e = fn()
	e.at = time.Now()
	m.mu.Lock()
	m.cache[key] = e
	m.mu.Unlock()
	return e
}

// Forget drops cached RCON results, e.g. after a player list change.
func (m *Manager) Forget(id string) {
	m.mu.Lock()
	delete(m.cache, "players:"+id)
	delete(m.cache, "paused:"+id)
	m.mu.Unlock()
}

type crashEntry struct {
	finished time.Time
	reason   string
}

// crashReason reads the log tail once per crash and caches the result.
func (m *Manager) crashReason(ctx context.Context, s *server.Server, c *docker.ContainerJSON) string {
	m.mu.Lock()
	e, ok := m.crash[s.ID]
	m.mu.Unlock()
	if ok && e.finished.Equal(c.State.FinishedAt) {
		return e.reason
	}
	var lines []string
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = m.Docker.Logs(lctx, s.Container(), false, 3000, c.Config.Tty, func(l string) { lines = append(lines, server.CleanLine(l)) })
	cancel()
	reason := server.CrashReason(lines)
	if c.State.Running {
		// Restart loop: the previous run was usually killed without a message; guessing misleads.
		reason = server.KnownCrashReason(lines)
	}
	// Hint lines are German source texts; log lines pass through unchanged.
	parts := strings.Split(reason, "\n")
	for i := range parts {
		parts[i] = m.t(parts[i])
	}
	reason = strings.Join(parts, "\n")
	if c.State.OOMKilled {
		reason = m.t("Der Container hat sein Speicherlimit von %d GB überschritten und wurde beendet. Arbeitsspeicher in den Einstellungen erhöhen.", s.MemoryGB)
	}
	m.mu.Lock()
	m.crash[s.ID] = crashEntry{c.State.FinishedAt, reason}
	m.mu.Unlock()
	return reason
}

// t translates a German source text into the interface language.
func (m *Manager) t(key string, args ...any) string {
	lang := i18n.Source
	if m.Lang != nil {
		lang = m.Lang()
	}
	return i18n.T(lang, key, args...)
}

func New(d *docker.Client, st *server.Store, cfg *config.Store, src *sources.Client) *Manager {
	return &Manager{Docker: d, Store: st, Config: cfg, Sources: src, ops: map[string]*Op{}, disk: map[string]diskEntry{}, crash: map[string]crashEntry{}, cache: map[string]cacheEntry{}, phase: map[string]*phaseEntry{},
		wake: make(chan struct{}, 1), attempts: map[string]time.Time{}}
}

// ---------------- Operations ----------------

// Op is a running Forgecraft action whose output can be followed.
type Op struct {
	Status server.Status
	mu     sync.Mutex
	lines  []string
	notify chan struct{}
	done   bool
	Err    error
}

func (o *Op) add(l string) {
	o.mu.Lock()
	o.lines = append(o.lines, l)
	if len(o.lines) > 2000 {
		o.lines = o.lines[len(o.lines)-2000:]
	}
	close(o.notify)
	o.notify = make(chan struct{})
	o.mu.Unlock()
}

// Since returns lines after index i, a channel that closes on the next change and whether the op ended.
func (o *Op) Since(i int) ([]string, <-chan struct{}, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if i > len(o.lines) {
		i = len(o.lines)
	}
	return append([]string(nil), o.lines[i:]...), o.notify, o.done
}

func (m *Manager) Op(id string) *Op {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops[id]
}

func (m *Manager) opStatus(id string) server.Status {
	if o := m.Op(id); o != nil {
		o.mu.Lock()
		defer o.mu.Unlock()
		if !o.done {
			return o.Status
		}
	}
	return ""
}

var ErrBusy = errors.New("Für diesen Server läuft bereits eine Aktion")

// run starts fn in the background as the server's current operation.
func (m *Manager) run(id string, st server.Status, fn func(o *Op) error) error {
	m.mu.Lock()
	if o := m.ops[id]; o != nil && !o.done {
		m.mu.Unlock()
		return errors.New(m.t(ErrBusy.Error()))
	}
	o := &Op{Status: st, notify: make(chan struct{})}
	m.ops[id] = o
	m.mu.Unlock()
	go func() {
		err := fn(o)
		if err != nil {
			o.add("[forgecraft] " + m.t("Fehler: %s", err.Error()))
			log.Printf("%s %s: %v", id, st, err)
		}
		o.mu.Lock()
		o.Err, o.done = err, true
		close(o.notify)
		o.notify = make(chan struct{})
		o.mu.Unlock()
	}()
	return nil
}

func (m *Manager) compose(ctx context.Context, s *server.Server, o *Op, args ...string) error {
	dir := m.Store.Dir(s.ID)
	full := append([]string{"compose", "--progress", "plain", "-f", filepath.Join(dir, "compose.yml"), "--project-directory", dir}, args...)
	o.add("[forgecraft] docker " + strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", full...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}
	go func() { err := cmd.Wait(); pw.CloseWithError(err) }()
	sc := bufio.NewScanner(pr)
	last := ""
	for sc.Scan() {
		if l := strings.TrimSpace(server.CleanLine(sc.Text())); l != "" && l != last {
			o.add(l)
			last = l
		}
	}
	if err := sc.Err(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf(m.t("docker compose %s fehlgeschlagen (Exit %d)"), args[0], ee.ExitCode())
		}
		return err
	}
	return nil
}

// ---------------- Views ----------------

type View struct {
	S           *server.Server
	Status      server.Status
	C           *docker.ContainerJSON
	Online      []string
	MaxPlayers  int
	MemUsed     uint64
	CrashReason string
	Since       time.Time
	OpError     string
	Restarts    int  // unexpected restarts of the current container (restart policy)
	AutoStopped bool // stopped by itzg autostop after idling
}

func (m *Manager) inspect(ctx context.Context, s *server.Server) *docker.ContainerJSON {
	c, err := m.Docker.Inspect(ctx, s.Container())
	if err != nil {
		return nil
	}
	return c
}

// View collects everything the UI shows for one server. withStats adds memory and players.
func (m *Manager) View(ctx context.Context, s *server.Server, withStats bool) View {
	v := View{S: s, MaxPlayers: s.Props.MaxPlayers}
	v.C = m.inspect(ctx, s)
	v.Status = server.Derive(v.C, m.opStatus(s.ID), m.Store.DataDir(s))
	if v.Status == server.StatusRunning && m.Store.DataDir(s) == "" {
		// Data in a Docker volume: check the autopause marker from inside the container.
		e := m.cached("paused:"+s.ID, 10*time.Second, func() cacheEntry {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, code, err := m.Docker.Exec(pctx, s.Container(), "test", "-e", "/data/.paused")
			if err == nil && code == 0 {
				return cacheEntry{max: 1}
			}
			return cacheEntry{}
		})
		if e.max == 1 {
			v.Status = server.StatusPaused
		}
	}
	if o := m.Op(s.ID); o != nil && o.done && o.Err != nil {
		v.OpError = o.Err.Error()
	}
	if v.C != nil && v.C.State.Running {
		v.Since = v.C.State.StartedAt
	}
	if v.C != nil && v.C.State.Running && (v.Status == server.StatusStarting || v.Status == server.StatusUnhealthy) {
		if done, installing := m.startupPhase(ctx, s, v.C); !done {
			v.Status = server.StatusStarting
			if installing {
				v.Status = server.StatusPreparing
			}
		}
	}
	if v.C != nil {
		v.Restarts = v.C.RestartCount
	}
	if v.Status == server.StatusStopped && s.Autostop && v.C != nil {
		key := fmt.Sprintf("autostop:%s:%d", s.ID, v.C.State.FinishedAt.Unix())
		e := m.cached(key, 24*time.Hour, func() cacheEntry {
			found := false
			lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_ = m.Docker.Logs(lctx, s.Container(), false, 150, v.C.Config.Tty, func(l string) {
				found = found || strings.Contains(l, "[Autostop]")
			})
			if found {
				return cacheEntry{max: 1}
			}
			return cacheEntry{}
		})
		v.AutoStopped = e.max == 1
	}
	if v.C != nil && (v.Status == server.StatusCrashed || v.Restarts > 0) {
		v.CrashReason = m.crashReason(ctx, s, v.C)
	}
	if withStats && v.C != nil && v.C.State.Running {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if st, err := m.Docker.StatsOnce(sctx, s.Container()); err == nil {
				v.MemUsed = st.MemUsed
			}
		}()
		if v.Status.Live() {
			if on, max, err := m.Players(ctx, s); err == nil {
				v.Online, v.MaxPlayers = on, max
			}
		}
		wg.Wait()
	}
	return v
}

// Views loads all servers in parallel.
func (m *Manager) Views(ctx context.Context, withStats bool) ([]View, error) {
	list, err := m.Store.List()
	if err != nil {
		return nil, err
	}
	out := make([]View, len(list))
	var wg sync.WaitGroup
	for i, s := range list {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = m.View(ctx, s, withStats) }()
	}
	wg.Wait()
	return out, nil
}

var listRe = regexp.MustCompile(`There are (\d+) of a max(?: of)? (\d+) players online:?(.*)`)

// Players asks the server via RCON who is online. Results are shared for 15 s, because every
// RCON connection spawns rcon-cli in the container and writes to the server log.
func (m *Manager) Players(ctx context.Context, s *server.Server) ([]string, int, error) {
	e := m.cached("players:"+s.ID, 15*time.Second, func() cacheEntry {
		p, max, err := m.players(ctx, s)
		return cacheEntry{players: p, max: max, err: err}
	})
	return e.players, e.max, e.err
}

func (m *Manager) players(ctx context.Context, s *server.Server) ([]string, int, error) {
	out, err := m.RCON(ctx, s, "list")
	if err != nil {
		return nil, 0, err
	}
	mm := listRe.FindStringSubmatch(server.CleanLine(out))
	if mm == nil {
		return nil, 0, fmt.Errorf(m.t("unerwartete Antwort: %q"), out)
	}
	max, _ := strconv.Atoi(mm[2])
	var names []string
	for _, n := range strings.Split(mm[3], ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names, max, nil
}

// RCON runs a console command through rcon-cli inside the container.
func (m *Manager) RCON(ctx context.Context, s *server.Server, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, code, err := m.Docker.Exec(ctx, s.Container(), "rcon-cli", cmd)
	if err != nil {
		return "", err
	}
	out = strings.TrimRight(server.CleanLine(out), "\n ")
	if code != 0 {
		if out == "" {
			out = fmt.Sprintf("rcon-cli Exit %d", code)
		}
		return "", errors.New(out)
	}
	return out, nil
}

// ---------------- Lifecycle ----------------

func (m *Manager) globals() server.Globals {
	c := m.Config.Get()
	return server.Globals{CFKey: c.CFAPIKey, TZ: c.Timezone}
}

// Loc is the dashboard's time zone (setting, else TZ/system).
func (m *Manager) Loc() *time.Location {
	if tz := m.Config.Get().Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// Create saves a new server and brings it up in the background.
func (m *Manager) Create(s *server.Server) error {
	if m.Store.Exists(s.ID) {
		return fmt.Errorf(m.t("Die ID „%s“ ist schon vergeben"), s.ID)
	}
	if err := m.Store.Save(s, m.globals()); err != nil {
		return err
	}
	return m.run(s.ID, server.StatusInstalling, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		o.add("[forgecraft] " + m.t("Compose-Datei geschrieben: %s", filepath.Join(m.Store.Dir(s.ID), "compose.yml")))
		if err := m.compose(ctx, s, o, "pull", "--quiet"); err != nil {
			return err
		}
		return m.compose(ctx, s, o, "up", "-d")
	})
}

func (m *Manager) Start(id string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	// Regenerate compose.yml so saved settings (and Forgecraft updates) apply on every start.
	if err := m.Store.Save(s, m.globals()); err != nil {
		return err
	}
	return m.run(id, server.StatusStarting, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		// compose up starts the existing container, or recreates it when the compose file changed.
		return m.compose(ctx, s, o, "up", "-d")
	})
}

func (m *Manager) Stop(id string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	return m.run(id, server.StatusStopping, func(o *Op) error {
		o.add("[forgecraft] " + m.t("Server wird gestoppt, Welt wird gespeichert"))
		return m.Docker.Stop(context.Background(), s.Container(), time.Minute)
	})
}

func (m *Manager) Restart(id string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	if err := m.Store.Save(s, m.globals()); err != nil {
		return err
	}
	return m.run(id, server.StatusStopping, func(o *Op) error {
		ctx := context.Background()
		if err := m.Docker.Stop(ctx, s.Container(), time.Minute); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return err
		}
		o.mu.Lock()
		o.Status = server.StatusStarting
		o.mu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		return m.compose(ctx, s, o, "up", "-d")
	})
}

// Apply saves the definition and recreates the container if it is running (or always with force).
func (m *Manager) Apply(s *server.Server, pull, restart bool) error {
	return m.ApplyUpdate(s, pull, restart, false)
}

// ApplyUpdate is Apply with an optional backup before anything changes.
func (m *Manager) ApplyUpdate(s *server.Server, pull, restart, backupFirst bool) error {
	if !restart && !pull {
		return m.Store.Save(s, m.globals())
	}
	return m.run(s.ID, server.StatusUpdating, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if backupFirst {
			// Back up with the old definition, before the new compose file is written.
			old, err := m.Store.Get(s.ID)
			if err == nil {
				if _, err := m.backup(ctx, old, o, ReasonUpdate, m.BackupDir(old), ""); err != nil {
					return fmt.Errorf(m.t("Backup vor dem Update fehlgeschlagen, Update abgebrochen: %w"), err)
				}
			}
		}
		if err := m.Store.Save(s, m.globals()); err != nil {
			return err
		}
		if pull {
			if err := m.compose(ctx, s, o, "pull", "--quiet"); err != nil {
				return err
			}
		}
		return m.compose(ctx, s, o, "up", "-d", "--force-recreate")
	})
}

// Remove modes for Uninstall.
const (
	RemoveKeepData = "keep"
	RemoveAll      = "all"
	RemoveBackup   = "backup" // final backup into <data>/archive, then remove everything
)

func (m *Manager) ArchiveDir() string { return filepath.Join(m.Store.Root(), "archive") }

func (m *Manager) Uninstall(id, mode string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	return m.run(id, server.StatusStopping, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if mode == RemoveBackup {
			if _, err := m.backup(ctx, s, o, ReasonUninstall, m.ArchiveDir(), s.ID+"-"); err != nil {
				return fmt.Errorf(m.t("Abschluss-Backup fehlgeschlagen, nichts gelöscht: %w"), err)
			}
		}
		if err := m.compose(ctx, s, o, "down", "--timeout", "60"); err != nil {
			// Fall back to removing the container directly (e.g. compose file damaged).
			if err := m.Docker.Remove(ctx, s.Container(), true); err != nil && !errors.Is(err, docker.ErrNotFound) {
				return err
			}
		}
		if mode == RemoveKeepData {
			return m.Store.RemoveDefinition(id)
		}
		if s.DataDir != "" {
			// Imported servers keep foreign data folders untouched.
			return m.Store.RemoveDir(id)
		}
		if err := m.Store.RemoveDir(id); err != nil && errors.Is(err, fs.ErrPermission) {
			// Files written by the container user: remove them from inside a throwaway container.
			o.add("[forgecraft] " + m.t("Dateien gehören dem Container-Benutzer, lösche per Docker"))
			cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", m.Store.Dir(id)+":/x", "--entrypoint", "rm", s.ImageRef(), "-rf", "/x/data")
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf(m.t("löschen fehlgeschlagen: %s"), strings.TrimSpace(string(out)))
			}
			return m.Store.RemoveDir(id)
		} else if err != nil {
			return err
		}
		return nil
	})
}

// ---------------- Ports & disk ----------------

// NextPort returns the lowest port in the configured range not used by another server.
func (m *Manager) NextPort(excludeID string) int {
	cfg := m.Config.Get()
	used := map[int]bool{}
	if list, err := m.Store.List(); err == nil {
		for _, s := range list {
			if s.ID != excludeID {
				used[s.Port] = true
			}
		}
	}
	for p := cfg.PortMin; p <= cfg.PortMax; p++ {
		if !used[p] {
			return p
		}
	}
	return cfg.PortMax + 1
}

func (m *Manager) PortTaken(port int, excludeID string) bool {
	list, _ := m.Store.List()
	for _, s := range list {
		if s.ID != excludeID && s.Port == port {
			return true
		}
	}
	return false
}

type diskEntry struct {
	at      time.Time
	size    int64
	running bool
}

// DiskUsage returns the cached size of a server's data folder and refreshes it in the background.
func (m *Manager) DiskUsage(s *server.Server) int64 {
	m.mu.Lock()
	e, ok := m.disk[s.ID]
	stale := !ok || time.Since(e.at) > 10*time.Minute
	if stale && !e.running {
		e.running = true
		m.disk[s.ID] = e
		dir := m.Store.DataDir(s)
		go func() {
			var total int64
			_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if info, err := d.Info(); err == nil {
						total += info.Size()
					}
				}
				return nil
			})
			m.mu.Lock()
			m.disk[s.ID] = diskEntry{at: time.Now(), size: total}
			m.mu.Unlock()
		}()
	}
	m.mu.Unlock()
	return e.size
}

// ReadDataFile reads a small JSON file from the server's data folder (whitelist.json etc.).
func (m *Manager) ReadDataFile(s *server.Server, name string) ([]byte, error) {
	if strings.ContainsAny(name, `/\`) {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(m.Store.DataDir(s), name))
}
