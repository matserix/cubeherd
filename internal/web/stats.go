package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/node"
	"cubeherd/internal/nodes"
	"cubeherd/internal/remote"
)

type rangeOpt struct {
	Key, Label string
	Dur        time.Duration
}

var statRanges = []rangeOpt{
	{"1h", "1 Stunde", time.Hour}, {"6h", "6 Stunden", 6 * time.Hour}, {"24h", "24 Stunden", 24 * time.Hour},
	{"7d", "7 Tage", 7 * 24 * time.Hour}, {"30d", "30 Tage", 30 * 24 * time.Hour}, {"1y", "1 Jahr", 365 * 24 * time.Hour},
}

func pickRange(key string) rangeOpt {
	for _, r := range statRanges {
		if r.Key == key {
			return r
		}
	}
	return statRanges[2]
}

type chartBox struct {
	Title  string
	Legend []chartSeries // names and colours, for charts with several lines
	SVG    template.HTML
}

type eventRow struct {
	Time   string
	Kind   string // translated label
	Class  string // the raw kind, for the colour
	Detail string
}

// historyData is what the history panels show.
type historyData struct {
	ID, Node     string // server id and node id for the links
	Name         string
	Range        string
	Ranges       []rangeOpt
	Charts       []chartBox
	Events       []eventRow
	Availability string // "99.2 %" or ""
	Enabled      bool   // the recorder is on
	Empty        bool   // no samples in the range
	Err          string
	CanManage    bool   // may purge and switch recording (nodes.manage)
	Base         string // address of the panel, for the range chips
	Fragment     bool   // the chips reload the panel in place instead of the page
	Since        string
}

// eventKinds are the German source texts of the event kinds.
var eventKinds = map[string]string{"start": "Gestartet", "stop": "Gestoppt", "restart": "Neu gestartet", "update": "Aktualisiert", "crash": "Absturz", "uninstall": "Deinstalliert"}

func col(h api.History, f func(api.MetricPoint) *float64) []*float64 {
	out := make([]*float64, len(h.Points))
	for i, p := range h.Points {
		out[i] = f(p)
	}
	return out
}

func anyValue(v []*float64) bool {
	for _, x := range v {
		if x != nil {
			return true
		}
	}
	return false
}

func times(h api.History) []int64 {
	t := make([]int64, len(h.Points))
	for i, p := range h.Points {
		t[i] = p.T
	}
	return t
}

func (a *App) spec(h api.History, title string, unit func(float64) string) chartSpec {
	return chartSpec{Title: title, Unit: unit, T: times(h), From: h.From, To: h.To, Step: max(h.Step, 1), Events: h.Events, Loc: displayLoc()}
}

// historyErr turns a failed history request into a message for the panel.
func (a *App) historyErr(err error) string {
	if errors.Is(err, remote.ErrUnsupported) {
		return a.t("Dieser Agent kennt noch keine Statistik. Aktualisiere den Agent, um den Verlauf zu sehen.")
	}
	return a.t("Der Verlauf konnte nicht geladen werden: %s", err.Error())
}

func (a *App) availability(h api.History) string {
	var sum, n float64
	for _, p := range h.Points {
		if p.Up != nil {
			sum += *p.Up
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return decimal(a.lang(), float64(int(sum/n*1000+0.5))/10) + " %"
}

func (a *App) eventRows(h api.History) []eventRow {
	var out []eventRow
	for i := len(h.Events) - 1; i >= 0; i-- { // newest first
		e := h.Events[i]
		k := eventKinds[e.Kind]
		if k == "" {
			k = e.Kind
		}
		out = append(out, eventRow{Time: time.Unix(e.T, 0).In(displayLoc()).Format("02.01. 15:04"), Kind: k, Class: e.Kind, Detail: e.Detail})
	}
	return out
}

func (a *App) baseStats(r *http.Request, h api.History, err error, scopeID string) historyData {
	rg := pickRange(r.FormValue("range"))
	d := historyData{Range: rg.Key, Ranges: statRanges, Node: a.NodeID, ID: scopeID, Enabled: h.Enabled, CanManage: principalOf(r).Global(access.NodesManage)}
	if err != nil {
		d.Err = a.historyErr(err)
	}
	d.Empty = len(h.Points) == 0
	if scopeID != "" {
		d.Base, d.Fragment = "/servers/"+scopeID+"/stats", true
	} else {
		d.Base = "/n/" + a.NodeID + "/stats"
	}
	return d
}

func (a *App) historyWindow(r *http.Request) (time.Time, time.Time) {
	to := time.Now()
	return to.Add(-pickRange(r.FormValue("range")).Dur), to
}

// serverStats is the history panel of the Performance tab.
func (a *App) serverStats(w http.ResponseWriter, r *http.Request) {
	s, ok := a.loadServer(w, r)
	if !ok {
		return
	}
	if !principalOf(r).Can(access.ServerView, a.NodeID, s.ID) { // history is for people who may see the server
		http.NotFound(w, r)
		return
	}
	from, to := a.historyWindow(r)
	h, err := a.M.ServerHistory(r.Context(), s.ID, from, to, 300)
	d := a.baseStats(r, h, err, s.ID)
	d.Name = s.Name
	if err == nil && !d.Empty {
		d.Charts = a.serverCharts(h)
		d.Events = a.eventRows(h)
		d.Availability = a.availability(h)
	}
	a.partial(w, "server", "statsBody", d)
}

func (a *App) serverCharts(h api.History) []chartBox {
	lang := a.lang()
	var out []chartBox
	add := func(title string, sp chartSpec) {
		any := false
		for _, s := range sp.Series {
			any = any || anyValue(s.Vals)
		}
		if any {
			out = append(out, chartBox{Title: title, SVG: renderChart(sp)})
		}
	}
	cpu := a.spec(h, a.t("CPU"), pctUnit)
	cpu.Series = []chartSeries{
		{Name: a.t("Mittel"), Class: "s1", Area: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.CPU })},
		{Name: a.t("Spitze"), Class: "s1", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.CPUMax })}}
	add(a.t("CPU (Anteil des Hosts)"), cpu)
	mem := a.spec(h, a.t("Arbeitsspeicher"), gbUnit(lang))
	mem.Div = 1 << 30
	mem.Series = []chartSeries{
		{Name: a.t("Mittel"), Class: "s2", Area: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.Mem })},
		{Name: a.t("Spitze"), Class: "s2", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.MemMax })}}
	add(a.t("Arbeitsspeicher"), mem)
	pl := a.spec(h, a.t("Spieler"), intUnit)
	pl.Count = true
	pl.Series = []chartSeries{
		{Name: a.t("Mittel"), Class: "s3", Area: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.Players })},
		{Name: a.t("Spitze"), Class: "s3", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.PlayersMax })}}
	add(a.t("Spieler online"), pl)
	tps := a.spec(h, "TPS", tpsUnit(lang))
	tps.YMax, tps.Fixed = 20, true
	tps.Series = []chartSeries{
		{Name: "TPS", Class: "s4", Vals: col(h, func(p api.MetricPoint) *float64 { return p.TPS })},
		{Name: a.t("Minimum"), Class: "s4", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.TPSMin })}}
	add("TPS", tps)
	ms := a.spec(h, "MSPT", msUnit(lang))
	ms.Series = []chartSeries{
		{Name: "MSPT", Class: "s5", Vals: col(h, func(p api.MetricPoint) *float64 { return p.MSPT })},
		{Name: a.t("Spitze"), Class: "s5", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.MSPTMax })}}
	add(a.t("Zeit pro Tick (MSPT)"), ms)
	return out
}

// nodeStats is the page with the history of one node.
func (a *App) nodeStats(w http.ResponseWriter, r *http.Request) {
	from, to := a.historyWindow(r)
	h, err := a.M.NodeHistory(r.Context(), from, to, 300)
	d := a.baseStats(r, h, err, "")
	d.Name = a.NodeName
	if err == nil && !d.Empty {
		d.Charts = a.nodeCharts(r.Context(), h, from, to)
		d.Events = nil
	}
	a.render(w, r, "nodestats", Page{Title: a.t("Statistik") + " · " + a.NodeName, Nav: "nodes", Data: d})
}

func (a *App) nodeCharts(ctx context.Context, h api.History, from, to time.Time) []chartBox {
	lang := a.lang()
	host := a.M.HostStats(ctx)
	var out []chartBox
	cpu := a.spec(h, "CPU", pctUnit)
	cpu.Series = []chartSeries{
		{Name: a.t("Mittel"), Class: "s1", Area: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.CPU })},
		{Name: a.t("Spitze"), Class: "s1", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.CPUMax })}}
	cpu.YMax, cpu.Fixed = 100, true
	out = append(out, chartBox{Title: a.t("CPU des Hosts"), SVG: renderChart(cpu)})
	mem := a.spec(h, a.t("Arbeitsspeicher"), gbUnit(lang))
	mem.Div = 1 << 30
	mem.Series = []chartSeries{
		{Name: a.t("Mittel"), Class: "s2", Area: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.Mem })},
		{Name: a.t("Spitze"), Class: "s2", Thin: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.MemMax })}}
	if host.MemTotal > 0 {
		mem.YMax, mem.Fixed = float64(host.MemTotal), true
	}
	if anyValue(mem.Series[0].Vals) {
		out = append(out, chartBox{Title: a.t("Arbeitsspeicher des Hosts"), SVG: renderChart(mem)})
	}
	disk := a.spec(h, a.t("Festplatte"), gbUnit(lang))
	disk.Div = 1 << 30
	disk.Series = []chartSeries{{Name: a.t("Belegt"), Class: "s6", Area: true, Vals: col(h, func(p api.MetricPoint) *float64 { return p.Disk })}}
	if host.DiskTotal > 0 {
		disk.YMax, disk.Fixed = float64(host.DiskTotal), true
	}
	if anyValue(disk.Series[0].Vals) {
		out = append(out, chartBox{Title: a.t("Festplatte"), SVG: renderChart(disk)})
	}
	run := a.spec(h, a.t("Server"), intUnit)
	run.Count = true
	run.Series = []chartSeries{
		{Name: a.t("Laufende Server"), Class: "s3", Vals: col(h, func(p api.MetricPoint) *float64 { return p.Up })},
		{Name: a.t("Spieler"), Class: "s5", Vals: col(h, func(p api.MetricPoint) *float64 { return p.Players })}}
	out = append(out, chartBox{Title: a.t("Laufende Server und Spieler"), SVG: renderChart(run)})
	// memory of each server, for the ones the user may see
	if views := a.visibleViews(ctx); len(views) > 0 && len(views) <= 8 {
		sp := a.spec(h, a.t("Arbeitsspeicher je Server"), gbUnit(lang))
		sp.Div = 1 << 30
		for i, v := range views {
			sh, err := a.M.ServerHistory(ctx, v.S.ID, from, to, 300)
			if err != nil || len(sh.Points) == 0 {
				continue
			}
			sp.Series = append(sp.Series, chartSeries{Name: v.S.Name, Class: fmt.Sprintf("s%d", i%6+1), Vals: alignTo(h, sh, func(p api.MetricPoint) *float64 { return p.Mem })})
		}
		if len(sp.Series) > 0 {
			out = append(out, chartBox{Title: a.t("Arbeitsspeicher je Server"), Legend: sp.Series, SVG: renderChart(sp)})
		}
	}
	return out
}

// visibleViews are the servers of this node the user may see.
func (a *App) visibleViews(ctx context.Context) []api.View {
	views, _ := a.M.Views(ctx, false)
	p := principalFrom(ctx)
	var out []api.View
	for _, v := range views {
		if p.Can(access.ServerView, a.NodeID, v.S.ID) && p.Can(access.StatsServer, a.NodeID, v.S.ID) {
			out = append(out, v)
		}
	}
	return out
}

// alignTo maps one server's points onto the time grid of the node's points, so both can share an x axis.
func alignTo(grid, h api.History, f func(api.MetricPoint) *float64) []*float64 {
	by := map[int64]*float64{}
	for _, p := range h.Points {
		by[p.T] = f(p)
	}
	out := make([]*float64, len(grid.Points))
	for i, p := range grid.Points {
		out[i] = by[p.T]
	}
	return out
}

// purgeStats deletes the history of one server, or of the whole node when no server is named.
func (a *App) purgeStats(w http.ResponseWriter, r *http.Request) {
	info, ok := a.Reg.Get(r.PathValue("id"))
	if !ok {
		a.fail(w, "Dieser Node existiert nicht.")
		return
	}
	scope := strings.TrimSpace(r.FormValue("server"))
	if err := info.Node.PurgeMetrics(r.Context(), scope); err != nil {
		a.fail(w, "Der Verlauf konnte nicht gelöscht werden: %s", err.Error())
		return
	}
	a.toast(w, "ok", "Verlauf gelöscht.")
	if scope == "" {
		redirect(w, r, "/n/"+info.ID+"/stats")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------- Switching the recorder (node dialogs) ----------------

// metricsForm reads the recorder fields of a node dialog. mode "" leaves the setting to the agent (nil).
func metricsForm(r *http.Request) *config.Metrics {
	switch r.FormValue("metrics") {
	case "on", "off":
		return &config.Metrics{Enabled: r.FormValue("metrics") == "on",
			Keep1mHours: formInt(r, "keep1m", 0, 0, 720), Keep5mDays: formInt(r, "keep5m", 0, 0, 365),
			Keep1hDays: formInt(r, "keep1h", 0, 0, 3650), MaxMB: formInt(r, "maxmb", 0, 0, 100000)}
	}
	return nil
}

// metricsFields is the data of the shared recorder fields in the node dialogs.
type metricsFields struct {
	Mode                          string // "", "on" or "off"
	Keep1m, Keep5m, Keep1h, MaxMB int
	CanInherit                    bool // an agent may be left to decide (environment variable)
	Size                          string
	Since                         string
	Capped                        bool
}

func fieldsOf(m *config.Metrics, inherit bool) metricsFields {
	f := metricsFields{CanInherit: inherit}
	if m == nil {
		return f
	}
	f.Mode = "off"
	if m.Enabled {
		f.Mode = "on"
	}
	f.Keep1m, f.Keep5m, f.Keep1h, f.MaxMB = m.Keep1mHours, m.Keep5mDays, m.Keep1hDays, m.MaxMB
	return f
}

func (a *App) statusLine(ctx context.Context, n node.Node, f *metricsFields) {
	st, err := n.MetricsStatus(ctx)
	if err != nil {
		return
	}
	f.Size = mbText(st.Size)
	f.Capped = st.Capped
	if st.Since > 0 {
		f.Since = time.Unix(st.Since, 0).In(displayLoc()).Format("02.01.2006")
	}
}

func mbText(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// applyLocalMetrics stores the setting of the embedded node and starts or stops its recorder.
func (a *App) applyLocalMetrics(m *config.Metrics) error {
	if m == nil {
		return nil
	}
	if err := a.Cfg.Update(func(s *config.Settings) { s.Metrics = *m }); err != nil {
		return err
	}
	if l, ok := a.Reg.Local().(interface {
		ApplyMetrics(api.MetricsConfig) error
	}); ok {
		return l.ApplyMetrics(api.MetricsConfig{Enabled: m.Enabled, Keep1mHours: m.Keep1mHours, Keep5mDays: m.Keep5mDays, Keep1hDays: m.Keep1hDays, MaxMB: m.MaxMB})
	}
	return nil
}

var _ = nodes.LocalID

// localMetrics is the stored recorder setting of the embedded node.
func (a *App) localMetrics() *config.Metrics {
	m := a.Cfg.Get().Metrics
	return &m
}
