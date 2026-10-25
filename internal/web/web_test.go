package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cubeherd/internal/auth"
	"cubeherd/internal/config"
	"cubeherd/internal/i18n"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
	"cubeherd/internal/sources"
	"cubeherd/internal/update"
)

func TestASCIIJSON(t *testing.T) {
	if got := asciiJSON("{\"msg\":\"hinzugef\u00fcgt \u2713\"}"); got != `{"msg":"hinzugef`+"\\"+`u00fcgt `+"\\"+`u2713"}` {
		t.Errorf("got %s", got)
	}
}

func TestDecimal(t *testing.T) {
	if decimal("de", 3.14) != "3,1" || decimal("en", 0.5) != "0.5" {
		t.Errorf("decimal: %s %s", decimal("de", 3.14), decimal("en", 0.5))
	}
}

func newTestApp(t *testing.T, password string) *App {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if password != "" {
		h, _ := auth.HashPassword(password)
		cfg.Update(func(s *config.Settings) { s.PasswordHash = h })
	}
	a := &App{Cfg: cfg, lim: auth.NewLimiter()}
	a.sess = auth.Sessions{Secret: func() (string, string) { return cfg.Get().SessionSecret, cfg.Get().PasswordHash }, Days: func() int { return 7 }}
	return a
}

func TestSecurityMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

	// Without a password every page leads to the setup.
	a := newTestApp(t, "")
	rec := httptest.NewRecorder()
	a.security(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Errorf("setup redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	a = newTestApp(t, "correct horse battery")
	// Not logged in → login.
	rec = httptest.NewRecorder()
	a.security(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/servers/x", nil))
	if rec.Header().Get("Location") != "/login" {
		t.Errorf("login redirect: %d", rec.Code)
	}
	// Cross-site POST is refused even with a valid session.
	sessRec := httptest.NewRecorder()
	a.sess.Issue(sessRec)
	cookie := sessRec.Result().Cookies()[0]
	req := httptest.NewRequest("POST", "/servers/x/action/stop", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://evil.example")
	rec = httptest.NewRecorder()
	a.security(ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("csrf: %d", rec.Code)
	}
	// Same-origin htmx request passes.
	req = httptest.NewRequest("POST", "/servers/x/action/stop", nil)
	req.AddCookie(cookie)
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	a.security(ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Errorf("htmx post: %d", rec.Code)
	}
	// Changing the password invalidates old sessions.
	h, _ := auth.HashPassword("another long password")
	a.Cfg.Update(func(s *config.Settings) { s.PasswordHash = h })
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if a.sess.Valid(req) {
		t.Error("session survived password change")
	}
}

// Templates only fail at execution time, so render the partials with representative data.
func TestRenderPartials(t *testing.T) {
	for _, l := range i18n.Languages {
		t.Run(l.Code, func(t *testing.T) { renderPartials(t, l.Code) })
	}
}

func renderPartials(t *testing.T, lang string) {
	a := newTestApp(t, "")
	a.Cfg.Update(func(s *config.Settings) { s.Language = lang })
	if err := a.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	s := &server.Server{ID: "atm", Name: "ATM", Kind: server.KindModrinth, Type: "NEOFORGE", Version: "1.21.1", MemoryGB: 8,
		Pack: &server.Pack{Name: "ATM10", VersionLabel: "4.15"}, Props: server.Props{MaxPlayers: 10}}
	for _, st := range []server.Status{server.StatusStopped, server.StatusRunning, server.StatusPreparing, server.StatusCrashed, server.StatusPaused, server.StatusUnhealthy, server.StatusInstalling} {
		v := manager.View{S: s, Status: st, Restarts: 2, CrashReason: "OutOfMemoryError", MemUsed: 3 << 30, AutoStopped: st == server.StatusStopped}
		for page, name := range map[string]any{"srvHead": serverPage{V: v, Disk: int64(5 << 30), Update: "x"}, "serverRow": v} {
			p := "server"
			if page == "serverRow" {
				p = "overview"
			}
			if _, err := a.renderString(p, page, name); err != nil {
				t.Errorf("%s/%s: %v", page, st, err)
			}
		}
	}
	if _, err := a.renderString("server", "stats", statsData{CPU: 12, Spark: spark([]float64{1, 5, 3}), MemUsed: 2 << 30, MemGB: 8}); err != nil {
		t.Error(err)
	}
	if _, err := a.renderString("server", "tabSettings", serverPage{V: manager.View{S: s}, MaxRAM: 16, HostRAM: "7,7", CPUOptions: []cpuOption{{"0", "Unbegrenzt"}, {"2", "2 Kerne"}}, CPUValue: "2"}); err != nil {
		t.Error(err)
	}
	bd := backupsData{ID: "atm", Plan: server.DefaultBackupPlan(), Busy: true, Next: time.Now().Add(time.Hour),
		List: []manager.Backup{{Name: "20261018-161712_manual.tar.zst", Time: time.Now(), Reason: "manual", Size: 12 << 20}}}
	if _, err := a.renderString("server", "tabBackups", bd); err != nil {
		t.Error(err)
	}
	// Backup health banner and verification column in every state.
	for _, lvl := range []string{"ok", "warn", "crit", "off", ""} {
		bd.Health = manager.Health{Level: lvl, Title: "t", Detail: "d"}
		bd.List[0].Check = map[string]string{"ok": "ok", "warn": "", "crit": "bad"}[lvl]
		if out, err := a.renderString("server", "backupList", bd); err != nil {
			t.Errorf("backupList %q: %v", lvl, err)
		} else if lvl == "off" && strings.Contains(out, `class="callout off"`) {
			t.Error("health level off must not render a banner")
		}
	}
	// Performance tab: every state, with and without per-dimension rows and percentiles.
	ps := []server.PerfSample{{TPS: 20, MSPT: 14.2, P95: 22, HaveP95: true, Dims: []server.PerfDim{{Name: "Overworld", TPS: 20, MSPT: 14.2}}}, {TPS: 9.7, MSPT: 102.5}}
	for _, reason := range []string{"loading", "stopped", "paused", "starting", "unsupported", "waiting"} {
		if _, err := a.renderString("server", "perfBody", a.perfView(s, nil, nil, 0, 0, reason, "boom")); err != nil {
			t.Errorf("perfBody %s: %v", reason, err)
		}
	}
	for _, n := range []int{1, 2} {
		if _, err := a.renderString("server", "perfBody", a.perfView(s, ps[:n], []float64{3, 8, 5}, 31, 7<<30, "", "")); err != nil {
			t.Errorf("perfBody samples=%d: %v", n, err)
		}
	}
	// A stuck server keeps the last known history on screen next to the timeout message.
	for _, reason := range []string{"timeout", "waiting"} {
		if out, err := a.renderString("server", "perfBody", a.perfView(s, ps[:1], nil, 3, 1<<30, reason, "x")); err != nil {
			t.Errorf("perfBody stale %s: %v", reason, err)
		} else if !strings.Contains(out, "Tick-Zeit") && lang == "de" {
			t.Errorf("stale %s lost the chart", reason)
		}
	}
	if _, err := a.renderString("server", "tabPerf", serverPage{Perf: a.perfView(s, nil, nil, 0, 0, "loading", "")}); err != nil {
		t.Error(err)
	}
	if _, err := a.renderString("server", "restoreModal", map[string]any{"S": s, "B": bd.List[0], "Pack": true}); err != nil {
		t.Error(err)
	}
	if _, err := a.renderString("server", "updateModal", updateData{S: s, Mode: "pack", Modes: []tab{{Key: "pack"}}}); err != nil {
		t.Error(err)
	}
	if _, err := a.renderString("server", "uninstallModal", map[string]any{"S": s, "Dir": "/x", "Archive": "/a"}); err != nil {
		t.Error(err)
	}
	s.Addons = []server.Addon{{Source: "modrinth", Ref: "lithium", Name: `Lith"ium`, IconURL: "https://x/y.png"}}
	s.Uploads = []string{"custom.jar"}
	ad := addonsData{S: s, Word: "Mods", Pending: true, Others: 1, Sources: []tab{{Key: "modrinth", Label: "Modrinth"}},
		Files: []manager.AddonFile{{Name: "custom.jar", Uploaded: true}, {Name: "x.jar"}}}
	if out, err := a.renderString("server", "tabMods", ad); err != nil {
		t.Error(err)
	} else if !strings.Contains(out, "&#34;ref&#34;:&#34;lithium&#34;") {
		t.Errorf("hx-vals not JSON-escaped as expected")
	}
	if _, err := a.renderString("server", "addonResults", map[string]any{"S": s, "Hits": []sources.AddonHit{{Source: "modrinth", Ref: "lithium", Title: "Lithium"}}, "Installed": map[string]bool{"modrinth:lithium": true}}); err != nil {
		t.Error(err)
	}
	td := treeData{ID: "atm", Path: "Meine Welt", Crumbs: crumbs("Meine Welt"), Entries: []manager.Entry{{Name: "a&b #1.txt", Size: 12}, {Name: "region", Dir: true}}}
	if out, err := a.renderString("server", "fileTree", td); err != nil {
		t.Error(err)
	} else if !strings.Contains(out, "path=Meine&#43;Welt%2Fa%26b&#43;%231.txt") {
		t.Errorf("file path not query-escaped:\n%s", out)
	}
	if _, err := a.renderString("server", "fileEditor", map[string]any{"ID": "atm", "Path": "server.properties", "Entry": manager.Entry{Name: "server.properties", Size: 10}, "Text": "a=b", "IsText": true, "Hint": "x", "Live": true}); err != nil {
		t.Error(err)
	}
	if _, err := a.renderString("server", "tabFiles", serverPage{V: manager.View{S: s}}); err != nil {
		t.Error(err)
	}
	if _, err := a.renderString("wizard", "preview", map[string]any{"S": s, "Compose": "x", "Dir": "/x"}); err != nil {
		t.Error(err)
	}
}

func TestSplitRCONError(t *testing.T) {
	in := "Unknown or incomplete command, see below for errorspark tps<--[HERE]"
	want := "Unknown or incomplete command, see below for error\nspark tps<--[HERE]"
	if got := splitRCONError(in); got != want {
		t.Fatalf("got %q", got)
	}
	if got := splitRCONError("There are 0 of a max of 10 players online:"); got != "There are 0 of a max of 10 players online:" {
		t.Fatalf("unrelated output changed: %q", got)
	}
}

func TestParseBindIPs(t *testing.T) {
	got, err := parseBindIPs("192.168.1.10\n10.8.0.1, 192.168.1.10\r\n0.0.0.0\nFD00:0:0:0:0:0:0:1")
	if err != nil || strings.Join(got, "|") != "192.168.1.10|10.8.0.1|fd00::1" {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parseBindIPs("10.0.0.1\nnot-an-ip"); err == nil || err.Error() != "not-an-ip" {
		t.Fatalf("want error naming the entry, got %v", err)
	}
	if got, err := parseBindIPs("  \n"); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
}

func TestUpdateBannerAndNetworkSettings(t *testing.T) {
	a := newTestApp(t, "")
	a.Cfg.Update(func(s *config.Settings) {
		s.Language = "en"
		s.BindIPs = []string{"192.168.1.10"}
		s.DefaultBindIP = "192.168.1.10"
	})
	if err := a.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	p := Page{Title: "x", Nav: "settings", Version: "0.5.1", Data: settingsData{Cfg: a.Cfg.Get()},
		Update: &update.Release{Tag: "v0.6.0", URL: "https://example.test/r", Notes: "- <b>new</b> thing"}}
	var b strings.Builder
	if err := a.pages["en"]["settings"].ExecuteTemplate(&b, "layout", p); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(b.String())
	for _, want := range []string{"Cubeherd v0.6.0 is available", "You are running version 0.5.1.", "- &lt;b&gt;new&lt;/b&gt; thing",
		`<option value="192.168.1.10" selected>`, "192.168.1.10\n</textarea>", `name="updateCheck" checked`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}
