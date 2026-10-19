package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cubeherd/internal/config"
	"cubeherd/internal/server"
)

func healthFixture(t *testing.T, plan server.BackupPlan, created time.Time) (*Manager, *server.Server) {
	dir := t.TempDir()
	st, err := server.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(nil, st, cfg, nil), &server.Server{ID: "srv", Name: "Srv", CreatedAt: created, Backup: &plan}
}

func daily() server.BackupPlan {
	return server.BackupPlan{Interval: "daily", Time: "04:00", Keep: 7, OnlyIfActive: true}
}

func TestBackupHealthLevels(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		plan    func(*server.BackupPlan)
		created time.Time
		state   func(*healthState)
		running bool
		want    string
		title   string // substring
	}{
		{"fresh server has nothing to warn about", nil, now, nil, true, "ok", "Noch kein Backup"},
		{"never backed up for days", nil, now.Add(-5 * 24 * time.Hour), nil, true, "warn", "Noch kein erfolgreiches"},
		{"stopped server skips runs", nil, now.Add(-5 * 24 * time.Hour), nil, false, "ok", "Noch kein Backup"},
		{"recent backup is fine", nil, now.Add(-9 * 24 * time.Hour), func(s *healthState) { s.LastOK = now.Add(-3 * time.Hour) }, true, "ok", "in Ordnung"},
		{"three days without a backup", nil, now.Add(-9 * 24 * time.Hour), func(s *healthState) { s.LastOK = now.Add(-72 * time.Hour) }, true, "warn", "Seit 3 Tagen"},
		{"just over the limit counts in days", nil, now.Add(-9 * 24 * time.Hour), func(s *healthState) { s.LastOK = now.Add(-50 * time.Hour) }, true, "warn", "Seit 2 Tagen"},
		{"custom threshold tolerates 50 h", func(p *server.BackupPlan) { p.WarnAfter = "3d" }, now.Add(-9 * 24 * time.Hour), func(s *healthState) { s.LastOK = now.Add(-50 * time.Hour) }, true, "ok", "in Ordnung"},
		{"no schedule cannot be overdue", func(p *server.BackupPlan) { p.Interval = "off" }, now.Add(-9 * 24 * time.Hour), func(s *healthState) { s.LastOK = now.Add(-30 * 24 * time.Hour) }, true, "ok", "in Ordnung"},
		{"failed upload warns even when recent", nil, now.Add(-9 * 24 * time.Hour), func(s *healthState) {
			s.LastOK, s.LastFail, s.FailMsg = now.Add(-20*time.Hour), now.Add(-time.Hour), "AccessDenied"
		}, true, "warn", "Seit 20 Stunden"},
		{"damaged archive is critical", nil, now.Add(-9 * 24 * time.Hour), func(s *healthState) {
			s.LastOK, s.LastFail, s.FailMsg, s.FailCorrupt = now.Add(-20*time.Hour), now.Add(-time.Hour), "unexpected end of file", true
		}, true, "crit", "beschädigt"},
		{"a newer good backup clears an old failure", nil, now.Add(-9 * 24 * time.Hour), func(s *healthState) {
			s.LastFail, s.FailMsg, s.FailCorrupt = now.Add(-5*time.Hour), "x", true
			s.LastOK = now.Add(-time.Hour)
		}, true, "ok", "in Ordnung"},
		{"warnings can be switched off", func(p *server.BackupPlan) { p.NoWarn = true }, now.Add(-9 * 24 * time.Hour), nil, true, "off", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := daily()
			if c.plan != nil {
				c.plan(&plan)
			}
			m, s := healthFixture(t, plan, c.created)
			if c.state != nil {
				m.updateHealth(s.ID, c.state)
			}
			h := m.BackupHealth(s, c.running)
			if h.Level != c.want || !strings.Contains(h.Title, c.title) {
				t.Fatalf("%+v, want %s / %q", h, c.want, c.title)
			}
		})
	}
}

func TestBackupHealthDetailMentionsCause(t *testing.T) {
	m, s := healthFixture(t, daily(), time.Now().Add(-9*24*time.Hour))
	m.updateHealth(s.ID, func(st *healthState) {
		st.LastOK, st.LastFail, st.FailMsg = time.Now().Add(-30*time.Hour), time.Now(), "S3 AccessDenied"
	})
	if h := m.BackupHealth(s, true); !strings.Contains(h.Detail, "S3 AccessDenied") || !strings.Contains(h.Detail, "letzte gute Backup") {
		t.Fatalf("%+v", h)
	}
}

func TestRecordAndAnnotate(t *testing.T) {
	m, s := healthFixture(t, daily(), time.Now())
	m.recordCheck(s.ID, "20260101-040000_manual.tar.zst", nil)
	m.recordCheck(s.ID, "20260102-040000_manual.tar.zst", errors.New("unexpected end"))
	list := []Backup{{Name: "20260101-040000_manual.tar.zst"}, {Name: "20260102-040000_manual.tar.zst"}, {Name: "20260103-040000_manual.tar.zst"}}
	m.annotate(s, list)
	if list[0].Check != "ok" || list[1].Check != "bad" || list[1].CheckErr != "unexpected end" || list[2].Check != "" {
		t.Fatalf("%+v", list)
	}
	m.recordFail(s.ID, "boom", true)
	if st := m.loadHealth(s.ID); st.FailMsg != "boom" || !st.FailCorrupt || st.LastFail.IsZero() {
		t.Fatalf("%+v", st)
	}
	m.recordOK(s.ID)
	if st := m.loadHealth(s.ID); st.LastOK.IsZero() || len(st.Checks) != 2 {
		t.Fatalf("%+v", st)
	}
}

func TestCorruptErrorIsRecognised(t *testing.T) {
	var err error = &corruptError{"x"}
	if !errors.Is(err, errCorrupt) {
		t.Fatal("corruptError must match errCorrupt")
	}
}

func TestLegacyArchivesCountAsGoodOnlyWithoutHistory(t *testing.T) {
	m, s := healthFixture(t, daily(), time.Now().Add(-9*24*time.Hour))
	if err := os.MkdirAll(m.BackupDir(s), 0o755); err != nil {
		t.Fatal(err)
	}
	name := time.Now().Add(-time.Hour).Format("20060102-150405") + "_manual.tar.zst"
	if err := os.WriteFile(filepath.Join(m.BackupDir(s), name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := m.BackupHealth(s, true); h.Level != "ok" || !strings.Contains(h.Detail, "Letztes erfolgreiches Backup") {
		t.Fatalf("legacy archive should count: %+v", h)
	}
	// Once that archive failed its check, it must not be reported as the last good one.
	m.recordCheck(s.ID, name, errors.New("premature end"))
	m.recordFail(s.ID, "premature end", true)
	if h := m.BackupHealth(s, true); h.Level != "crit" || !strings.Contains(h.Detail, "noch kein erfolgreiches Backup") {
		t.Fatalf("broken archive must not count: %+v", h)
	}
}
