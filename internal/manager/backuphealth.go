package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"cubeherd/internal/server"
)

// Check is the result of reading an archive back once (zstd + tar listing).
type Check struct {
	At  time.Time `json:"at"`
	OK  bool      `json:"ok"`
	Err string    `json:"err,omitempty"`
}

// healthState is kept in servers/<id>/backup-health.json. It only holds small facts (no archive data).
type healthState struct {
	Checks      map[string]Check `json:"checks,omitempty"`
	LastOK      time.Time        `json:"lastOk,omitzero"`   // newest backup that was made, checked and uploaded as configured
	LastFail    time.Time        `json:"lastFail,omitzero"` // newest failure of a backup attempt
	FailMsg     string           `json:"failMsg,omitempty"`
	FailCorrupt bool             `json:"failCorrupt,omitempty"` // the failure was a broken archive
}

// errCorrupt marks a verification failure that was already recorded.
var errCorrupt = errors.New("archive failed verification")

type corruptError struct{ msg string }

func (e *corruptError) Error() string { return e.msg }
func (e *corruptError) Unwrap() error { return errCorrupt }

func (m *Manager) healthPath(id string) string {
	return filepath.Join(m.Store.Dir(id), "backup-health.json")
}

func (m *Manager) loadHealth(id string) healthState {
	var st healthState
	if b, err := os.ReadFile(m.healthPath(id)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// updateHealth changes the stored state under a lock; old check results are dropped after 90 days.
func (m *Manager) updateHealth(id string, f func(*healthState)) {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	st := m.loadHealth(id)
	if st.Checks == nil {
		st.Checks = map[string]Check{}
	}
	f(&st)
	for n, c := range st.Checks {
		if time.Since(c.At) > 90*24*time.Hour {
			delete(st.Checks, n)
		}
	}
	if b, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(m.healthPath(id)), 0o755)
		_ = os.WriteFile(m.healthPath(id), b, 0o644)
	}
}

func (m *Manager) recordOK(id string) {
	m.updateHealth(id, func(st *healthState) { st.LastOK = time.Now() })
}

func (m *Manager) recordCheck(id, name string, err error) {
	m.updateHealth(id, func(st *healthState) {
		c := Check{At: time.Now(), OK: err == nil}
		if err != nil {
			c.Err = err.Error()
		}
		st.Checks[name] = c
	})
}

func (m *Manager) recordFail(id, msg string, corrupt bool) {
	m.updateHealth(id, func(st *healthState) {
		st.LastFail, st.FailMsg, st.FailCorrupt = time.Now(), msg, corrupt
	})
}

// verifyScript reads the whole archive without extracting it.
const verifyScript = `set -e
nice -n 19 ionice -c3 tar -I zstd -tf "/b/$F" >/dev/null`

// verifyArchive checks that a local archive can be read completely. It runs in a short-lived container of the
// server's image (which ships tar and zstd), like the restore does.
func (m *Manager) verifyArchive(ctx context.Context, s *server.Server, dir, name string) error {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-e", "F="+name, "-v", dir+":/b:ro",
		"--entrypoint", "sh", s.ImageRef(), "-c", verifyScript)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = nil, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New(firstLine(stderr.String(), err))
	}
	return nil
}

// VerifyBackup re-checks one archive in the background. Archives that only exist in the external storage
// are downloaded for the check and removed again.
func (m *Manager) VerifyBackup(id, name string) error {
	s, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	if !backupName.MatchString(name) {
		return errors.New(m.t("Backup nicht gefunden"))
	}
	_, localErr := m.BackupPath(s, name)
	if localErr != nil && !m.hasRemoteBackup(s, name) {
		return errors.New(m.t("Backup nicht gefunden"))
	}
	return m.run(id, server.StatusBackingUp, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if localErr != nil {
			if err := m.fetch(ctx, s, o, name); err != nil {
				return fmt.Errorf(m.t("Download aus dem externen Speicher fehlgeschlagen: %w"), err)
			}
			defer os.Remove(filepath.Join(m.BackupDir(s), name))
		}
		o.add("[cubeherd] " + m.t("Prüfe %s", name))
		err := m.verifyArchive(ctx, s, m.BackupDir(s), name)
		m.recordCheck(id, name, err)
		if err != nil {
			// Only the newest archive decides the health banner; an old broken one just gets its row marked.
			if list, _ := m.BackupsAll(ctx, s); len(list) > 0 && list[0].Name == name {
				// The newest archive is no longer a good one: fall back to the next older archive that is not known to be broken.
				var prev time.Time
				for _, b := range list[1:] {
					if b.Check != "bad" {
						prev = b.Time
						break
					}
				}
				m.updateHealth(id, func(st *healthState) {
					st.LastOK, st.LastFail, st.FailMsg, st.FailCorrupt = prev, time.Now(), err.Error(), true
				})
			}
			return fmt.Errorf(m.t("Archiv beschädigt: %s"), err.Error())
		}
		o.add("[cubeherd] " + m.t("Archiv ist in Ordnung"))
		if list, _ := m.BackupsAll(ctx, s); len(list) > 0 && list[0].Name == name {
			m.recordOK(id) // the newest archive is readable, which clears an earlier "damaged" report
		}
		return nil
	})
}

// Health is what the Backups tab shows above the list.
type Health struct {
	Level  string // ok | warn | crit | off
	Title  string
	Detail string
}

// warnAfter is how old the last good backup may get before a warning is shown.
func warnAfter(p server.BackupPlan) time.Duration {
	step := map[string]time.Duration{"6h": 6 * time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour}[p.Interval]
	switch p.WarnAfter {
	case "25h":
		return 25 * time.Hour
	case "3d":
		return 72 * time.Hour
	}
	if step == 0 {
		return 0 // no schedule, so nothing can be overdue
	}
	return 2*step + time.Hour
}

func (m *Manager) stamp(t time.Time) string { return t.In(m.Loc()).Format("2006-01-02 15:04") }

func (m *Manager) ago(d time.Duration) string {
	if d < 48*time.Hour {
		return m.t("%d Stunden", max(1, int(d.Hours())))
	}
	return m.t("%d Tagen", int(d.Hours()/24))
}

// BackupHealth rates the backups of a server. running tells whether the server runs (stopped servers skip
// scheduled backups when "only if running" is set, which must not count as a missed run).
func (m *Manager) BackupHealth(s *server.Server, running bool) Health {
	plan := s.Plan()
	if plan.NoWarn {
		return Health{Level: "off"}
	}
	st := m.loadHealth(s.ID)
	lastOK := st.LastOK
	if lastOK.IsZero() && st.LastFail.IsZero() && len(st.Checks) == 0 { // archives from before this feature count as good
		if list, _ := m.Backups(s); len(list) > 0 {
			lastOK = list[0].Time
		}
	}
	now := time.Now()
	good := m.t("Es gibt noch kein erfolgreiches Backup.")
	if !lastOK.IsZero() {
		good = m.t("Das letzte gute Backup ist vom %s.", m.stamp(lastOK))
	}
	failed := st.LastFail.After(lastOK)
	if failed && st.FailCorrupt {
		return Health{"crit", m.t("Das letzte Backup ist beschädigt"),
			m.t("Die Prüfung ist fehlgeschlagen: %s. %s Lege jetzt ein neues an.", st.FailMsg, good)}
	}
	ref := lastOK
	if ref.Before(s.CreatedAt) {
		ref = s.CreatedAt
	}
	limit := warnAfter(plan)
	overdue := limit > 0 && !ref.IsZero() && now.Sub(ref) > limit && (running || !plan.OnlyIfActive)
	if failed || overdue {
		title := m.t("Noch kein erfolgreiches Backup")
		if !lastOK.IsZero() {
			title = m.t("Seit %s kein erfolgreiches Backup", m.ago(now.Sub(lastOK)))
		}
		detail := m.t("Der Zeitplan hätte ein Backup erstellen sollen. Prüfe, ob Cubeherd lief und der Server aktiv war. %s", good)
		if failed {
			detail = m.t("Der letzte Versuch ist fehlgeschlagen: %s. %s", st.FailMsg, good)
		}
		return Health{"warn", title, detail}
	}
	if lastOK.IsZero() {
		return Health{"ok", m.t("Noch kein Backup"), m.t("Das erste geplante Backup folgt nach dem Zeitplan. „Jetzt sichern“ erstellt es sofort.")}
	}
	extra := ""
	if !plan.NoVerify {
		extra += " " + m.t("Geprüft.")
	}
	if plan.Remote {
		extra += " " + m.t("Extern gespeichert.")
	}
	return Health{"ok", m.t("Backups sind in Ordnung"), m.t("Letztes erfolgreiches Backup: %s.", m.stamp(lastOK)) + extra}
}

// annotate fills the verification result of each archive from the stored state.
func (m *Manager) annotate(s *server.Server, list []Backup) {
	st := m.loadHealth(s.ID)
	for i := range list {
		if c, ok := st.Checks[list[i].Name]; ok {
			list[i].Check, list[i].CheckErr = "ok", ""
			if !c.OK {
				list[i].Check, list[i].CheckErr = "bad", c.Err
			}
		}
	}
}
