package web

import (
	"context"
	"fmt"
	"html/template"
	"strings"
	"sync"
	"time"

	"cubeherd/internal/server"
)

const (
	perfInterval = 5 * time.Second
	perfKeep     = 120 // samples kept per open page: 10 minutes
)

type perfDim struct {
	Name string
	TPS  string
	MSPT string
	Pct  int
	Tone string
}

type perfData struct {
	ID     string
	Src    string
	State  string // ok | warn | crit | none
	Reason string // for none: stopped | paused | starting | unsupported | waiting
	Err    string

	TPS, MSPT, P95, CPU string
	HaveP95             bool
	TPSTone, MSPTTone   string
	MSPTPct             int
	TPSSpark, CPUSpark  template.HTML
	MemUsed             uint64
	MemGB               int
	HeapGB              string
	Chart               template.HTML
	Dims                []perfDim
}

func tone(pct float64) string {
	switch {
	case pct > 95:
		return "crit"
	case pct > 70:
		return "warn"
	}
	return "ok"
}

func (a *App) perfFormat(v float64) string { return decimal(a.lang(), roundTo(v, 1)) }

func roundTo(v float64, n int) float64 {
	p := 1.0
	for i := 0; i < n; i++ {
		p *= 10
	}
	return float64(int(v*p+0.5)) / p
}

// perfView builds the template data from the samples collected while the page is open.
func (a *App) perfView(s *server.Server, samples []server.PerfSample, cpuHist []float64, cpu float64, mem uint64, reason, errText string) perfData {
	d := perfData{ID: s.ID, Src: server.PerfSource(s.Type), State: "none", Reason: reason, Err: errText, MemUsed: mem, MemGB: s.MemoryGB,
		HeapGB: decimal(a.lang(), float64(s.HeapMB())/1024)}
	d.CPU = a.perfFormat(cpu)
	d.CPUSpark = spark(cpuHist)
	if len(samples) == 0 {
		return d
	}
	cur := samples[len(samples)-1]
	d.State = server.PerfLevel(cur)
	d.TPS, d.MSPT = a.perfFormat(cur.TPS), a.perfFormat(cur.MSPT)
	d.TPSTone, d.MSPTTone = d.State, d.State
	d.MSPTPct = int(min(100, cur.MSPT/50*100))
	d.HaveP95 = cur.HaveP95
	d.P95 = a.perfFormat(cur.P95)
	tps := make([]float64, len(samples))
	for i, p := range samples {
		tps[i] = p.TPS
	}
	d.TPSSpark = perfSpark(tps, 20)
	d.Chart = perfChart(samples)
	for _, x := range cur.Dims {
		pct := min(100, x.MSPT/50*100)
		d.Dims = append(d.Dims, perfDim{Name: x.Name, TPS: a.perfFormat(x.TPS), MSPT: a.perfFormat(x.MSPT), Pct: int(pct), Tone: tone(pct)})
	}
	return d
}

// perfSpark is a small TPS line scaled to 0..max.
func perfSpark(vals []float64, max float64) template.HTML {
	const w, h = 160.0, 40.0
	if len(vals) < 2 {
		vals = []float64{max, max}
	}
	var pts strings.Builder
	for i, v := range vals {
		fmt.Fprintf(&pts, "%.1f,%.1f ", float64(i)/float64(len(vals)-1)*w, h-2-min(v/max, 1)*(h-4))
	}
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 %g %g" preserveAspectRatio="none" aria-hidden="true"><polyline points="%s" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/></svg>`, w, h, strings.TrimSpace(pts.String())))
}

// perfChart draws mean and 95th percentile tick time with the 50 ms limit.
func perfChart(samples []server.PerfSample) template.HTML {
	const w, h = 900.0, 200.0
	maxY := 60.0
	for _, p := range samples {
		maxY = max(maxY, p.MSPT, p.P95)
	}
	maxY *= 1.1
	y := func(v float64) float64 { return h - 22 - v/maxY*(h-30) }
	x := func(i int) float64 {
		if len(samples) < 2 {
			return 36
		}
		return 36 + float64(i)/float64(len(samples)-1)*(w-44)
	}
	line := func(f func(server.PerfSample) float64) string {
		var b strings.Builder
		for i, p := range samples {
			fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(f(p)))
		}
		return strings.TrimSpace(b.String())
	}
	var b strings.Builder
	for _, t := range []float64{0, 25, 50, 75, 100, 150, 200} {
		if t <= maxY {
			fmt.Fprintf(&b, `<line class="grid" x1="36" x2="%g" y1="%.1f" y2="%.1f"/><text class="axis" x="30" y="%.1f" text-anchor="end">%g</text>`, w-8, y(t), y(t), y(t)+3, t)
		}
	}
	fmt.Fprintf(&b, `<line class="limit" x1="36" x2="%g" y1="%.1f" y2="%.1f"/>`, w-8, y(50), y(50))
	mspt := line(func(p server.PerfSample) float64 { return p.MSPT })
	fmt.Fprintf(&b, `<polygon class="area" points="36,%.1f %s %g,%.1f"/>`, y(0), mspt, w-8, y(0))
	if samples[len(samples)-1].HaveP95 {
		fmt.Fprintf(&b, `<polyline class="line2" points="%s"/>`, line(func(p server.PerfSample) float64 { return p.P95 }))
	}
	fmt.Fprintf(&b, `<polyline class="line" points="%s"/>`, mspt)
	return template.HTML(fmt.Sprintf(`<svg class="chart" viewBox="0 0 %g %g" preserveAspectRatio="none" role="img">%s</svg>`, w, h, b.String()))
}

// streamPerf polls tick performance every few seconds while the Performance tab is open and
// keeps the history in memory for this page only. A paused server is never asked (RCON would not wake it).
func (a *App) streamPerf(ctx context.Context, s *server.Server, emit func(string, string)) {
	var (
		mu      sync.Mutex
		cpu     float64
		mem     uint64
		cpuHist []float64
	)
	go func() {
		for ctx.Err() == nil {
			c, err := a.M.Docker.Inspect(ctx, s.Container())
			if err != nil || !c.State.Running {
				sleep(ctx, 3*time.Second)
				continue
			}
			n := 0
			_ = a.M.Docker.StatsStream(ctx, s.Container(), func(st dockerStats) {
				if n++; n%2 == 1 {
					return
				}
				mu.Lock()
				cpu, mem = st.CPUPercent, st.MemUsed
				mu.Unlock()
			})
			sleep(ctx, 2*time.Second)
		}
	}()

	var samples []server.PerfSample
	for ctx.Err() == nil {
		cur, _ := a.M.Store.Get(s.ID)
		if cur == nil {
			return
		}
		s = cur
		reason, errText := "", ""
		switch st := a.M.View(ctx, s, false).Status; {
		case st == server.StatusPaused:
			reason = "paused"
		case st == server.StatusStarting || st == server.StatusPreparing || st == server.StatusInstalling:
			reason = "starting"
		case !st.Live():
			reason = "stopped"
		default:
			p, err := a.M.Perf(ctx, s)
			switch {
			case err == server.ErrPerfUnsupported:
				reason = "unsupported"
			case err != nil:
				reason, errText = "waiting", err.Error()
			default:
				p.At = time.Now()
				if samples = append(samples, p); len(samples) > perfKeep {
					samples = samples[1:]
				}
			}
		}
		if reason == "stopped" || reason == "paused" {
			samples = nil // old values would be misleading after a restart
		}
		mu.Lock()
		c, m := cpu, mem
		if c > 0 || m > 0 {
			if cpuHist = append(cpuHist, c); len(cpuHist) > 60 {
				cpuHist = cpuHist[1:]
			}
		}
		mu.Unlock()
		html, err := a.renderString("server", "perfBody", a.perfView(s, samples, cpuHist, c, m, reason, errText))
		if err == nil {
			emit("perf", html)
		}
		sleep(ctx, perfInterval)
	}
}
