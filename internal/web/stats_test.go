package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/remote"
)

func fp(v float64) *float64 { return &v }

func TestChartBreaksLinesAtGapsAndMissingValues(t *testing.T) {
	sp := chartSpec{Title: "cpu", Unit: pctUnit, From: 0, To: 3600, Step: 60, Loc: time.UTC}
	for i := 0; i < 10; i++ {
		sp.T = append(sp.T, int64(i*60))
	}
	for i := 0; i < 5; i++ { // five points, a pause of ten minutes, then four
		sp.T[i] = int64(i * 60)
	}
	sp.T[5], sp.T[6], sp.T[7], sp.T[8], sp.T[9] = 1000, 1060, 1120, 1180, 1240
	vals := make([]*float64, 10)
	for i := range vals {
		vals[i] = fp(float64(i))
	}
	vals[8] = nil // a missing value
	sp.Series = []chartSeries{{Name: "cpu", Class: "s1", Vals: vals}}
	svg := string(renderChart(sp))
	if got := strings.Count(svg, `class="line s1" d="`); got != 1 {
		t.Fatalf("one path element expected, got %d", got)
	}
	d := svg[strings.Index(svg, `class="line s1" d="`):]
	d = d[:strings.Index(d[20:], `"`)+20]
	if got := strings.Count(d, "M"); got != 2 || strings.Count(svg, "<circle") != 1 {
		t.Fatalf("a gap and a missing value make three pieces (two lines and a lone dot), got %d lines, %d dots: %s", got, strings.Count(svg, "<circle"), d)
	}
	if strings.Count(svg, "<title>") != 10 {
		t.Fatalf("every point has a hover text, got %d", strings.Count(svg, "<title>"))
	}
}

func TestChartHandlesTheEdgeCases(t *testing.T) {
	base := chartSpec{Title: "<b>x</b>", Unit: intUnit, From: 0, To: 100, Step: 60, Loc: time.UTC}
	// no data at all
	if svg := string(renderChart(base)); !strings.Contains(svg, "<svg") || strings.Contains(svg, "NaN") {
		t.Fatalf("empty chart: %s", svg)
	}
	// one point draws a dot, not a path
	one := base
	one.T = []int64{50}
	one.Series = []chartSeries{{Name: "n", Class: "s2", Vals: []*float64{fp(5)}}}
	if svg := string(renderChart(one)); !strings.Contains(svg, "<circle") || strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("single point: %s", svg)
	}
	// the title and event details are escaped
	ev := base
	ev.Events = []api.MetricEvent{{T: 10, Kind: "crash", Detail: `<script>alert(1)</script>`}}
	svg := string(renderChart(ev))
	if strings.Contains(svg, "<script>") || strings.Contains(svg, "<b>x</b>") {
		t.Fatal("chart text must be escaped")
	}
	if !strings.Contains(svg, `ev-crash`) {
		t.Fatal("event marker missing")
	}
	// zero span and zero step must not divide by zero
	z := chartSpec{Title: "z", Unit: intUnit, From: 5, To: 5, Step: 0, T: []int64{5}, Series: []chartSeries{{Class: "s1", Vals: []*float64{fp(1)}}}}
	if svg := string(renderChart(z)); strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("degenerate chart: %s", svg)
	}
	if niceMax(0) != 1 || niceMax(7) != 10 || niceMax(21) != 50 || niceMax(0.3) != 0.5 {
		t.Fatal("niceMax rounds to 1, 2, 5, 10")
	}
}

func TestUnsupportedAgentGetsAHint(t *testing.T) {
	a := newTestApp(t, "")
	if msg := a.historyErr(remote.ErrUnsupported); !strings.Contains(msg, "Agent") {
		t.Fatalf("hint missing: %q", msg)
	}
	if msg := a.historyErr(errors.New("boom")); !strings.Contains(msg, "boom") {
		t.Fatalf("other errors keep their text: %q", msg)
	}
}

func TestStatsRightsAreSplitByServerAndNode(t *testing.T) {
	d, st := newAccounts(t)
	// a role with the statistics rights only
	sid, _ := st.SaveRole(access.Role{Name: "Server stats", Perms: []access.Perm{access.StatsServer}})
	nid, _ := st.SaveRole(access.Role{Name: "Node stats", Perms: []access.Perm{access.StatsNode}})
	u := addUser(t, st, "stella")
	giveRole(t, st, u, access.RoleViewer, access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"})
	_, _ = st.AddGrant(access.Grant{PrincipalID: u.ID, RoleID: sid, Scope: access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"}})
	c := d.cookieFor(t, st, "stella")
	if got := d.as(c, "GET", "/n/local/s/loc-one/stats", nil, false); got.Code != 200 || !strings.Contains(got.Body.String(), "stats-panel") {
		t.Fatalf("the granted server: %d", got.Code)
	}
	// the right on one server does not carry over to another, even one the user may see
	v := addUser(t, st, "viktor")
	giveRole(t, st, v, access.RoleViewer, access.Scope{Level: access.LevelNode, Node: "local"})
	_, _ = st.AddGrant(access.Grant{PrincipalID: v.ID, RoleID: sid, Scope: access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"}})
	cv := d.cookieFor(t, st, "viktor")
	if got := d.as(cv, "GET", "/n/local/s/loc-two/stats", nil, false).Code; got != 403 {
		t.Errorf("another server on the node: %d, want 403", got)
	}
	if got := d.as(cv, "GET", "/n/local/s/loc-one/stats", nil, false).Code; got != 200 {
		t.Errorf("the granted server: %d", got)
	}
	// the node history is its own right
	if got := d.as(c, "GET", "/n/local/stats", nil, false).Code; got != 403 {
		t.Errorf("node stats without the right: %d", got)
	}
	w := addUser(t, st, "nora")
	_, _ = st.AddGrant(access.Grant{PrincipalID: w.ID, RoleID: nid, Scope: access.Scope{Level: access.LevelNode, Node: "local"}})
	cw := d.cookieFor(t, st, "nora")
	if got := d.as(cw, "GET", "/n/local/stats", nil, false).Code; got != 200 {
		t.Errorf("node stats with the right: %d", got)
	}
	// stats without being allowed to see the server stay hidden
	if got := d.as(cw, "GET", "/n/local/s/loc-one/stats", nil, false).Code; got != 404 && got != 403 {
		t.Errorf("server stats through a node right: %d", got)
	}
	// the nodes page lists the node for her, without its health
	body := d.as(cw, "GET", "/nodes", nil, false).Body.String()
	if !strings.Contains(body, "/n/local/stats") {
		t.Error("the nodes page should link to the statistics")
	}
	if strings.Contains(body, "Latency") && strings.Contains(body, "pill s-online") {
		t.Error("health of a node needs nodes.view")
	}
	// purging needs nodes.manage
	if got := d.as(cw, "POST", "/nodes/local/stats/purge", url.Values{}, false).Code; got != 403 {
		t.Errorf("purge without nodes.manage: %d", got)
	}
	if got := d.as(d.cookieFor(t, st, "admin"), "POST", "/nodes/local/stats/purge", url.Values{"server": {"loc-one"}}, true).Code; got != 204 {
		t.Errorf("purge by an owner: %d", got)
	}
}

func TestBuiltInRolesCarryTheStatsRights(t *testing.T) {
	_, st := newAccounts(t)
	rs, _ := st.ListRoles()
	has := map[string]map[access.Perm]bool{}
	for _, r := range rs {
		has[r.Name] = map[access.Perm]bool{}
		for _, p := range r.Perms {
			has[r.Name][p] = true
		}
	}
	for role, want := range map[string][2]bool{access.RoleOwner: {true, true}, access.RoleAdmin: {true, true}, access.RoleOperator: {true, false}, access.RoleModerator: {false, false}, access.RoleViewer: {false, false}} {
		if has[role][access.StatsServer] != want[0] || has[role][access.StatsNode] != want[1] {
			t.Errorf("%s: server=%v node=%v", role, has[role][access.StatsServer], has[role][access.StatsNode])
		}
	}
}

func TestSidebarRefreshDoesNotLeakOtherServers(t *testing.T) {
	d, st := newAccounts(t)
	u := addUser(t, st, "leah")
	giveRole(t, st, u, access.RoleViewer, access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"})
	body := d.as(d.cookieFor(t, st, "leah"), "GET", "/partials/rack", nil, true).Body.String()
	if !strings.Contains(body, "LOC-ONE") || strings.Contains(body, "LOC-TWO") {
		t.Fatalf("the sidebar refresh shows servers the user may not see:\n%s", body)
	}
	if ov := d.as(d.cookieFor(t, st, "leah"), "GET", "/partials/overview", nil, true).Body.String(); strings.Contains(ov, "LOC-TWO") {
		t.Fatal("the overview refresh leaks a server")
	}
}

func TestNodeDialogSavesTheRecorderSetting(t *testing.T) {
	d := newDashboard(t, true)
	rec := d.do("POST", "/nodes/local", url.Values{"name": {"home"}, "metrics": {"on"}, "keep1m": {"24"}, "maxmb": {"64"}}, true)
	if rec.Code >= 400 {
		t.Fatalf("save: %d", rec.Code)
	}
	if got := d.cfg.Get().Metrics; !got.Enabled || got.Keep1mHours != 24 || got.MaxMB != 64 {
		t.Fatalf("stored setting: %+v", got)
	}
	// out-of-range numbers are clamped, not trusted
	d.do("POST", "/nodes/local", url.Values{"name": {"home"}, "metrics": {"on"}, "keep1m": {"99999"}, "maxmb": {"-5"}}, true)
	if got := d.cfg.Get().Metrics; got.Keep1mHours != 720 || got.MaxMB != 0 {
		t.Fatalf("clamping: %+v", got)
	}
	// the dialog shows what is stored
	if body := d.do("GET", "/nodes/local/edit", nil, true).Body.String(); !strings.Contains(body, `value="on" selected`) {
		t.Fatal("the dialog should show the recorder as on")
	}
}

func TestRemoteNodeSettingIsPushedToTheAgent(t *testing.T) {
	var _ config.Metrics
	m := metricsForm(mustForm(url.Values{"metrics": {""}}))
	if m != nil {
		t.Fatal("leaving it to the agent stores nothing")
	}
	m = metricsForm(mustForm(url.Values{"metrics": {"off"}}))
	if m == nil || m.Enabled {
		t.Fatalf("explicit off: %+v", m)
	}
}

func mustForm(v url.Values) *http.Request {
	r, _ := http.NewRequestWithContext(context.Background(), "POST", "/", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestPrometheusEndpoint(t *testing.T) {
	d := newDashboard(t, true)
	get := func(token string) *http.Response {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "/metrics", nil)
		req.Host = "example.com"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		d.h.ServeHTTP(rec, req)
		return rec.Result()
	}
	if got := get("anything").StatusCode; got != 404 {
		t.Fatalf("off by default: %d", got)
	}
	rec := d.do("POST", "/settings/prometheus", url.Values{"op": {"token"}}, true)
	body := rec.Body.String()
	i := strings.Index(body, "cmp_")
	if rec.Code != 200 || i < 0 {
		t.Fatalf("token not shown: %d %s", rec.Code, body)
	}
	tok := body[i : i+4+43]
	if strings.Contains(d.cfg.Get().ScrapeTokenHash, tok) || d.cfg.Get().ScrapeTokenHash == "" {
		t.Fatal("only the hash may be stored")
	}
	if got := get("").StatusCode; got != 401 {
		t.Errorf("no token: %d", got)
	}
	if got := get("cmp_wrong").StatusCode; got != 401 {
		t.Errorf("wrong token: %d", got)
	}
	resp := get(tok)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("with the token: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	b := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := b.String()
	for _, want := range []string{"# TYPE cubeherd_node_up gauge", `cubeherd_node_up{node="`, "cubeherd_server_up{", "cubeherd_build_info{version="} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// rotating invalidates the old token
	d.do("POST", "/settings/prometheus", url.Values{"op": {"token"}}, true)
	if got := get(tok).StatusCode; got != 401 {
		t.Errorf("the old token after rotation: %d", got)
	}
	// switching off
	d.do("POST", "/settings/prometheus", url.Values{"op": {"off"}}, true)
	if got := get(tok).StatusCode; got != 404 {
		t.Errorf("after switching off: %d", got)
	}
}

func TestPrometheusEscapesLabels(t *testing.T) {
	var p promWriter
	p.sample("m", "h", "gauge", 1.5, "server", "a\"b\\c\nd")
	if got := p.b.String(); !strings.Contains(got, `server="a\"b\\c\nd"} 1.5`) {
		t.Fatalf("%q", got)
	}
}

func TestMemoryAxisEndsOnWholeGigabytes(t *testing.T) {
	sp := chartSpec{Title: "m", Unit: gbUnit("en"), From: 0, To: 100, Step: 60, Loc: time.UTC, Div: 1 << 30,
		T: []int64{10}, Series: []chartSeries{{Class: "s2", Vals: []*float64{fp(2.2 * (1 << 30))}}}}
	svg := string(renderChart(sp))
	for _, want := range []string{">1.0 GB<", ">2.0 GB<", ">3.0 GB<", ">4.0 GB<"} {
		if !strings.Contains(svg, want) {
			t.Fatalf("the axis should read 0 to 4 GB in round steps, missing %s", want)
		}
	}
}

func TestCountAxesUseWholeNumbers(t *testing.T) {
	sp := chartSpec{Title: "p", Unit: intUnit, Count: true, From: 0, To: 100, Step: 60, Loc: time.UTC, T: []int64{10}, Series: []chartSeries{{Class: "s3", Vals: []*float64{fp(1)}}}}
	svg := string(renderChart(sp))
	for _, want := range []string{">0<", ">1<", ">2<", ">3<", ">4<"} {
		if !strings.Contains(svg, want) {
			t.Fatalf("a player axis should read 0 to 4, missing %s", want)
		}
	}
	if m := msUnit("en"); m(0.05) != "0.05 ms" || m(12.34) != "12.3 ms" || m(0) != "0 ms" && m(0) != "0.0 ms" {
		t.Fatalf("ms labels: %q %q %q", m(0.05), m(12.34), m(0))
	}
}
