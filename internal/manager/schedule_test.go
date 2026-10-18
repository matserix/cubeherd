package manager

import (
	"testing"
	"time"

	"forgecraft/internal/server"
)

func TestNextBackup(t *testing.T) {
	loc := time.Local
	at := func(s string) time.Time { tt, _ := time.ParseInLocation("2006-01-02 15:04", s, loc); return tt }
	daily := server.BackupPlan{Interval: "daily", Time: "04:00"}
	cases := []struct {
		name      string
		plan      server.BackupPlan
		last, now string
		want      string
	}{
		{"daily, last today", daily, "2026-10-18 04:00", "2026-10-18 12:00", "2026-10-19 04:00"},
		{"daily, no backup yet", daily, "", "2026-10-18 12:00", "2026-10-19 04:00"},
		{"daily, before time", daily, "", "2026-10-18 03:00", "2026-10-18 04:00"},
		{"daily, missed", daily, "2026-10-15 04:00", "2026-10-18 12:00", "2026-10-18 12:02"},
		{"6h", server.BackupPlan{Interval: "6h", Time: "04:00"}, "2026-10-18 10:00", "2026-10-18 12:00", "2026-10-18 16:00"},
		{"weekly → Monday", server.BackupPlan{Interval: "weekly", Time: "03:30"}, "", "2026-10-18 12:00", "2026-10-19 03:30"},
	}
	for _, c := range cases {
		var last time.Time
		if c.last != "" {
			last = at(c.last)
		}
		got := NextBackup(c.plan, last, at(c.now))
		if !got.Equal(at(c.want)) {
			t.Errorf("%s: got %s want %s", c.name, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	if !NextBackup(server.BackupPlan{Interval: "off"}, time.Time{}, time.Now()).IsZero() {
		t.Error("off must not schedule")
	}
}
