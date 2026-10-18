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

// Backup is one archive in servers/<id>/backups, named <YYYYMMDD-HHMMSS>_<reason>.tar.zst.
type Backup struct {
	Name   string
	Time   time.Time
	Reason string
	Size   int64
}

const (
	ReasonManual    = "manual"
	ReasonSchedule  = "schedule"
	ReasonUpdate    = "update"
	ReasonRestore   = "restore"
	ReasonUninstall = "uninstall"
)

var reasonLabel = map[string]string{
	ReasonManual: "Manuell", ReasonSchedule: "Zeitplan", ReasonUpdate: "Vor Update",
	ReasonRestore: "Vor Wiederherstellung", ReasonUninstall: "Vor Deinstallation",
}

func (b Backup) Label() string { return reasonLabel[b.Reason] }

// Automatic backups are pruned by the plan's Keep value; manual ones stay until deleted.
func (b Backup) Automatic() bool { return b.Reason != ReasonManual }

var backupName = regexp.MustCompile(`^(\d{8}-\d{6})_([a-z]+)\.tar\.zst$`)

func (m *Manager) BackupDir(s *server.Server) string {
	return filepath.Join(m.Store.Dir(s.ID), "backups")
}

// Backups lists a server's archives, newest first.
func (m *Manager) Backups(s *server.Server) ([]Backup, error) {
	entries, err := os.ReadDir(m.BackupDir(s))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Backup
	for _, e := range entries {
		mm := backupName.FindStringSubmatch(e.Name())
		if mm == nil {
			continue
		}
		t, err := time.ParseInLocation("20060102-150405", mm[1], time.Local)
		if err != nil {
			continue
		}
		b := Backup{Name: e.Name(), Time: t, Reason: mm[2]}
		if info, err := e.Info(); err == nil {
			b.Size = info.Size()
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// BackupPath validates a backup name and returns its path.
func (m *Manager) BackupPath(s *server.Server, name string) (string, error) {
	if !backupName.MatchString(name) {
		return "", os.ErrNotExist
	}
	p := filepath.Join(m.BackupDir(s), name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func (m *Manager) DeleteBackup(s *server.Server, name string) error {
	p, err := m.BackupPath(s, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// BackupNow creates a backup in the background as the server's current operation.
func (m *Manager) BackupNow(id, reason string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	return m.run(id, server.StatusBackingUp, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		_, err := m.backup(ctx, s, o, reason, m.BackupDir(s), "")
		return err
	})
}

// excludes skips files that itzg downloads again on start; modpack mods are reinstalled from the pack.
func excludes(s *server.Server) []string {
	ex := []string{"--no-wildcards-match-slash"}
	for _, p := range []string{"./logs", "./crash-reports", "./debug", "./cache", "./.cache", "./versions", "./libraries", "./libraries-*",
		"./*.jar", "./.paused", "./.rcon-cli.*", "./.mixin.out", "./backups"} {
		ex = append(ex, "--exclude="+p)
	}
	if s.Pack != nil {
		ex = append(ex, "--exclude=./mods", "--exclude=./dynamic-data-pack-cache")
	}
	return ex
}

// dataMount is the -v source for /data when the container is not running.
func (m *Manager) dataMount(s *server.Server) string {
	if s.DataVol != "" {
		return s.DataVol
	}
	return m.Store.DataDir(s)
}

func (m *Manager) paused(ctx context.Context, s *server.Server) bool {
	if dir := m.Store.DataDir(s); dir != "" {
		_, err := os.Stat(filepath.Join(dir, ".paused"))
		return err == nil
	}
	_, code, err := m.Docker.Exec(ctx, s.Container(), "test", "-e", "/data/.paused")
	return err == nil && code == 0
}

// backup writes one archive into destDir. A running server stops writing chunks while the
// archive is made (save-off / save-all flush / save-on); paused and stopped servers are consistent anyway.
func (m *Manager) backup(ctx context.Context, s *server.Server, o *Op, reason, destDir, prefix string) (Backup, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Backup{}, err
	}
	now := time.Now()
	name := prefix + now.Format("20060102-150405") + "_" + reason + ".tar.zst"
	tmp := filepath.Join(destDir, "."+name+".part")
	final := filepath.Join(destDir, name)

	c := m.inspect(ctx, s)
	running := c != nil && c.State.Running
	tarArgs := append([]string{"-n", "19", "ionice", "-c3", "tar", "-I", "zstd -3 -T1"}, excludes(s)...)
	tarArgs = append(tarArgs, "-cf", "-", "-C", "/data", ".")

	var cmd *exec.Cmd
	if running {
		if !m.paused(ctx, s) {
			if _, err := m.RCON(ctx, s, "save-off"); err == nil {
				defer func() {
					if _, err := m.RCON(context.Background(), s, "save-on"); err != nil {
						o.add("[forgecraft] Warnung: save-on fehlgeschlagen: " + err.Error())
					}
				}()
				o.add("[forgecraft] Automatisches Speichern pausiert, Welt wird geschrieben")
				if _, err := m.RCON(ctx, s, "save-all flush"); err != nil {
					o.add("[forgecraft] Warnung: save-all fehlgeschlagen: " + err.Error())
				}
			} else {
				o.add("[forgecraft] RCON nicht erreichbar, sichere den aktuellen Stand auf der Platte")
			}
		}
		cmd = exec.CommandContext(ctx, "docker", append([]string{"exec", s.Container(), "nice"}, tarArgs...)...)
	} else {
		cmd = exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", "--entrypoint", "nice",
			"-v", m.dataMount(s) + ":/data:ro", s.ImageRef()}, tarArgs...)...)
	}
	o.add("[forgecraft] Erstelle Backup " + name)

	f, err := os.Create(tmp)
	if err != nil {
		return Backup{}, err
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = f, &stderr
	runErr := cmd.Run()
	closeErr := f.Close()
	// GNU tar exits with 1 when files changed while reading; the archive is still usable.
	var ee *exec.ExitError
	if runErr != nil && !(errors.As(runErr, &ee) && ee.ExitCode() == 1) {
		os.Remove(tmp)
		return Backup{}, fmt.Errorf("Backup fehlgeschlagen: %s", firstLine(stderr.String(), runErr))
	}
	if closeErr != nil {
		os.Remove(tmp)
		return Backup{}, closeErr
	}
	if err := os.Rename(tmp, final); err != nil {
		return Backup{}, err
	}
	info, _ := os.Stat(final)
	b := Backup{Name: name, Time: now, Reason: reason}
	if info != nil {
		b.Size = info.Size()
	}
	o.add(fmt.Sprintf("[forgecraft] Backup fertig: %s (%.1f MB, %s)", name, float64(b.Size)/(1<<20), time.Since(now).Round(time.Second)))
	if prefix == "" && b.Automatic() {
		if n := m.prune(s); n > 0 {
			o.add(fmt.Sprintf("[forgecraft] %d alte automatische Backups gelöscht", n))
		}
	}
	return b, nil
}

func firstLine(s string, err error) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return err.Error()
	}
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// prune deletes automatic backups beyond the plan's Keep value.
func (m *Manager) prune(s *server.Server) int {
	list, err := m.Backups(s)
	if err != nil {
		return 0
	}
	keep, n := s.Plan().Keep, 0
	if keep <= 0 {
		keep = 1
	}
	auto := 0
	for _, b := range list {
		if !b.Automatic() {
			continue
		}
		if auto++; auto > keep {
			if os.Remove(filepath.Join(m.BackupDir(s), b.Name)) == nil {
				n++
			}
		}
	}
	return n
}

// restoreScript removes the top-level entries contained in the archive and extracts it.
// Everything the archive does not contain (e.g. modpack mods) stays untouched.
const restoreScript = `set -e
cd /data
tar -I zstd -tf "/b/$F" | cut -d/ -f2 | sort -u | grep -v -e '^$' -e '^\.$' -e '^\.\.$' | while IFS= read -r n; do rm -rf -- "./$n"; done
tar -I zstd -xf "/b/$F"`

// Restore replaces the world and configuration with a backup. The current state is saved first.
func (m *Manager) Restore(id, name string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	if _, err := m.BackupPath(s, name); err != nil {
		return fmt.Errorf("Backup nicht gefunden")
	}
	return m.run(id, server.StatusRestoring, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		c := m.inspect(ctx, s)
		wasUp := c != nil && c.State.Running
		if wasUp {
			o.add("[forgecraft] Server wird gestoppt")
			if err := m.Docker.Stop(ctx, s.Container(), time.Minute); err != nil {
				return err
			}
		}
		if _, err := m.backup(ctx, s, o, ReasonRestore, m.BackupDir(s), ""); err != nil {
			return fmt.Errorf("Sicherung des aktuellen Stands fehlgeschlagen, Wiederherstellung abgebrochen: %w", err)
		}
		o.add("[forgecraft] Stelle " + name + " wieder her")
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-e", "F="+name, "-v", m.dataMount(s)+":/data",
			"-v", m.BackupDir(s)+":/b:ro", "--entrypoint", "sh", s.ImageRef(), "-c", restoreScript)
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.Discard, &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("Wiederherstellung fehlgeschlagen: %s", firstLine(stderr.String(), err))
		}
		o.add("[forgecraft] Wiederherstellung abgeschlossen")
		if wasUp {
			if err := m.Store.Save(s, m.cfKey()); err != nil {
				return err
			}
			return m.compose(ctx, s, o, "up", "-d")
		}
		return nil
	})
}
