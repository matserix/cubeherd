package web

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cubeherd/internal/agent"
	"cubeherd/internal/api"
	"cubeherd/internal/auth"
	"cubeherd/internal/config"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/manager"
	"cubeherd/internal/node"
	"cubeherd/internal/nodes"
	"cubeherd/internal/server"
	"cubeherd/internal/sources"
)

// fakeHost is a node with two servers, used both as the dashboard's local node and as an agent's backend.
type fakeHost struct {
	node.Servers
	node.Console
	node.Live
	node.Files
	node.Addons
	node.Backups
	node.Host
	node.Metrics
	prefix string
}

func (f *fakeHost) Views(context.Context, bool) ([]api.View, error) {
	mk := func(id string, st server.Status, port int) api.View {
		return api.View{S: &server.Server{ID: id, Name: strings.ToUpper(id), Port: port, MemoryGB: 4, Type: "PAPER", Version: "1.21.4", Kind: server.KindSoftware}, Status: st, MaxPlayers: 20}
	}
	return []api.View{mk(f.prefix+"-one", server.StatusRunning, 25565), mk(f.prefix+"-two", server.StatusStopped, 25566)}, nil
}
func (f *fakeHost) Server(id string) (*server.Server, error) {
	if id == f.prefix+"-one" {
		return &server.Server{ID: id, Name: "ONE"}, nil
	}
	return nil, server.ErrNotFound
}
func (f *fakeHost) Discover(context.Context) []api.Unmanaged { return nil }
func (f *fakeHost) HostStats(context.Context) host.Stats {
	return host.Stats{Cores: 8, MemTotal: 16 << 30, MemUsed: 4 << 30, DiskTotal: 100 << 30, DiskUsed: 20 << 30, CPUPercent: 10}
}
func (f *fakeHost) DockerInfo(context.Context) (docker.Info, error) { return docker.Info{}, nil }
func (f *fakeHost) DockerHost() string                              { return "unix:///x" }
func (f *fakeHost) Loc() *time.Location                             { return time.UTC }
func (f *fakeHost) ArchiveDir() string                              { return "/a" }
func (f *fakeHost) ApplyConfig(api.Config) error                    { return nil }
func (f *fakeHost) NextPort(string) int                             { return 25570 }

type dashboard struct {
	app    *App
	h      http.Handler
	cookie *http.Cookie
	reg    *nodes.Registry
	cfg    *config.Store
}

func startFakeAgent(t *testing.T, prefix, token string) (addr, fp string, srv *http.Server) {
	t.Helper()
	cert, err := agent.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv = agent.New(&fakeHost{prefix: prefix}, agent.NewToken(token), "dev", "/srv").Server("", cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), api.Fingerprint(cert.Certificate[0]), srv
}

// newDashboard builds the real handler tree with a fake local node (optional) and any number of remote nodes.
func newDashboard(t *testing.T, withLocal bool, remotes ...config.Node) *dashboard {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("correct horse battery")
	cfg.Update(func(s *config.Settings) { s.PasswordHash, s.Language, s.Nodes = hash, "en", remotes })
	var local node.Node
	if withLocal {
		local = &fakeHost{prefix: "loc"}
	}
	reg := nodes.New(cfg, local, "dev")
	app, err := New(reg, sources.New(func() string { return "" }), cfg, t.TempDir(), ":0")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.sess.Issue(rec, false)
	return &dashboard{app: app, h: app.Handler(), cookie: rec.Result().Cookies()[0], reg: reg, cfg: cfg}
}

func (d *dashboard) do(method, path string, form url.Values, hx bool) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(d.cookie)
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	return rec
}

func TestNodePrefixMapsOntoThePlainRoutes(t *testing.T) {
	a := newTestApp(t, "")
	var gotPath, gotNode string
	h := a.nodePrefix(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotNode, _ = explicitNode(r)
	}))
	for _, c := range []struct{ in, path, node string }{
		{"/n/vps/s/survival/events", "/servers/survival/events", "vps"},
		{"/n/vps/s/new", "/servers/new", "vps"},
		{"/n/vps/s", "/servers", "vps"},
		{"/n/vps/wizard/preview", "/wizard/preview", "vps"},
		{"/n/vps/import", "/import", "vps"},
		{"/servers/survival", "/servers/survival", ""}, // the old URLs are untouched
		{"/settings", "/settings", ""},
	} {
		gotPath, gotNode = "", ""
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", c.in, nil))
		if gotPath != c.path || gotNode != c.node {
			t.Errorf("%s → %q node %q, want %q node %q", c.in, gotPath, gotNode, c.path, c.node)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/n/vps/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?node=vps" {
		t.Errorf("/n/vps/ → %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := httptest.NewRecorder(); func() int { h.ServeHTTP(rec, httptest.NewRequest("GET", "/n//x", nil)); return rec.Code }() != http.StatusNotFound {
		t.Error("an empty node id is not a 404")
	}
}

func TestScopeLinks(t *testing.T) {
	a := &App{NodeID: "vps"}
	in := `<a href="/servers/x?tab=files">a</a><button hx-post="/servers/x/action/stop">b</button>` +
		`<div sse-connect="/servers/x/events?tab=console"></div><form hx-post="/servers" action="/wizard/preview"></form>` +
		`<a href="/servers/new">new</a><a href="/n/other/s/y">explicit</a><a href="/settings">s</a><a href="/">home</a>`
	got := a.scopeLinks(in)
	for _, want := range []string{
		`href="/n/vps/s/x?tab=files"`, `hx-post="/n/vps/s/x/action/stop"`, `sse-connect="/n/vps/s/x/events?tab=console"`,
		`hx-post="/n/vps/s"`, `action="/n/vps/wizard/preview"`,
		`href="/servers/new"`, `href="/n/other/s/y"`, `href="/settings"`, `href="/"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if (&App{}).scopeLinks(in) != in {
		t.Error("a page that is not node-scoped must stay untouched")
	}
	// Node ids with odd characters are escaped, never injected.
	if out := (&App{NodeID: `a"b`}).scopeLinks(`<a href="/servers/x">`); strings.Contains(out, `a"b`) {
		t.Errorf("unescaped node id: %s", out)
	}
}

func TestPartialsOnANodePageStayOnThatNode(t *testing.T) {
	d := newDashboard(t, true)
	b := *d.app
	b.NodeID, b.NodeName = "vps", "VPS"
	s := &server.Server{ID: "atm", Name: "ATM", Kind: server.KindSoftware, Type: "PAPER", Version: "1.21.4", MemoryGB: 4, Node: "vps", NodeName: "VPS"}
	out, err := b.renderString("server", "srvHead", serverPage{V: manager.View{S: s, Status: server.StatusRunning, Can: api.Caps{Power: true, Update: true, Delete: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `/n/vps/s/atm/action/stop`) || strings.Contains(out, `"/servers/atm`) {
		t.Errorf("header actions are not scoped to the node:\n%s", out)
	}
}

func TestOverviewShowsEveryNode(t *testing.T) {
	addr, fp, _ := startFakeAgent(t, "rem", "tok")
	d := newDashboard(t, true, config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp})
	rec := d.do("GET", "/", nil, false)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	for _, want := range []string{
		`href="/n/local/s/loc-one"`, `href="/n/vps/s/rem-one"`, // links that mix nodes name their node
		`class="nodes-grid"`, `href="/?node=vps"`, `>VPS<`,
		`hx-post="/n/vps/s/rem-two/action/start"`, `id="row-vps-rem-two"`, // unique row ids and node actions
		`hx-get="/partials/overview?node="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %s", want)
		}
	}
	// Filtered to one node.
	body = d.do("GET", "/?node=vps", nil, false).Body.String()
	if strings.Contains(body, `href="/n/local/s/loc-one" style`) || !strings.Contains(body, `href="/n/vps/s/rem-one"`) {
		t.Error("the node filter does not narrow the table")
	}
	// The live partial works for the same data.
	if p := d.do("GET", "/partials/overview", nil, true); p.Code != 200 || !strings.Contains(p.Body.String(), "rem-two") {
		t.Errorf("partial: %d", p.Code)
	}
}

func TestSingleNodeOverviewIsUnchanged(t *testing.T) {
	d := newDashboard(t, true)
	body := d.do("GET", "/", nil, false).Body.String()
	if strings.Contains(body, `class="nodes-grid"`) || !strings.Contains(body, `class="host"`) {
		t.Error("with one node the overview keeps its host tiles")
	}
	if !strings.Contains(body, `href="/n/local/s/loc-one"`) {
		t.Error("server links use the node scheme")
	}
}

func TestUnreachableNodeIsShownAndBlocked(t *testing.T) {
	addr, fp, srv := startFakeAgent(t, "rem", "tok")
	d := newDashboard(t, true, config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp})
	d.do("GET", "/", nil, false) // learns the servers while the agent is up
	srv.Close()

	start := time.Now()
	body := d.do("GET", "/", nil, false).Body.String()
	if time.Since(start) > nodes.Wait+2*time.Second {
		t.Fatalf("an unreachable node stalled the overview for %v", time.Since(start))
	}
	if !strings.Contains(body, "VPS is unreachable") || !strings.Contains(body, "rem-one") {
		t.Errorf("no banner or no last known servers")
	}
	rec := d.do("GET", "/n/vps/s/rem-one", nil, false)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "This node is unreachable") {
		t.Errorf("server page of a dead node: %d", rec.Code)
	}
	if rec := d.do("POST", "/n/vps/s/rem-one/action/start", url.Values{}, true); !strings.Contains(rec.Header().Get("HX-Trigger"), "unreachable") {
		t.Errorf("action on a dead node: %d %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	// The local node keeps working.
	if rec := d.do("GET", "/n/local/s/missing", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("the local node must answer for itself (404 for a missing server), got %d", rec.Code)
	}
}

func TestUnknownNode(t *testing.T) {
	d := newDashboard(t, true)
	if rec := d.do("GET", "/n/nope/s/x", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("unknown node: %d", rec.Code)
	}
}

func TestWizardPicker(t *testing.T) {
	// One node: the picker skips itself.
	d := newDashboard(t, true)
	if rec := d.do("GET", "/servers/new", nil, false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/n/local/s/new" {
		t.Errorf("single node: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Several: a choice, with an unreachable node disabled.
	addr, fp, _ := startFakeAgent(t, "rem", "tok")
	d = newDashboard(t, true,
		config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp},
		config.Node{ID: "dead", Name: "DEAD", Addr: "127.0.0.1:1", Token: "x", Fingerprint: fp})
	body := d.do("GET", "/servers/new", nil, false).Body.String()
	if !strings.Contains(body, `href="/n/vps/s/new"`) || !strings.Contains(body, `href="/n/local/s/new"`) || strings.Contains(body, `href="/n/dead/s/new"`) {
		t.Errorf("picker:\n%s", body)
	}
	// No nodes at all.
	d = newDashboard(t, false)
	if body := d.do("GET", "/servers/new", nil, false).Body.String(); !strings.Contains(body, "There is no node yet") {
		t.Error("an empty dashboard does not say it needs a node")
	}
	if rec := d.do("GET", "/", nil, false); rec.Code != 200 || !strings.Contains(rec.Body.String(), "There is no node yet") {
		t.Errorf("empty overview: %d", rec.Code)
	}
}

func TestAddEditRemoveNodeThroughTheUI(t *testing.T) {
	addr, fp, _ := startFakeAgent(t, "rem", "tok")
	d := newDashboard(t, true)

	// Step 1 hands out a token and the compose snippet.
	step1 := d.do("GET", "/nodes/new", nil, true).Body.String()
	if !strings.Contains(step1, "CUBEHERD_AGENT_TOKEN: chag_") || !strings.Contains(step1, `command: ["agent"]`) {
		t.Fatalf("step 1:\n%s", step1)
	}
	// A wrong token is reported without leaving the dialog.
	form := url.Values{"name": {"VPS Fra"}, "addr": {addr}, "token": {"wrong"}}
	if body := d.do("POST", "/nodes/probe", form, true).Body.String(); !strings.Contains(body, "rejected the token") || !strings.Contains(body, "hx-post=\"/nodes/probe\"") {
		t.Errorf("wrong token:\n%s", body)
	}
	// Right token: step 2 shows the fingerprint to confirm.
	form.Set("token", "tok")
	step2 := d.do("POST", "/nodes/probe", form, true).Body.String()
	if !strings.Contains(step2, fp) || !strings.Contains(step2, "Token accepted") {
		t.Fatalf("step 2:\n%s", step2)
	}
	// Without the confirmation nothing is stored.
	add := url.Values{"name": {"VPS Fra"}, "addr": {addr}, "token": {"tok"}, "fingerprint": {fp}}
	d.do("POST", "/nodes", add, true)
	if len(d.cfg.Get().Nodes) != 0 {
		t.Fatal("a node was added without confirming the fingerprint")
	}
	// A wrong fingerprint is refused even when confirmed.
	bad := url.Values{"name": {"VPS Fra"}, "addr": {addr}, "token": {"tok"}, "fingerprint": {"00:11"}, "confirm": {"on"}}
	d.do("POST", "/nodes", bad, true)
	if len(d.cfg.Get().Nodes) != 0 {
		t.Fatal("a node was added with a wrong fingerprint")
	}
	add.Set("confirm", "on")
	if rec := d.do("POST", "/nodes", add, true); rec.Header().Get("HX-Redirect") != "/nodes" {
		t.Fatalf("add: %d %v", rec.Code, rec.Header())
	}
	got := d.cfg.Get().Nodes
	if len(got) != 1 || got[0].ID != "vps-fra" || got[0].Token != "tok" || got[0].Fingerprint != fp {
		t.Fatalf("stored = %+v", got)
	}
	// Adding the same name again is refused.
	d.do("POST", "/nodes", add, true)
	if len(d.cfg.Get().Nodes) != 1 {
		t.Fatal("duplicate node stored")
	}
	if body := d.do("GET", "/nodes", nil, false).Body.String(); !strings.Contains(body, "VPS Fra") || !strings.Contains(body, "Online") {
		t.Errorf("nodes page:\n%s", body)
	}

	// Edit: per-node port range and addresses are stored, a bad range is not.
	edit := url.Values{"name": {"VPS Fra"}, "addr": {"https://" + addr}, "fingerprint": {fp}, "portMin": {"26000"}, "portMax": {"26100"}, "bindIps": {"10.0.0.5"}, "defaultBindIp": {"10.0.0.5"}}
	if rec := d.do("POST", "/nodes/vps-fra", edit, true); rec.Header().Get("HX-Redirect") != "/nodes" {
		t.Fatalf("edit: %d %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	if n := d.cfg.Get().Nodes[0]; n.PortMin != 26000 || n.PortMax != 26100 || n.Token != "tok" || len(n.BindIPs) != 1 || n.DefaultBindIP != "10.0.0.5" {
		t.Fatalf("edited node = %+v", n)
	}
	edit.Set("portMax", "100")
	d.do("POST", "/nodes/vps-fra", edit, true)
	if d.cfg.Get().Nodes[0].PortMax != 26100 {
		t.Fatal("an invalid port range was stored")
	}

	// The wizard of that node uses its own range and addresses.
	d.app.Cfg.Update(func(s *config.Settings) { s.PortMin, s.PortMax = 25565, 25600 })
	b := *d.app
	b.NodeID = "vps-fra"
	if pmin, pmax := b.portRange(); pmin != 26000 || pmax != 26100 {
		t.Errorf("node port range = %d–%d", pmin, pmax)
	}
	if ips := b.bindIPs(); len(ips) != 1 || ips[0] != "10.0.0.5" || b.defaultBindIP() != "10.0.0.5" {
		t.Errorf("node bind IPs = %v", ips)
	}
	local := *d.app
	local.NodeID = "local"
	if pmin, _ := local.portRange(); pmin != 25565 {
		t.Errorf("the local node must use the global range, got %d", pmin)
	}

	// Remove: the entry goes, nothing else.
	if rec := d.do("POST", "/nodes/vps-fra/remove", url.Values{}, true); rec.Header().Get("HX-Redirect") != "/nodes" {
		t.Fatalf("remove: %d", rec.Code)
	}
	if len(d.cfg.Get().Nodes) != 0 {
		t.Fatal("node still stored")
	}
	// The local node cannot be edited or removed.
	d.do("POST", "/nodes/local/remove", url.Values{}, true)
	if _, ok := d.reg.Get("local"); !ok {
		t.Fatal("the local node was removed")
	}
}

func TestSettingsPageWithoutLocalNode(t *testing.T) {
	d := newDashboard(t, false)
	rec := d.do("GET", "/settings", nil, false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "no Docker access of its own") {
		t.Errorf("settings without a local node: %d", rec.Code)
	}
}

func TestSidebarMarksTheCurrentServerOnItsNode(t *testing.T) {
	d := newDashboard(t, true)
	b := *d.app
	views := d.reg.Refresh(context.Background(), false, 0)
	out, err := b.renderString("overview", "rackList", Page{Nodes: views, RackCount: 4, Current: "loc-one", CurrentNode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	// One node: no group header. The current server is marked, others are not.
	if strings.Contains(out, "rack-group") || strings.Count(out, `aria-current="true"`) != 1 {
		t.Errorf("single node sidebar:\n%s", out)
	}
	addr, fp, _ := startFakeAgent(t, "rem", "tok")
	d = newDashboard(t, true, config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp})
	b = *d.app
	views = d.reg.Refresh(context.Background(), false, 0)
	// The same server id on another node must not be marked.
	out, _ = b.renderString("overview", "rackList", Page{Nodes: views, RackCount: 4, Current: "rem-one", CurrentNode: "vps"})
	if strings.Count(out, "rack-group") != 2 || strings.Count(out, `aria-current="true"`) != 1 || !strings.Contains(out, `href="/n/vps/s/rem-one" class="" aria-current="true"`) {
		t.Errorf("grouped sidebar:\n%s", out)
	}
}

func TestRenameTheLocalNode(t *testing.T) {
	d := newDashboard(t, true)
	if body := d.do("GET", "/nodes/local/edit", nil, true).Body.String(); !strings.Contains(body, `hx-post="/nodes/local"`) || strings.Contains(body, `name="addr"`) {
		t.Fatalf("local edit dialog:\n%s", body)
	}
	if rec := d.do("POST", "/nodes/local", url.Values{"name": {"Home Lab"}}, true); rec.Header().Get("HX-Redirect") != "/nodes" {
		t.Fatalf("rename: %d", rec.Code)
	}
	if got, _ := d.reg.Get("local"); got.Name != "Home Lab" {
		t.Fatalf("name = %q", got.Name)
	}
	d.do("POST", "/nodes/local", url.Values{"name": {""}}, true)
	if got, _ := d.reg.Get("local"); got.Name == "Home Lab" || got.Name == "" {
		t.Fatalf("an empty name must fall back to the host name, got %q", got.Name)
	}
}

func TestSettingsSayWhichNodesTheyApplyTo(t *testing.T) {
	// One node, nothing to distinguish: no chips.
	d := newDashboard(t, true)
	body := d.do("GET", "/settings", nil, false).Body.String()
	if strings.Contains(body, `class="scope`) || !strings.Contains(body, `name="portMin"`) {
		t.Error("a single-node dashboard must not show scope chips, and keeps its network fields")
	}

	// With remote nodes every section names its scope, and the network fields name the local node.
	addr, fp, _ := startFakeAgent(t, "rem", "tok")
	d = newDashboard(t, true, config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp})
	d.cfg.Update(func(s *config.Settings) { s.LocalName = "Home" })
	body = d.do("GET", "/settings", nil, false).Body.String()
	if got := strings.Count(body, `scope-all`); got != 4 {
		t.Errorf("%d \"All nodes\" chips, want 4 (language, sources, backup storage, defaults)", got)
	}
	if !strings.Contains(body, "The label shows which nodes a setting applies to.") {
		t.Error("the page subtitle does not explain the labels")
	}
	for _, want := range []string{`scope scope-local" title="For other nodes, set it under Nodes → Edit.">Only Home<`, `scope scope-dash">Dashboard only<`, "These apply to Home only."} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Port range and bind addresses live in the network section now, not among the defaults for every node.
	net, def := strings.Index(body, "Network interfaces"), strings.Index(body, "Defaults for new servers")
	if pm := strings.Index(body, `name="portMin"`); net < 0 || def < 0 || pm < net || pm > def {
		t.Errorf("the port range is not inside the network section (net %d, port %d, defaults %d)", net, pm, def)
	}

	// A dashboard without an embedded node has no network settings of its own.
	d = newDashboard(t, false, config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp})
	body = d.do("GET", "/settings", nil, false).Body.String()
	if strings.Contains(body, `name="portMin"`) || strings.Contains(body, `name="bindIps"`) || !strings.Contains(body, "does not manage a host of its own") {
		t.Error("without a local node the network section is only a pointer to the Nodes page")
	}
	if strings.Contains(body, "scope-local") {
		t.Error("a local-only chip without a local node")
	}
}

// Pages, live partials and node edits at the same time. Meaningful under -race.
func TestConcurrentRequestsWhileNodesChange(t *testing.T) {
	addr, fp, _ := startFakeAgent(t, "rem", "tok")
	d := newDashboard(t, true, config.Node{ID: "vps", Name: "VPS", Addr: addr, Token: "tok", Fingerprint: fp})
	done := make(chan struct{})
	var wg sync.WaitGroup
	paths := []string{"/", "/partials/overview", "/partials/rack?current=rem-one&node=vps", "/nodes", "/servers/new", "/settings", "/n/vps/s/missing"}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-done:
					return
				default:
				}
				p := paths[(i+n)%len(paths)]
				if rec := d.do("GET", p, nil, strings.HasPrefix(p, "/partials")); rec.Code >= 500 && rec.Code != http.StatusServiceUnavailable {
					t.Errorf("%s: %d", p, rec.Code)
				}
			}
		}()
	}
	for i := 0; i < 15; i++ {
		d.reg.Edit("vps", func(n *config.Node) { n.PortMin, n.PortMax = 26000+i, 26100 })
		d.cfg.Update(func(s *config.Settings) { s.Language = []string{"de", "en"}[i%2] })
		d.reg.ForgetPushed()
		if i%4 == 0 {
			d.do("POST", "/nodes/vps", url.Values{"name": {"VPS"}, "addr": {"https://" + addr}, "fingerprint": {fp}, "portMin": {"26000"}, "portMax": {"26100"}}, true)
		}
	}
	close(done)
	wg.Wait()
}

func (f *fakeHost) MetricsStatus(context.Context) (api.MetricsStatus, error) {
	return api.MetricsStatus{}, nil
}
func (f *fakeHost) NodeHistory(_ context.Context, from, to time.Time, _ int) (api.History, error) {
	return api.History{Scope: "node", From: from.Unix(), To: to.Unix()}, nil
}
func (f *fakeHost) ServerHistory(_ context.Context, id string, from, to time.Time, _ int) (api.History, error) {
	return api.History{Scope: id, From: from.Unix(), To: to.Unix()}, nil
}
func (f *fakeHost) MetricsLatest(context.Context) (map[string]api.MetricPoint, error) {
	return nil, nil
}
func (f *fakeHost) PurgeMetrics(context.Context, string) error { return nil }
