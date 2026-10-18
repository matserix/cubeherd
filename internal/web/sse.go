package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"forgecraft/internal/docker"
	"forgecraft/internal/manager"
	"forgecraft/internal/server"
)

type sseEvent struct{ name, data string }

type dockerStats = docker.Stats

// serverEvents streams status, log, stats and player updates while a server page is open.
// Nothing is polled or followed once the browser disconnects.
func (a *App) serverEvents(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming nicht unterstützt", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ctx := r.Context()
	console := r.FormValue("tab") == "console"
	events := make(chan sseEvent, 64)
	emit := func(name, data string) {
		select {
		case events <- sseEvent{name, data}:
		case <-ctx.Done():
		}
	}

	if console {
		lines := make(chan string, 512)
		go a.streamLogs(ctx, s, lines)
		go a.streamOps(ctx, s.ID, lines)
		go batchLines(ctx, lines, func(html string) { emit("log", html) })
		go a.streamStats(ctx, s, emit)
	}

	statusTick := time.NewTicker(2 * time.Second)
	playersTick := time.NewTicker(20 * time.Second)
	keepAlive := time.NewTicker(25 * time.Second)
	defer statusTick.Stop()
	defer playersTick.Stop()
	defer keepAlive.Stop()

	lastHead, lastPlayers := "", ""
	var lastStatus server.Status
	var lastRender time.Time
	pushStatus := func(force bool) {
		cur, _ := a.M.Store.Get(s.ID)
		if cur == nil {
			emit("status", `<div class="callout crit"><div><b>Server wurde entfernt</b><span class="muted"><a href="/">Zur Übersicht</a></span></div></div>`)
			return
		}
		s = cur
		v := a.M.View(ctx, s, false)
		if !force && v.Status == lastStatus && time.Since(lastRender) < 30*time.Second {
			return // unchanged; the uptime text is refreshed every 30 s
		}
		lastRender = time.Now()
		statusChanged := v.Status != lastStatus
		lastStatus = v.Status
		html, err := a.renderString("server", "srvHead", serverPage{V: v, Disk: a.M.DiskUsage(s)})
		if err == nil && html != lastHead {
			lastHead = html
			emit("status", html)
		}
		if console && statusChanged {
			a.pushPlayers(ctx, s, v, &lastPlayers, emit)
		}
	}
	pushStatus(true)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			fmt.Fprintf(w, "event: %s\n", ev.name)
			for _, l := range strings.Split(ev.data, "\n") {
				fmt.Fprintf(w, "data: %s\n", l)
			}
			fmt.Fprint(w, "\n")
			fl.Flush()
		case <-statusTick.C:
			pushStatus(false)
		case <-playersTick.C:
			if console {
				a.pushPlayers(ctx, s, a.M.View(ctx, s, false), &lastPlayers, emit)
			}
		case <-keepAlive.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (a *App) pushPlayers(ctx context.Context, s *server.Server, v manager.View, last *string, emit func(string, string)) {
	d := playersData{ID: s.ID, Max: s.Props.MaxPlayers, Live: v.Status.Live()}
	if d.Live {
		if on, max, err := a.M.Players(ctx, s); err == nil {
			d.Online, d.Max = on, max
		}
	}
	html, err := a.renderString("server", "players", d)
	if err == nil && html != *last {
		*last = html
		emit("players", html)
	}
}

// streamLogs follows the container log and survives restarts of the container.
func (a *App) streamLogs(ctx context.Context, s *server.Server, out chan<- string) {
	shown := false
	var since int64
	for ctx.Err() == nil {
		c, err := a.M.Docker.Inspect(ctx, s.Container())
		if err != nil {
			sleep(ctx, 2*time.Second)
			continue
		}
		send := func(l string) {
			select {
			case out <- server.CleanLine(l):
			case <-ctx.Done():
			}
		}
		tail := 300
		if shown {
			tail = -1
		}
		if !c.State.Running {
			if !shown {
				_ = a.M.Docker.LogsSince(ctx, s.Container(), false, tail, 0, c.Config.Tty, send)
				shown, since = true, time.Now().Unix()
			}
			sleep(ctx, 2*time.Second)
			continue
		}
		_ = a.M.Docker.LogsSince(ctx, s.Container(), true, tail, since, c.Config.Tty, send)
		shown, since = true, time.Now().Unix()
		sleep(ctx, time.Second)
	}
}

// streamOps forwards Forgecraft action output (compose pull/up, stop) into the console.
func (a *App) streamOps(ctx context.Context, id string, out chan<- string) {
	var cur *manager.Op
	idx := 0
	for ctx.Err() == nil {
		o := a.M.Op(id)
		if o == nil {
			sleep(ctx, 2*time.Second)
			continue
		}
		if o != cur {
			cur, idx = o, 0
			// Skip output of actions that ended before this page was opened.
			if lines, _, done := o.Since(0); done {
				idx = len(lines)
			}
		}
		lines, changed, done := o.Since(idx)
		idx += len(lines)
		for _, l := range lines {
			select {
			case out <- "\x00" + l: // marker: Forgecraft line
			case <-ctx.Done():
				return
			}
		}
		if done {
			sleep(ctx, 2*time.Second)
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// batchLines groups log lines into one SSE event per ~120 ms.
func batchLines(ctx context.Context, in <-chan string, emit func(string)) {
	var b strings.Builder
	n := 0
	flush := func() {
		if n > 0 {
			emit(b.String())
			b.Reset()
			n = 0
		}
	}
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-in:
			if server.DashboardNoise(l) {
				continue
			}
			level := ""
			if strings.HasPrefix(l, "\x00") {
				l, level = l[1:], "cmd"
			} else {
				level = server.Level(l)
			}
			b.WriteString(`<div class="lv-` + level + `">` + template.HTMLEscapeString(l) + `</div>`)
			if n++; n >= 200 {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

func (a *App) streamStats(ctx context.Context, s *server.Server, emit func(string, string)) {
	hist := make([]float64, 0, 40)
	for ctx.Err() == nil {
		c, err := a.M.Docker.Inspect(ctx, s.Container())
		if err != nil || !c.State.Running {
			sleep(ctx, 3*time.Second)
			continue
		}
		n := 0
		_ = a.M.Docker.StatsStream(ctx, s.Container(), func(st dockerStats) {
			if n++; n%2 == 1 {
				return // one update every ~2 s is plenty
			}
			hist = append(hist, st.CPUPercent)
			if len(hist) > 40 {
				hist = hist[1:]
			}
			html, err := a.renderString("server", "stats", statsData{CPU: st.CPUPercent, Spark: spark(hist), MemUsed: st.MemUsed, MemGB: s.MemoryGB})
			if err == nil {
				emit("stats", html)
			}
		})
		sleep(ctx, 2*time.Second)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
