package manager

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/docker"
	"cubeherd/internal/server"
	"cubeherd/internal/sources"
)

var apiPrefix = regexp.MustCompile(`^/v[0-9.]+`)

// fakeDocker answers the few Engine API calls the sampler makes and counts them per container.
type fakeDocker struct {
	mu       sync.Mutex
	state    map[string]fdState
	cpu, sys uint64
	execs    map[string]int // rcon commands per container
	stats    map[string]int // stats calls per container
	execCmd  map[string]string
}

type fdState struct {
	running, restarting bool
	exit                int
	started             time.Time
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := apiPrefix.ReplaceAllString(r.URL.Path, "")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "json":
		st, ok := f.state[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var c docker.ContainerJSON
		c.ID, c.Name = parts[1], "/"+parts[1]
		c.State.Running, c.State.Restarting, c.State.ExitCode, c.State.StartedAt = st.running, st.restarting, st.exit, st.started
		if st.running {
			c.State.Status = "running"
		} else {
			c.State.Status = "exited"
		}
		json.NewEncoder(w).Encode(c)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "stats":
		f.stats[parts[1]]++
		f.cpu += 200_000_000   // 0.2 s of container CPU per call ...
		f.sys += 4_000_000_000 // ... against 4 s of host CPU: 5 %
		json.NewEncoder(w).Encode(map[string]any{
			"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": f.cpu}, "system_cpu_usage": f.sys, "online_cpus": 4},
			"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 0}, "system_cpu_usage": 0},
			"memory_stats": map[string]any{"usage": 2 << 30, "limit": 4 << 30, "stats": map[string]any{"inactive_file": 0}},
		})
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "exec":
		var body struct{ Cmd []string }
		json.NewDecoder(r.Body).Decode(&body)
		f.execs[parts[1]]++
		id := "e" + parts[1] + strings.Join(body.Cmd, "-")
		f.execCmd[id] = strings.Join(body.Cmd[1:], " ")
		json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "start":
		out := map[string]string{
			"list": "There are 2 of a max of 20 players online: anna, ben",
			"tps":  "§6TPS from last 1m, 5m, 15m: §a19.8, §a20.0, §a20.0",
			"mspt": "Server tick times (avg/min/max) from last 5s, 10s, 1m: ◴ 12.3/8.1/30.2, 11.0/7.9/25.0, 10.5/7.0/28.0",
		}[f.execCmd[parts[1]]]
		hdr := make([]byte, 8)
		hdr[0] = 1
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(out)))
		w.Write(append(hdr, out...))
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "json":
		json.NewEncoder(w).Encode(map[string]int{"ExitCode": 0})
	default:
		http.NotFound(w, r)
	}
}

func recorderManager(t *testing.T) (*Manager, *fakeDocker) {
	t.Helper()
	fd := &fakeDocker{state: map[string]fdState{}, execs: map[string]int{}, stats: map[string]int{}, execCmd: map[string]string{}}
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	dc, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st, err := server.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, st, cfg, sources.New(func() string { return "" }))
	m.MetricsDir = filepath.Join(root, "metrics")
	for _, id := range []string{"a", "b", "c", "d"} {
		s := &server.Server{ID: id, Name: strings.ToUpper(id), Type: "PAPER", Version: "1.21.4", Port: 25565, MemoryGB: 4, Kind: server.KindSoftware, DataDir: filepath.Join(root, "data-"+id)}
		if err := os.MkdirAll(s.DataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := st.Save(s, server.Globals{}); err != nil {
			t.Fatal(err)
		}
	}
	// a runs, b is paused by autopause, c is stopped cleanly, d keeps crashing
	started := time.Now().Add(-time.Hour)
	fd.state["mc-a"] = fdState{running: true, started: started}
	fd.state["mc-b"] = fdState{running: true, started: started}
	_ = os.WriteFile(filepath.Join(root, "data-b", ".paused"), nil, 0o644)
	fd.state["mc-c"] = fdState{exit: 0}
	fd.state["mc-d"] = fdState{running: true, restarting: true, started: started}
	return m, fd
}

func TestSamplerRecordsRunningServersAndLeavesOthersAlone(t *testing.T) {
	m, fd := recorderManager(t)
	st, err := m.openMetrics(true, api.MetricsConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.CloseMetrics()
	m.rec.counters, m.rec.status = map[string]serverCounters{}, map[string]server.Status{}
	ctx := context.Background()
	now := time.Now().Truncate(time.Minute).Add(-10 * time.Minute)
	m.sample(ctx, st, now.Unix())
	m.sample(ctx, st, now.Add(time.Minute).Unix())
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	hist := func(scope string) api.History {
		h, err := st.Query(scope, now.Add(-time.Minute), now.Add(2*time.Minute), 100)
		if err != nil || len(h.Points) != 2 {
			t.Fatalf("%s: %v %d points", scope, err, len(h.Points))
		}
		return h
	}
	a := hist("a").Points
	if a[0].CPU != nil {
		t.Error("the first sample has nothing to compare with and must not report CPU")
	}
	if a[1].CPU == nil || *a[1].CPU < 4.9 || *a[1].CPU > 5.1 {
		t.Errorf("cpu share from the counter delta should be 5 %%: %v", a[1].CPU)
	}
	if *a[1].Up != 1 || *a[1].Mem != float64(2<<30) || *a[1].Players != 2 {
		t.Errorf("running server: %+v", a[1])
	}
	if a[1].TPS == nil || *a[1].TPS != 19.8 || a[1].MSPT == nil || *a[1].MSPT != 12.3 {
		t.Errorf("tick figures: tps=%v mspt=%v", a[1].TPS, a[1].MSPT)
	}
	b := hist("b").Points[1]
	if *b.Up != 1 || b.Players != nil || b.TPS != nil {
		t.Errorf("a paused server counts as up but is never queried: %+v", b)
	}
	if fd.execs["mc-b"] != 0 || fd.execs["mc-c"] != 0 || fd.execs["mc-d"] != 0 {
		t.Errorf("RCON must only reach running servers: %v", fd.execs)
	}
	if fd.stats["mc-c"] != 0 || fd.stats["mc-d"] != 0 {
		t.Errorf("stopped and crashed containers are not asked for stats: %v", fd.stats)
	}
	c := hist("c").Points[1]
	if *c.Up != 0 || c.Mem != nil {
		t.Errorf("stopped server: %+v", c)
	}
	node := hist("node").Points[1]
	if *node.Up != 2 || *node.Players != 2 {
		t.Errorf("node: up=%v players=%v", node.Up, node.Players)
	}
	if h, _ := st.Query("d", now.Add(-time.Minute), time.Now().Add(time.Minute), 100); len(h.Events) != 1 || h.Events[0].Kind != "crash" {
		t.Errorf("a crash is one event, not one per sample: %+v", h.Events)
	}
}

func TestSamplerSkipsCPUAfterAContainerRestart(t *testing.T) {
	m, fd := recorderManager(t)
	st, _ := m.openMetrics(true, api.MetricsConfig{Enabled: true})
	defer m.CloseMetrics()
	m.rec.counters, m.rec.status = map[string]serverCounters{}, map[string]server.Status{}
	ctx := context.Background()
	now := time.Now().Truncate(time.Minute).Add(-10 * time.Minute)
	m.sample(ctx, st, now.Unix())
	fd.mu.Lock()
	fd.state["mc-a"] = fdState{running: true, started: time.Now()} // restarted since the first sample
	fd.mu.Unlock()
	m.sample(ctx, st, now.Add(time.Minute).Unix())
	_ = st.Flush()
	h, _ := st.Query("a", now.Add(-time.Minute), now.Add(2*time.Minute), 100)
	if len(h.Points) != 2 || h.Points[1].CPU != nil {
		t.Fatalf("counters of a new container run must not be compared with the old ones: %+v", h.Points[1].CPU)
	}
}

func TestRecorderStartsAndStopsWithTheSetting(t *testing.T) {
	m, _ := recorderManager(t)
	if err := m.ApplyMetrics(api.MetricsConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.MetricsStatus(context.Background()); !st.Enabled {
		t.Fatal("recorder should be on")
	}
	if _, err := os.Stat(filepath.Join(m.MetricsDir, "metrics.db")); err != nil {
		t.Fatalf("the file is created when recording starts: %v", err)
	}
	if err := m.ApplyMetrics(api.MetricsConfig{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.MetricsStatus(context.Background()); st.Enabled {
		t.Fatal("recorder should be off")
	}
	// history stays readable while off
	h, err := m.NodeHistory(context.Background(), time.Now().Add(-time.Hour), time.Now(), 100)
	if err != nil || h.Enabled {
		t.Fatalf("history while off: %v enabled=%v", err, h.Enabled)
	}
	m.CloseMetrics()
}

func TestNothingIsCreatedWhileTheFeatureIsOff(t *testing.T) {
	m, _ := recorderManager(t)
	if _, err := m.NodeHistory(context.Background(), time.Now().Add(-time.Hour), time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.MetricsStatus(context.Background()); st.Enabled || st.Size != 0 {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(m.MetricsDir); err == nil {
		t.Fatal("reading history must not create the metrics directory")
	}
}

func TestApplyConfigSwitchesTheRecorder(t *testing.T) {
	m, _ := recorderManager(t)
	defer m.CloseMetrics()
	ctx := context.Background()
	if err := m.ApplyConfig(api.Config{Metrics: &api.MetricsConfig{Enabled: true, Keep1mHours: 12}}); err != nil {
		t.Fatal(err)
	}
	if got := m.Config.Get().Metrics; !got.Enabled || got.Keep1mHours != 12 {
		t.Fatalf("stored: %+v", got)
	}
	if st, _ := m.MetricsStatus(ctx); !st.Enabled {
		t.Fatal("recorder did not start")
	}
	// a push without the field leaves it alone (a dashboard that does not manage this node)
	if err := m.ApplyConfig(api.Config{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.MetricsStatus(ctx); !st.Enabled {
		t.Fatal("a push without a recorder setting must not switch it off")
	}
	if err := m.ApplyConfig(api.Config{Metrics: &api.MetricsConfig{}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.MetricsStatus(ctx); st.Enabled || m.Config.Get().Metrics.Enabled {
		t.Fatal("explicit off was ignored")
	}
}
