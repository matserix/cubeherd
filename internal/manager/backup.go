package manager

import (
	"bytes"
	"context"
	"encoding/json"
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

// Backup is one archive in servers/<id>/backups, named <YYYYMMDD-HHMMSS>_<reason>.tar.zst.
type Backup struct {
	Name   string
	Time   time.Time
	Reason string
	Size   int64
	Local  bool // archive exists in servers/<id>/backups
	Remote bool // archive exists in the external storage
	// UploadErr is set by backup() when the archive was made but the external upload failed.
	UploadErr error
	// Check is the stored verification result: "" (never checked), ok or bad.
	Check, CheckErr string
	// Snapshot is the server software at the time of the backup; nil if unknown.
	Snapshot *server.Snapshot
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

// ReasonLabels returns all backup reason texts (translation keys).
func ReasonLabels() []string {
	var out []string
	for _, v := range reasonLabel {
		out = append(out, v)
	}
	return out
}

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
		// The name is only a label; the file time is authoritative, so changing the time zone
		// never shifts existing backups.
		b := Backup{Name: e.Name(), Reason: mm[2], Local: true}
		if info, err := e.Info(); err == nil {
			b.Size, b.Time = info.Size(), info.ModTime()
		} else if t, err := time.ParseInLocation("20060102-150405", mm[1], m.Loc()); err == nil {
			b.Time = t
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
	if err := os.Remove(p); err != nil {
		return err
	}
	m.forgetSnapshot(s, name)
	return nil
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
		b, err := m.backup(ctx, s, o, reason, m.BackupDir(s), "")
		if err == nil {
			err = b.UploadErr // visible as a failed operation; the local archive is kept
		}
		return err
	})
}

// excludes skips files that itzg downloads again on start; modpack mods are reinstalled from the pack.
func excludes(s *server.Server) []string {
	ex := []string{"--no-wildcards-match-slash"}
	for _, p := range []string{"./logs", "./crash-reports", "./debug", "./cache", "./.cache", "./versions", "./libraries", "./libraries-*",
		"./*.jar", "./.paused", "./.rcon-cli.*", "./.mixin.out", "./backups",
		"./.fabric/server", "./.fabric/remappedJars", "./" + restoreStash} {
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
	b, err := m.backupRun(ctx, s, o, reason, destDir, prefix)
	if err != nil && prefix == "" && !errors.Is(err, errCorrupt) {
		m.recordFail(s.ID, err.Error(), false)
	}
	return b, err
}

func (m *Manager) backupRun(ctx context.Context, s *server.Server, o *Op, reason, destDir, prefix string) (Backup, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Backup{}, err
	}
	now := time.Now()
	name := prefix + now.In(m.Loc()).Format("20060102-150405") + "_" + reason + ".tar.zst"
	tmp := filepath.Join(destDir, "."+name+".part")
	final := filepath.Join(destDir, name)

	c := m.inspect(ctx, s)
	running := c != nil && c.State.Running
	snapshot := m.installedSnapshot(ctx, s, c)
	// An unknown version is written as an empty object: it still shadows a file of that name in /data.
	meta := []byte("{}")
	if snapshot != nil {
		var err error
		if meta, err = json.Marshal(snapshot); err != nil {
			return Backup{}, err
		}
	}
	env := "SNAPSHOT=" + string(meta)
	tarArgs := append([]string{"-n", "19", "sh", "-c", archiveScript, "sh"}, excludes(s)...)

	var cmd *exec.Cmd
	if running {
		if !m.paused(ctx, s) {
			if _, err := m.RCON(ctx, s, "save-off"); err == nil {
				defer func() {
					if _, err := m.RCON(context.Background(), s, "save-on"); err != nil {
						o.add("[cubeherd] " + m.t("Warnung: save-on fehlgeschlagen: %s", err.Error()))
					}
				}()
				o.add("[cubeherd] " + m.t("Automatisches Speichern pausiert, Welt wird geschrieben"))
				if _, err := m.RCON(ctx, s, "save-all flush"); err != nil {
					o.add("[cubeherd] " + m.t("Warnung: save-all fehlgeschlagen: %s", err.Error()))
				}
			} else {
				o.add("[cubeherd] " + m.t("RCON nicht erreichbar, sichere den aktuellen Stand auf der Platte"))
			}
		}
		cmd = exec.CommandContext(ctx, "docker", append([]string{"exec", "-e", env, s.Container(), "nice"}, tarArgs...)...)
	} else {
		cmd = exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", "-e", env, "--entrypoint", "nice",
			"-v", m.dataMount(s) + ":/data:ro", s.ImageRef()}, tarArgs...)...)
	}
	o.add("[cubeherd] " + m.t("Erstelle Backup %s", name))

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
		return Backup{}, fmt.Errorf(m.t("Backup fehlgeschlagen: %s"), firstLine(stderr.String(), runErr))
	}
	if closeErr != nil {
		os.Remove(tmp)
		return Backup{}, closeErr
	}
	if err := os.Rename(tmp, final); err != nil {
		return Backup{}, err
	}
	info, _ := os.Stat(final)
	b := Backup{Name: name, Time: now, Reason: reason, Snapshot: snapshot}
	if info != nil {
		b.Size = info.Size()
	}
	o.add("[cubeherd] " + m.t("Backup fertig: %s (%.1f MB, %s)", name, float64(b.Size)/(1<<20), time.Since(now).Round(time.Second)))
	if prefix == "" && snapshot != nil {
		m.recordSnapshot(s.ID, name, snapshot)
	}
	if !s.Plan().NoVerify {
		o.add("[cubeherd] " + m.t("Prüfe Archiv"))
		if err := m.verifyArchive(ctx, s, destDir, name); err != nil {
			if prefix == "" {
				m.recordCheck(s.ID, name, err)
				m.recordFail(s.ID, err.Error(), true)
			}
			// A broken archive is kept for inspection, but neither pruned against nor uploaded.
			return b, &corruptError{fmt.Sprintf(m.t("Backup beschädigt: %s"), err.Error())}
		}
		if prefix == "" {
			m.recordCheck(s.ID, name, nil)
		}
		o.add("[cubeherd] " + m.t("Archiv ist in Ordnung"))
	}
	if prefix == "" && b.Automatic() {
		if n := m.prune(s, o.pinned); n > 0 {
			o.add("[cubeherd] " + m.t("%d alte automatische Backups gelöscht", n))
		}
	}
	if prefix == "" {
		m.ship(ctx, s, o, &b)
		if b.UploadErr != nil {
			m.recordFail(s.ID, b.UploadErr.Error(), false)
		} else {
			m.recordOK(s.ID)
		}
	}
	return b, nil
}

// ship uploads a fresh archive when the plan asks for it. With RemoteOnly the local copy is removed
// after a successful upload; a failed upload always keeps it. The safety copy made before a restore
// stays local, so a restore never depends on the external storage being reachable.
func (m *Manager) ship(ctx context.Context, s *server.Server, o *Op, b *Backup) {
	plan := s.Plan()
	if !plan.Remote || m.remote() == nil {
		return
	}
	if err := m.upload(ctx, s, o, b.Name); err != nil {
		b.UploadErr = fmt.Errorf(m.t("Upload in den externen Speicher fehlgeschlagen: %w"), err)
		o.add("[cubeherd] " + m.t("Warnung: %s", b.UploadErr.Error()))
		return
	}
	b.Remote = true
	if plan.RemoteOnly && b.Reason != ReasonRestore {
		if os.Remove(filepath.Join(m.BackupDir(s), b.Name)) == nil {
			o.add("[cubeherd] " + m.t("Lokale Kopie von %s gelöscht, das Backup liegt nur im externen Speicher", b.Name))
		}
	}
}

func firstLine(s string, err error) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return err.Error()
	}
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// prune deletes automatic backups beyond the plan's Keep value. The archive named pinned is being restored and
// stays.
func (m *Manager) prune(s *server.Server, pinned string) int {
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
		if auto++; auto > keep && b.Name != pinned {
			if os.Remove(filepath.Join(m.BackupDir(s), b.Name)) == nil {
				m.forgetSnapshot(s, b.Name)
				n++
			}
		}
	}
	return n
}

// restoreScript removes the top-level entries contained in the archive and extracts it.
// Everything the archive does not contain (e.g. modpack mods) stays untouched. The archive is listed completely
// before anything is removed.
// For modpacks ($PACK) itzg's manifests that already exist are kept: they list the mods that are on disk, which
// the archive does not carry. An older manifest would leave newer mods behind next to the older ones.
const restoreScript = `set -e
cd /data
keep='` + snapshotFile + `'
set -- --exclude=./` + snapshotFile + `
if [ -n "$PACK" ]; then
  for f in .*manifest.json; do
    [ -f "$f" ] || continue
    keep="$keep
$f"
    set -- "$@" "--exclude=./$f"
  done
fi
list=$(mktemp)
tar -I zstd -tf "/b/$F" >"$list"
cut -d/ -f2 "$list" | sort -u | grep -v -e '^$' -e '^\.$' -e '^\.\.$' | grep -vxF -e "$keep" | while IFS= read -r n; do rm -rf -- "./$n"; done
rm -f "$list"
tar -I zstd "$@" -xf "/b/$F"`

// restoreStash is the folder in /data that holds what is needed to undo a restore. It is never backed up.
const restoreStash = ".cubeherd-restore"

// stashScript notes which entries exist before a restore. With $MODS it also copies what the safety backup does
// not carry and a modpack installation changes: the mods and itzg's manifests.
const stashScript = `set -e
cd /data
d=` + restoreStash + `
rm -rf "$d"
mkdir "$d"
ls -A >"$d/entries"
if [ -n "$MODS" ]; then
  if [ -d mods ]; then cp -a mods "$d/mods"; fi
  for f in .*manifest.json; do
    if [ -f "$f" ]; then cp -a "$f" "$d/"; fi
  done
  touch "$d/with-mods"
fi`

// revertScript removes entries a failed restore added and puts the stashed mods and manifests back. The files of
// the safety backup are extracted afterwards.
const revertScript = `set -e
cd /data
d=` + restoreStash + `
test -f "$d/entries"
ls -A | { grep -vxF -f "$d/entries" || true; } | while IFS= read -r n; do rm -rf -- "./$n"; done
if [ -f "$d/with-mods" ]; then
  rm -rf mods
  rm -f .*manifest.json
  if [ -d "$d/mods" ]; then cp -a "$d/mods" mods; fi
  for f in "$d"/.*manifest.json; do
    if [ -f "$f" ]; then cp -a "$f" .; fi
  done
fi`

const dropStashScript = `rm -rf /data/` + restoreStash

// extract unpacks a local archive into the server's data.
func (m *Manager) extract(ctx context.Context, s *server.Server, name string) error {
	pack := ""
	if s.Pack != nil {
		pack = "1"
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-e", "F="+name, "-e", "PACK="+pack, "-v", m.dataMount(s)+":/data",
		"-v", m.BackupDir(s)+":/b:ro", "--entrypoint", "sh", s.ImageRef(), "-c", restoreScript)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	if err := cmd.Run(); err != nil {
		return errors.New(firstLine(stderr.String(), err))
	}
	return nil
}

// revert brings the data back to the state before a restore that failed with cause, using the stash and the safety
// backup. It has its own time limit, because the restore may have failed by running out of time.
func (m *Manager) revert(s *server.Server, o *Op, safety string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	o.add("[cubeherd] " + m.t("Fehlgeschlagen, spiele den Stand davor zurück"))
	err := m.dataShell(ctx, s, revertScript, nil, nil, io.Discard, false)
	if err == nil {
		err = m.extract(ctx, s, safety)
	}
	if err != nil {
		return errors.Join(fmt.Errorf(m.t("Wiederherstellung fehlgeschlagen, und der Stand davor ließ sich nicht zurückspielen. Er liegt im Backup „Vor Wiederherstellung“: %w"), cause), err)
	}
	m.dropStash(ctx, s, o)
	return fmt.Errorf(m.t("Wiederherstellung fehlgeschlagen, der Stand davor wurde zurückgespielt. Der Server bleibt gestoppt: %w"), cause)
}

func (m *Manager) dropStash(ctx context.Context, s *server.Server, o *Op) {
	if err := m.dataShell(ctx, s, dropStashScript, nil, nil, io.Discard, false); err != nil {
		o.add("[cubeherd] " + m.t("Warnung: %s", err.Error()))
	}
}

// rollbackSnapshot returns the software version of archive name if the server can be set back to it. The local
// index is preferred; without an entry the local archive is asked.
func (m *Manager) rollbackSnapshot(ctx context.Context, s *server.Server, name string) (*server.Snapshot, error) {
	sn := m.loadMeta(s.ID)[name]
	if sn == nil {
		sn, _ = m.readSnapshot(ctx, s, m.BackupDir(s), name)
	}
	if !s.CanRollback(sn) {
		return nil, errors.New(m.t("Die Version aus diesem Backup lässt sich nicht wiederherstellen"))
	}
	return sn, nil
}

// installPack lets the image install the modpack version of s without starting the server. itzg replaces the mods
// of the installed version and overwrites the pack's configuration files, so it runs before the backup is extracted.
// It uses its own compose file: the stored definition only changes once the whole restore has worked.
func (m *Manager) installPack(ctx context.Context, s *server.Server, o *Op) error {
	o.add("[cubeherd] " + m.t("Installiere %s", s.Snapshot().Label()))
	file := filepath.Join(m.Store.Dir(s.ID), "compose.setup.yml")
	if err := os.WriteFile(file, []byte(server.Compose(s, m.globals())), 0o600); err != nil {
		return err
	}
	defer os.Remove(file)
	return m.composeFile(ctx, s, o, file, "run", "--rm", "--no-deps", "-T", "-e", "SETUP_ONLY=true", "mc")
}

// Restore replaces the world and configuration with a backup. The current state is saved first and brought back
// if the restore fails. With rollback the server also goes back to the software version of the backup.
func (m *Manager) Restore(id, name string, rollback bool) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	_, localErr := m.BackupPath(s, name)
	if localErr != nil && !m.hasRemoteBackup(s, name) {
		return errors.New(m.t("Backup nicht gefunden"))
	}
	if rollback {
		if err := m.mayStart(s); err != nil {
			return err
		}
	}
	return m.run(id, server.StatusRestoring, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		o.pinned = name // retention must not delete the archive that is being restored
		if localErr != nil {
			if err := m.fetch(ctx, s, o, name); err != nil {
				return fmt.Errorf(m.t("Download aus dem externen Speicher fehlgeschlagen: %w"), err)
			}
			if s.Plan().RemoteOnly {
				defer os.Remove(filepath.Join(m.BackupDir(s), name)) // fetched only for this restore
			}
		}
		// Everything that can rule the restore out is checked before the server is touched.
		o.add("[cubeherd] " + m.t("Prüfe %s", name))
		err := m.verifyArchive(ctx, s, m.BackupDir(s), name)
		if ctx.Err() == nil {
			m.recordCheck(id, name, err)
		}
		if err != nil {
			return fmt.Errorf(m.t("Archiv beschädigt, nichts wurde verändert: %w"), err)
		}
		var target *server.Server
		var sn *server.Snapshot
		if rollback {
			if sn, err = m.rollbackSnapshot(ctx, s, name); err != nil {
				return err
			}
			target = s.Rollback(sn)
		}
		c := m.inspect(ctx, s)
		wasUp := c != nil && c.State.Running
		if wasUp {
			o.add("[cubeherd] " + m.t("Server wird gestoppt"))
			if err := m.Docker.Stop(ctx, s.Container(), time.Minute); err != nil {
				return err
			}
		}
		safety, err := m.backup(ctx, s, o, ReasonRestore, m.BackupDir(s), "")
		if err != nil {
			return fmt.Errorf(m.t("Sicherung des aktuellen Stands fehlgeschlagen, Wiederherstellung abgebrochen: %w"), err)
		}
		install := target != nil && target.Pack != nil
		mods := ""
		if install {
			mods = "1"
		}
		if err := m.dataShell(ctx, s, stashScript, map[string]string{"MODS": mods}, nil, io.Discard, false); err != nil {
			m.dropStash(ctx, s, o)
			return fmt.Errorf(m.t("Sicherung des aktuellen Stands fehlgeschlagen, Wiederherstellung abgebrochen: %w"), err)
		}
		if install {
			if err := m.installPack(ctx, target, o); err != nil {
				return m.revert(s, o, safety.Name, err)
			}
		}
		files := s
		if target != nil {
			files = target
		}
		o.add("[cubeherd] " + m.t("Stelle %s wieder her", name))
		if err := m.extract(ctx, files, name); err != nil {
			return m.revert(s, o, safety.Name, err)
		}
		// Only the software changes; whatever was saved while the restore ran (e.g. a suspension) stays.
		cur, err := m.Store.Update(id, m.globals(), func(d *server.Server) {
			if sn != nil {
				d.ApplyRollback(sn)
			}
		})
		if err != nil {
			return m.revert(s, o, safety.Name, err)
		}
		m.dropStash(ctx, cur, o)
		if sn != nil {
			m.event(id, "update", "")
		}
		o.add("[cubeherd] " + m.t("Wiederherstellung abgeschlossen"))
		if wasUp && !cur.Suspended {
			return m.compose(ctx, cur, o, "up", "-d")
		}
		if sn != nil && c != nil {
			// Replace the stopped container, so nothing can start it with the newer version.
			return m.compose(ctx, cur, o, "up", "--no-start")
		}
		return nil
	})
}
