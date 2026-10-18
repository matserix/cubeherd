package manager

import (
	"testing"
	"time"

	"cubeherd/internal/server"
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
		{"daily, missed", daily, "2026-10-15 04:00", "2026-10-18 12:00", "2026-10-16 04:00"},
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

// A missed run is delayed once after the start, but becomes due afterwards instead of being pushed back forever.
func TestCatchUp(t *testing.T) {
	at := func(s string) time.Time { tt, _ := time.ParseInLocation("2006-01-02 15:04", s, time.Local); return tt }
	started, missed := at("2026-10-18 12:00"), at("2026-10-16 04:00")
	if got := catchUp(missed, started); !got.Equal(at("2026-10-18 12:02")) {
		t.Fatalf("right after the start: %s", got)
	}
	// Later the same overdue run stays in the past, so the scheduler runs it.
	if got := catchUp(missed, started); got.After(at("2026-10-18 12:05")) {
		t.Fatalf("catch-up must not move with the clock: %s", got)
	}
	future := at("2026-10-19 04:00")
	if got := catchUp(future, started); !got.Equal(future) {
		t.Fatalf("future runs stay untouched: %s", got)
	}
	if !catchUp(time.Time{}, started).IsZero() {
		t.Fatal("no plan stays zero")
	}
}
