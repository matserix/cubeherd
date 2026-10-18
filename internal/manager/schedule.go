package manager

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"forgecraft/internal/server"
)

// catchUpDelay delays backups that were missed (e.g. while Forgecraft was down) after a restart.
const catchUpDelay = 2 * time.Minute

// NextBackup returns the next scheduled run after last (zero if the plan is off).
// Runs that are already overdue are moved to now+catchUpDelay.
func NextBackup(p server.BackupPlan, last, now time.Time) time.Time {
	step := map[string]time.Duration{"6h": 6 * time.Hour, "daily": 24 * time.Hour, "weekly": 24 * time.Hour}[p.Interval]
	if step == 0 {
		return time.Time{}
	}
	h, mi := parseClock(p.Time)
	from := last
	if from.IsZero() {
		from = now
	}
	// Walk occurrences from a week before "from" until the first one after it.
	y, mo, d := from.AddDate(0, 0, -7).Date()
	t := time.Date(y, mo, d, h, mi, 0, 0, from.Location())
	for !t.After(from) || (p.Interval == "weekly" && t.Weekday() != time.Monday) {
		t = t.Add(step)
	}
	if t.Before(now) {
		return now.Add(catchUpDelay)
	}
	return t
}

func parseClock(s string) (int, int) {
	hs, ms, _ := strings.Cut(s, ":")
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 4, 0
	}
	return h, m
}

// WakeScheduler recomputes the next backup time after a plan or server change.
func (m *Manager) WakeScheduler() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// lastScheduled is the newest scheduled backup or skipped attempt.
func (m *Manager) lastScheduled(s *server.Server) time.Time {
	var last time.Time
	if list, err := m.Backups(s); err == nil {
		for _, b := range list {
			if b.Reason == ReasonSchedule {
				last = b.Time
				break
			}
		}
	}
	m.mu.Lock()
	if t := m.attempts[s.ID]; t.After(last) {
		last = t
	}
	m.mu.Unlock()
	return last
}

// NextBackupFor is shown in the UI.
func (m *Manager) NextBackupFor(s *server.Server) time.Time {
	loc := m.Loc()
	return NextBackup(s.Plan(), m.lastScheduled(s).In(loc), time.Now().In(loc))
}

// RunScheduler sleeps until the next due backup; it does no periodic polling.
func (m *Manager) RunScheduler(ctx context.Context) {
	for {
		list, _ := m.Store.List()
		loc := m.Loc()
		now := time.Now().In(loc)
		var next time.Time
		var due []*server.Server
		for _, s := range list {
			t := NextBackup(s.Plan(), m.lastScheduled(s).In(loc), now)
			switch {
			case t.IsZero():
			case !t.After(now):
				due = append(due, s)
			case next.IsZero() || t.Before(next):
				next = t
			}
		}
		for _, s := range due {
			m.scheduledBackup(ctx, s)
		}
		if len(due) > 0 {
			continue // recompute after running
		}
		wait := 24 * time.Hour
		if !next.IsZero() {
			wait = time.Until(next)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *Manager) scheduledBackup(ctx context.Context, s *server.Server) {
	m.mu.Lock()
	m.attempts[s.ID] = time.Now()
	m.mu.Unlock()
	v := m.View(ctx, s, false)
	if s.Plan().OnlyIfActive && (!v.Status.Up() || v.Status == server.StatusPaused) {
		log.Printf("%s: geplantes Backup übersprungen (%s)", s.ID, v.Status.Label())
		return
	}
	if err := m.BackupNow(s.ID, ReasonSchedule); err != nil {
		log.Printf("%s: geplantes Backup nicht gestartet: %v", s.ID, err)
		return
	}
	// Run scheduled backups one after another to keep the load low.
	if o := m.Op(s.ID); o != nil {
		for {
			_, ch, done := o.Since(1 << 30)
			if done {
				break
			}
			select {
			case <-ch:
			case <-ctx.Done():
				return
			}
		}
		if o.Err != nil {
			log.Printf("%s: geplantes Backup fehlgeschlagen: %v", s.ID, o.Err)
		}
	}
}
