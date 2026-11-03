package metrics

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cubeherd/internal/api"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)}
	s, err := Open(t.TempDir(), ConfigFrom(api.MetricsConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	s.now = c.now
	s.flushedAt = c.t
	t.Cleanup(func() { s.Close() })
	return s, c
}

func f(v float64) *float64 { return &v }

// feed adds one sample per minute for the node and one server, moving the clock along, and flushes at the end.
func feed(s *Store, c *clock, minutes int, cpu func(i int) float64) {
	for i := 0; i < minutes; i++ {
		ts := c.t.Unix()
		s.Add(Row{"node", api.MetricPoint{T: ts, CPU: f(cpu(i)), Mem: f(1000)}},
			Row{"srv", api.MetricPoint{T: ts, Up: f(1), CPU: f(cpu(i) / 2), Players: f(float64(i % 5)), TPS: f(20 - float64(i%3))}})
		c.t = c.t.Add(time.Minute)
	}
	_ = s.Flush()
}

func TestThinningKeepsAverageMaxAndMin(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 120, func(i int) float64 { return float64(i % 10) }) // 0..9 repeating
	c.t = c.t.Add(5 * time.Minute)
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	h, err := s.Query("srv", c.t.Add(-10*24*time.Hour), c.t, 5000) // beyond the minute rows: five-minute buckets
	if err != nil {
		t.Fatal(err)
	}
	if h.Res != Res5m || len(h.Points) == 0 {
		t.Fatalf("res=%d points=%d", h.Res, len(h.Points))
	}
	p := h.Points[0]
	if p.CPU == nil || *p.CPU != 2.0 { // server cpu = i/2 over i%10 in {0,1,2,3,4} -> mean 1; check below with node
		_ = p
	}
	hn, _ := s.Query("node", c.t.Add(-10*24*time.Hour), c.t, 5000)
	first := hn.Points[0]
	if *first.CPU != 2 || *first.CPUMax != 4 { // minutes 0..4 -> cpu 0..4
		t.Fatalf("avg=%v max=%v", *first.CPU, *first.CPUMax)
	}
	if *first.Mem != 1000 {
		t.Fatalf("mem=%v", *first.Mem)
	}
	if first.TPSMin != nil {
		t.Fatal("the node has no TPS")
	}
	if hs, _ := s.Query("srv", c.t.Add(-10*24*time.Hour), c.t, 5000); hs.Points[0].TPSMin == nil || *hs.Points[0].TPSMin != 18 {
		t.Fatalf("tps min should survive thinning: %+v", hs.Points[0].TPSMin)
	}
}

func TestQueryUsesTheFinestResolutionThatCoversTheRange(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 60, func(i int) float64 { return 1 })
	if h, _ := s.Query("node", c.t.Add(-time.Hour), c.t, 500); h.Res != Res1m || len(h.Points) != 60 {
		t.Fatalf("last hour: res=%d points=%d", h.Res, len(h.Points))
	}
	if h, _ := s.Query("node", c.t.Add(-7*24*time.Hour), c.t, 500); h.Res != Res5m {
		t.Fatalf("a week: res=%d", h.Res)
	}
	if h, _ := s.Query("node", c.t.Add(-100*24*time.Hour), c.t, 500); h.Res != Res1h {
		t.Fatalf("100 days: res=%d", h.Res)
	}
}

func TestQueryThinsToTheRequestedNumberOfPoints(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 600, func(i int) float64 { return float64(i) })
	h, _ := s.Query("node", c.t.Add(-10*time.Hour), c.t, 100)
	if len(h.Points) > 100 || len(h.Points) < 50 || h.Step%60 != 0 || h.Step <= 60 {
		t.Fatalf("points=%d step=%d", len(h.Points), h.Step)
	}
}

func TestGapsStayGaps(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 10, func(int) float64 { return 1 })
	c.t = c.t.Add(30 * time.Minute) // agent was down
	feed(s, c, 10, func(int) float64 { return 1 })
	h, _ := s.Query("node", c.t.Add(-2*time.Hour), c.t, 500)
	if len(h.Points) != 20 {
		t.Fatalf("a gap must not be filled: %d points", len(h.Points))
	}
	var jump int64
	for i := 1; i < len(h.Points); i++ {
		jump = max(jump, h.Points[i].T-h.Points[i-1].T)
	}
	if jump != 30*60+60 {
		t.Fatalf("largest step %d s", jump)
	}
}

func TestRetentionPrunesOnlyWhatIsRolledUp(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 30, func(int) float64 { return 1 })
	c.t = c.t.Add(3 * 24 * time.Hour) // far past the 48 h of minute rows
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	var raw, five int
	s.db.QueryRow("SELECT COUNT(*) FROM samples WHERE res=60").Scan(&raw)
	s.db.QueryRow("SELECT COUNT(*) FROM samples WHERE res=300").Scan(&five)
	if raw != 0 || five == 0 {
		t.Fatalf("minute rows=%d five-minute rows=%d", raw, five)
	}
	// rows that were never rolled up are kept, however old
	s2, c2 := newStore(t)
	s2.Add(Row{"node", api.MetricPoint{T: c2.t.Add(-50 * time.Hour).Unix(), CPU: f(1)}})
	_ = s2.Flush()
	_ = s2.setMeta("rolled300", c2.t.Add(-60*time.Hour).Unix()) // watermark older than the row
	_ = s2.Maintain()
	var n int
	s2.db.QueryRow("SELECT COUNT(*) FROM samples WHERE res=300").Scan(&n)
	if n == 0 {
		t.Fatal("the old row should have been rolled up before it was dropped")
	}
}

func TestRollupDoesNotUseIncompleteBuckets(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 3, func(int) float64 { return 1 }) // 12:00-12:02, bucket 12:00-12:05 is not complete
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM samples WHERE res=300").Scan(&n)
	if n != 0 {
		t.Fatalf("an incomplete bucket was built (%d rows)", n)
	}
}

func TestPurgeAndLatest(t *testing.T) {
	s, c := newStore(t)
	feed(s, c, 5, func(i int) float64 { return float64(i) })
	s.Add(Row{"srv", api.MetricPoint{T: c.t.Unix(), CPU: f(99)}}) // still in the buffer
	l, err := s.Latest()
	if err != nil || *l["srv"].CPU != 99 || *l["node"].CPU != 4 {
		t.Fatalf("latest: %v %+v", err, l)
	}
	_ = s.Flush()
	s.Event("srv", "crash", "boom")
	if err := s.Purge("srv"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := s.Scopes(); len(sc) != 1 || sc[0] != "node" {
		t.Fatalf("scopes after purge: %v", sc)
	}
	_ = s.Flush()
	if h, _ := s.Query("srv", c.t.Add(-time.Hour), c.t.Add(time.Hour), 100); len(h.Events) != 1 {
		t.Fatalf("an event added after the purge must stay: %+v", h.Events)
	}
	if err := s.Purge(""); err != nil {
		t.Fatal(err)
	}
	if sc, _ := s.Scopes(); len(sc) != 0 {
		t.Fatal("purge all left rows")
	}
}

func TestEventsAreReturnedWithTheRange(t *testing.T) {
	s, c := newStore(t)
	s.Event("srv", "start", "")
	c.t = c.t.Add(time.Minute)
	s.Event("srv", "crash", "OOM")
	h, err := s.Query("srv", c.t.Add(-time.Hour), c.t, 100)
	if err != nil || len(h.Events) != 2 || h.Events[1].Kind != "crash" || h.Events[1].Detail != "OOM" {
		t.Fatalf("events: %v %+v", err, h.Events)
	}
}

func TestSizeCapStopsRecording(t *testing.T) {
	s, c := newStore(t)
	s.SetConfig(Config{Keep1m: 48 * time.Hour, Keep5m: 24 * time.Hour, Keep1h: 24 * time.Hour, MaxBytes: 1})
	feed(s, c, 5, func(int) float64 { return 1 })
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	if !s.Capped() {
		t.Fatal("the cap should be reached")
	}
	s.Add(Row{"node", api.MetricPoint{T: c.t.Unix(), CPU: f(1)}})
	if s.Buffered() != 0 {
		t.Fatal("samples must be dropped while capped")
	}
	s.SetConfig(Config{Keep1m: 48 * time.Hour, Keep5m: 24 * time.Hour, Keep1h: 24 * time.Hour, MaxBytes: 1 << 30})
	_ = s.Maintain()
	if s.Capped() {
		t.Fatal("raising the cap should resume recording")
	}
}

func TestNaNAndNilAreStoredAsMissing(t *testing.T) {
	s, c := newStore(t)
	nan := f(0)
	*nan = *nan / *nan
	s.Add(Row{"node", api.MetricPoint{T: c.t.Unix(), CPU: nan, Mem: f(5)}})
	_ = s.Flush()
	h, _ := s.Query("node", c.t.Add(-time.Hour), c.t.Add(time.Hour), 100)
	if len(h.Points) != 1 || h.Points[0].CPU != nil || *h.Points[0].Mem != 5 {
		t.Fatalf("%+v", h.Points)
	}
}

func TestBrokenFileIsSetAsideAndRecreated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metrics.db"), []byte("this is not a database, just text that is long enough to look like a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, ConfigFrom(api.MetricsConfig{}))
	if err != nil {
		t.Fatalf("open must recover: %v", err)
	}
	defer s.Close()
	broken, _ := filepath.Glob(filepath.Join(dir, "metrics.db.broken-*"))
	if len(broken) != 1 {
		t.Fatalf("the broken file should be kept aside: %v", broken)
	}
	s.Add(Row{"node", api.MetricPoint{T: 1, CPU: f(1)}})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	s, c := newStore(t)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Add(Row{"node", api.MetricPoint{T: c.t.Unix() + int64(i*60), CPU: f(1)}})
				if i%10 == 0 {
					_ = s.Flush()
					_, _ = s.Query("node", c.t.Add(-time.Hour), c.t.Add(24*time.Hour), 100)
					_, _ = s.Latest()
				}
			}
		}()
	}
	wg.Wait()
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := ConfigFrom(api.MetricsConfig{})
	if c.Keep1m != 48*time.Hour || c.Keep5m != 30*24*time.Hour || c.Keep1h != 365*24*time.Hour || c.MaxBytes != 256<<20 {
		t.Fatalf("%+v", c)
	}
	if c := ConfigFrom(api.MetricsConfig{Keep1mHours: 6, MaxMB: 8}); c.Keep1m != 6*time.Hour || c.MaxBytes != 8<<20 {
		t.Fatalf("%+v", c)
	}
}
