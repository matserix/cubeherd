package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/server"
)

// The disk quota. A server may have a limit for its data folder. Docker cannot enforce one on a bind mount, so
// Cubeherd measures and acts in steps: from 90 % the dashboard warns, from 100 % nothing more can be written through
// Cubeherd (uploads, file saves, world import) and the server cannot be started, from 120 % a running server is
// stopped. Data is never deleted.

const (
	quotaWarn = 90
	quotaFull = 100
	quotaOver = 120

	quotaEvery = time.Minute
)

// quotaUnit is the size of one GB of quota; tests make it small.
var quotaUnit = int64(1 << 30)

type quotaEntry struct {
	at   time.Time
	used int64
}

type quotaState struct {
	mu      sync.Mutex
	entries map[string]quotaEntry
	acted   map[string]bool // stopped for being over the limit; cleared once the server is below it again
}

func quotaLevel(used, limit int64) string {
	if limit <= 0 {
		return ""
	}
	switch pct := used * 100 / limit; {
	case pct >= quotaOver:
		return api.QuotaOver
	case pct >= quotaFull:
		return api.QuotaFull
	case pct >= quotaWarn:
		return api.QuotaWarn
	}
	return api.QuotaOK
}

func measure(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// QuotaOf describes the limit of a server and how much of it is used; nil without a limit.
func (m *Manager) QuotaOf(s *server.Server) *api.Quota {
	if s.DiskGB <= 0 {
		return nil
	}
	limit := int64(s.DiskGB) * quotaUnit
	m.qs.mu.Lock()
	e, ok := m.qs.entries[s.ID]
	m.qs.mu.Unlock()
	if !ok {
		e.used = m.DiskUsage(s) // the first answer may be an estimate, the watchdog replaces it within a minute
	}
	return &api.Quota{Used: e.used, Limit: limit, Level: quotaLevel(e.used, limit)}
}

// measureQuota refreshes the stored size of one server.
func (m *Manager) measureQuota(s *server.Server) int64 {
	dir := m.Store.DataDir(s)
	if dir == "" {
		return m.DiskUsage(s) // a Docker volume cannot be walked from here
	}
	used := measure(dir)
	m.qs.mu.Lock()
	if m.qs.entries == nil {
		m.qs.entries = map[string]quotaEntry{}
	}
	m.qs.entries[s.ID] = quotaEntry{at: time.Now(), used: used}
	m.qs.mu.Unlock()
	return used
}

// ErrQuota is returned when a write or start is refused because the data folder is full.
var ErrQuota = errors.New("disk quota reached")

func (m *Manager) quotaMsg(q *api.Quota) error {
	return fmt.Errorf("%w: "+m.t("Der Speicherplatz dieses Servers ist belegt (%s von %s). Lösche Dateien, um wieder schreiben oder starten zu können."), ErrQuota, byteSize(q.Used), byteSize(q.Limit))
}

func byteSize(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }

// checkWritable refuses writes once the quota is reached.
func (m *Manager) checkWritable(s *server.Server) error {
	if q := m.QuotaOf(s); q != nil && (q.Level == api.QuotaFull || q.Level == api.QuotaOver) {
		return m.quotaMsg(q)
	}
	return nil
}

// quotaReader stops an upload that would take the folder past the limit.
type quotaReader struct {
	r    io.Reader
	left int64
	err  error
}

func (q *quotaReader) Read(p []byte) (int, error) {
	if q.left <= 0 {
		return 0, q.err
	}
	if int64(len(p)) > q.left {
		p = p[:q.left]
	}
	n, err := q.r.Read(p)
	q.left -= int64(n)
	return n, err
}

// limitedBy caps a reader at the room that is left below the quota; without a quota it returns r as it is.
func (m *Manager) limitedBy(s *server.Server, r io.Reader) (io.Reader, error) {
	q := m.QuotaOf(s)
	if q == nil {
		return r, nil
	}
	if err := m.checkWritable(s); err != nil {
		return nil, err
	}
	return &quotaReader{r: r, left: q.Limit - q.Used, err: m.quotaMsg(q)}, nil
}

// RunQuota is the watchdog: once a minute it measures every server with a limit and stops a running one that is
// over by 20 %.
func (m *Manager) RunQuota(ctx context.Context) {
	t := time.NewTicker(quotaEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.checkQuotas(ctx)
		}
	}
}

func (m *Manager) checkQuotas(ctx context.Context) {
	list, err := m.Store.List()
	if err != nil {
		return
	}
	for _, s := range list {
		if s.DiskGB <= 0 {
			continue
		}
		limit := int64(s.DiskGB) * quotaUnit
		used := m.measureQuota(s)
		lvl := quotaLevel(used, limit)
		m.qs.mu.Lock()
		if m.qs.acted == nil {
			m.qs.acted = map[string]bool{}
		}
		done := m.qs.acted[s.ID]
		if lvl != api.QuotaOver {
			delete(m.qs.acted, s.ID)
		}
		m.qs.mu.Unlock()
		if lvl != api.QuotaOver || done {
			continue
		}
		if m.View(ctx, s, false).Status.Live() {
			m.event(s.ID, "quota", byteSize(used)+" / "+byteSize(limit))
			if err := m.Stop(s.ID); err == nil {
				m.qs.mu.Lock()
				m.qs.acted[s.ID] = true
				m.qs.mu.Unlock()
			}
		}
	}
}

// ErrSuspended is returned for a server that its host has suspended.
var ErrSuspended = errors.New("server suspended")

// mayStart refuses what would bring a suspended or full server up.
func (m *Manager) mayStart(s *server.Server) error {
	if s.Suspended {
		return fmt.Errorf("%w: %s", ErrSuspended, m.t("Dieser Server wurde vom Betreiber gesperrt."))
	}
	return m.checkWritable(s)
}
