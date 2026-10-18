package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"forgecraft/internal/auth"
	"forgecraft/internal/config"
	"forgecraft/internal/manager"
	"forgecraft/internal/server"
)

func TestASCIIJSON(t *testing.T) {
	if got := asciiJSON("{\"msg\":\"hinzugef\u00fcgt \u2713\"}"); got != `{"msg":"hinzugef`+"\\"+`u00fcgt `+"\\"+`u2713"}` {
		t.Errorf("got %s", got)
	}
}

func TestGB(t *testing.T) {
	if gb(int64(3<<30)) != "3,0" || gb(uint64(1<<29)) != "0,5" {
		t.Errorf("gb: %s %s", gb(int64(3<<30)), gb(uint64(1<<29)))
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
	a := &App{}
	if err := a.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	s := &server.Server{ID: "atm", Name: "ATM", Kind: server.KindModrinth, Type: "NEOFORGE", Version: "1.21.1", MemoryGB: 8,
		Pack: &server.Pack{Name: "ATM10", VersionLabel: "4.15"}, Props: server.Props{MaxPlayers: 10}}
	for _, st := range []server.Status{server.StatusRunning, server.StatusPreparing, server.StatusCrashed, server.StatusPaused, server.StatusUnhealthy, server.StatusInstalling} {
		v := manager.View{S: s, Status: st, Restarts: 2, CrashReason: "OutOfMemoryError", MemUsed: 3 << 30}
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
	if _, err := a.renderString("server", "tabSettings", serverPage{V: manager.View{S: s}, MaxRAM: 16, HostRAM: "7,7"}); err != nil {
		t.Error(err)
	}
	bd := backupsData{ID: "atm", Plan: server.DefaultBackupPlan(), Busy: true, Next: time.Now().Add(time.Hour),
		List: []manager.Backup{{Name: "20261018-161712_manual.tar.zst", Time: time.Now(), Reason: "manual", Size: 12 << 20}}}
	if _, err := a.renderString("server", "tabBackups", bd); err != nil {
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
	if _, err := a.renderString("wizard", "preview", map[string]any{"S": s, "Compose": "x", "Dir": "/x"}); err != nil {
		t.Error(err)
	}
}
