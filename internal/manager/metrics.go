package manager

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/docker"
	"cubeherd/internal/metrics"
	"cubeherd/internal/server"
)

// The statistics recorder: while it is on, one goroutine samples the host and every running server once a minute and
// hands the rows to metrics.Store, which writes them in batches. While it is off nothing here runs.

const (
	sampleEvery = time.Minute
	flushEvery  = 5 * time.Minute
	sampleLimit = 4 // servers sampled at the same time
)

type recorderState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	cfg    api.MetricsConfig

	counters map[string]serverCounters
	status   map[string]server.Status
}

type serverCounters struct {
	c       docker.Counters
	started time.Time
}

// metricsStore returns the open store, or nil when there is none (never enabled and no file).
func (m *Manager) metricsStore() *metrics.Store { return m.ms.Load() }

// openMetrics opens metrics.db if the node has a data directory for it. With create false an absent file stays absent.
func (m *Manager) openMetrics(create bool, cfg api.MetricsConfig) (*metrics.Store, error) {
	if st := m.ms.Load(); st != nil {
		st.SetConfig(metrics.ConfigFrom(cfg))
		return st, nil
	}
	if m.MetricsDir == "" {
		return nil, errors.New("this node has no place for statistics")
	}
	if _, err := os.Stat(filepath.Join(m.MetricsDir, "metrics.db")); err != nil && !create {
		return nil, nil
	}
	m.msMu.Lock()
	defer m.msMu.Unlock()
	if st := m.ms.Load(); st != nil {
		return st, nil
	}
	st, err := metrics.Open(m.MetricsDir, metrics.ConfigFrom(cfg))
	if err != nil {
		return nil, err
	}
	m.ms.Store(st)
	return st, nil
}

// ApplyMetrics starts, reconfigures or stops the recorder. Stopping keeps the data.
func (m *Manager) ApplyMetrics(c api.MetricsConfig) error {
	m.rec.mu.Lock()
	defer m.rec.mu.Unlock()
	m.rec.cfg = c
	if !c.Enabled {
		if m.rec.cancel != nil {
			m.rec.cancel()
			<-m.rec.done
			m.rec.cancel = nil
		}
		if st := m.ms.Load(); st != nil {
			st.SetConfig(metrics.ConfigFrom(c))
		}
		return nil
	}
	st, err := m.openMetrics(true, c)
	if err != nil {
		return err
	}
	if m.rec.cancel != nil {
		return nil // already running; the retention was updated above
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.rec.cancel, m.rec.done = cancel, make(chan struct{})
	m.rec.counters, m.rec.status = map[string]serverCounters{}, map[string]server.Status{}
	go m.runRecorder(ctx, st, m.rec.done)
	return nil
}

// CloseMetrics stops the recorder and flushes what is buffered; call it on shutdown.
func (m *Manager) CloseMetrics() {
	_ = m.ApplyMetrics(api.MetricsConfig{})
	if st := m.ms.Swap(nil); st != nil {
		_ = st.Close()
	}
}

func (m *Manager) runRecorder(ctx context.Context, st *metrics.Store, done chan struct{}) {
	defer close(done)
	defer func() { _ = st.Flush() }()
	var lastFlush, lastMaintain time.Time
	for {
		next := time.Now().Truncate(sampleEvery).Add(sampleEvery)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		m.sample(ctx, st, next.Unix())
		if time.Since(lastFlush) >= flushEvery {
			if err := st.Flush(); err != nil {
				log.Printf("statistics: %v", err)
			}
			lastFlush = time.Now()
		}
		if time.Since(lastMaintain) >= time.Hour {
			if err := st.Maintain(); err != nil {
				log.Printf("statistics: %v", err)
			}
			lastMaintain = time.Now()
		}
	}
}

// sample takes one reading of the node and of every server and buffers it.
func (m *Manager) sample(ctx context.Context, st *metrics.Store, ts int64) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	views, err := m.Views(ctx, false)
	if err != nil {
		return
	}
	var (
		mu     sync.Mutex
		rows   []metrics.Row
		wg     sync.WaitGroup
		sem    = make(chan struct{}, sampleLimit)
		up     float64
		people float64
	)
	for _, v := range views {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p := m.sampleServer(ctx, st, v, ts)
			mu.Lock()
			defer mu.Unlock()
			rows = append(rows, metrics.Row{Scope: v.S.ID, P: p})
			if p.Up != nil && *p.Up > 0 {
				up++
			}
			if p.Players != nil {
				people += *p.Players
			}
		}()
	}
	wg.Wait()

	node := api.MetricPoint{T: ts, Up: api.Float(up), Players: api.Float(people)}
	if m.Hoster != nil {
		h := m.Hoster.Read(ctx)
		if h.CPUPercent >= 0 {
			node.CPU = api.Float(h.CPUPercent)
		}
		if h.MemUsed > 0 {
			node.Mem = api.Float(float64(h.MemUsed))
		}
		if h.DiskTotal > 0 {
			node.Disk = api.Float(float64(h.DiskUsed))
		}
	}
	rows = append(rows, metrics.Row{Scope: "node", P: node})
	st.Add(rows...)

	// forget servers that are gone
	m.rec.mu.Lock()
	seen := map[string]bool{}
	for _, v := range views {
		seen[v.S.ID] = true
	}
	for id := range m.rec.counters {
		if !seen[id] {
			delete(m.rec.counters, id)
		}
	}
	for id := range m.rec.status {
		if !seen[id] {
			delete(m.rec.status, id)
		}
	}
	m.rec.mu.Unlock()
}

// sampleServer reads one server. A server that is not really running only gets its up flag, so autopause and
// autostop behave as without the recorder; RCON is used for running servers only.
func (m *Manager) sampleServer(ctx context.Context, st *metrics.Store, v View, ts int64) api.MetricPoint {
	p := api.MetricPoint{T: ts, Up: api.Float(0)}
	if v.Status.Up() {
		p.Up = api.Float(1)
	}
	m.rec.mu.Lock()
	prev, hadPrev := m.rec.status[v.S.ID]
	m.rec.status[v.S.ID] = v.Status
	m.rec.mu.Unlock()
	if v.Status == server.StatusCrashed && (!hadPrev || prev != server.StatusCrashed) {
		st.Event(v.S.ID, "crash", shortLine(v.CrashReason, 200))
	}
	if !v.Status.Up() || v.C == nil || !v.C.State.Running {
		return p
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	cur, err := m.Docker.CountersOnce(cctx, v.S.Container())
	cancel()
	if err == nil {
		p.Mem = api.Float(float64(cur.MemUsed))
		m.rec.mu.Lock()
		old, ok := m.rec.counters[v.S.ID]
		m.rec.counters[v.S.ID] = serverCounters{cur, v.C.State.StartedAt}
		m.rec.mu.Unlock()
		if ok && old.started.Equal(v.C.State.StartedAt) {
			if pct, ok := docker.CPUShare(old.c, cur); ok {
				p.CPU = api.Float(pct)
			}
		}
	}
	if !v.Status.Live() {
		return p
	}
	if names, _, err := m.Players(ctx, v.S); err == nil {
		p.Players = api.Float(float64(len(names)))
	}
	if len(server.PerfCommands(v.S.Type)) > 0 {
		if s, err := m.Perf(ctx, v.S); err == nil {
			p.TPS, p.MSPT = api.Float(s.TPS), api.Float(s.MSPT)
		}
	}
	return p
}

func shortLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// event notes something that happened to a server, if the recorder is on.
func (m *Manager) event(id, kind, detail string) {
	m.rec.mu.Lock()
	on := m.rec.cancel != nil
	m.rec.mu.Unlock()
	if st := m.ms.Load(); on && st != nil {
		st.Event(id, kind, detail)
	}
}

// ---------------- Reading ----------------

// MetricsStatus describes the recorder.
func (m *Manager) MetricsStatus(ctx context.Context) (api.MetricsStatus, error) {
	m.rec.mu.Lock()
	cfg, on := m.rec.cfg, m.rec.cancel != nil
	m.rec.mu.Unlock()
	out := api.MetricsStatus{Enabled: on, Config: cfg}
	st, err := m.openMetrics(false, cfg)
	if err != nil || st == nil {
		return out, nil
	}
	out.Size = st.Size()
	out.Since, out.Last = st.Range()
	out.Capped = st.Capped()
	return out, nil
}

// NodeHistory is the node's series; without any data it is empty, not an error.
func (m *Manager) NodeHistory(ctx context.Context, from, to time.Time, points int) (api.History, error) {
	return m.history("node", from, to, points)
}

// ServerHistory is one server's series and events.
func (m *Manager) ServerHistory(ctx context.Context, id string, from, to time.Time, points int) (api.History, error) {
	return m.history(id, from, to, points)
}

func (m *Manager) history(scope string, from, to time.Time, points int) (api.History, error) {
	m.rec.mu.Lock()
	cfg, on := m.rec.cfg, m.rec.cancel != nil
	m.rec.mu.Unlock()
	st, err := m.openMetrics(false, cfg)
	if err != nil || st == nil {
		return api.History{Scope: scope, From: from.Unix(), To: to.Unix(), Enabled: on}, err
	}
	h, err := st.Query(scope, from, to, points)
	h.Enabled = on
	return h, err
}

// MetricsLatest is the newest sample of the node and each server, for the Prometheus endpoint.
func (m *Manager) MetricsLatest(ctx context.Context) (map[string]api.MetricPoint, error) {
	st := m.metricsStore()
	if st == nil {
		return nil, nil
	}
	return st.Latest()
}

// PurgeMetrics deletes the history of one server (or "node"), or of everything when scope is "".
func (m *Manager) PurgeMetrics(ctx context.Context, scope string) error {
	m.rec.mu.Lock()
	cfg := m.rec.cfg
	m.rec.mu.Unlock()
	st, err := m.openMetrics(false, cfg)
	if err != nil || st == nil {
		return err
	}
	return st.Purge(scope)
}
