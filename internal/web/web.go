// Package web serves the dashboard: server-rendered HTML, htmx partials and SSE streams.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf16"

	"cubeherd/internal/auth"
	"cubeherd/internal/config"
	"cubeherd/internal/host"
	"cubeherd/internal/i18n"
	"cubeherd/internal/manager"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const Version = "0.1.0"

type App struct {
	M       *manager.Manager
	Cfg     *config.Store
	Host    *host.Reader
	DataDir string
	Listen  string

	sess  auth.Sessions
	lim   *auth.Limiter
	pages map[string]map[string]*template.Template // language → page → templates
	start time.Time
}

func New(m *manager.Manager, cfg *config.Store, h *host.Reader, dataDir, listen string) (*App, error) {
	a := &App{M: m, Cfg: cfg, Host: h, DataDir: dataDir, Listen: listen, lim: auth.NewLimiter(), start: time.Now()}
	a.sess = auth.Sessions{
		Secret: func() (string, string) {
			s := cfg.Get()
			ph := s.PasswordHash
			if len(ph) > 20 {
				ph = ph[len(ph)-20:]
			}
			return s.SessionSecret, ph
		},
		Days: func() int { return cfg.Get().SessionDays },
	}
	displayLoc = m.Loc
	if err := a.loadTemplates(); err != nil {
		return nil, err
	}
	return a, nil
}

// loadTemplates parses one template set per language, each with its own formatting functions.
func (a *App) loadTemplates() error {
	a.pages = map[string]map[string]*template.Template{}
	for _, l := range i18n.Languages {
		base, err := template.New("base").Funcs(newFuncs(l.Code)).ParseFS(templateFS, "templates/layout.html", "templates/partials.html")
		if err != nil {
			return err
		}
		a.pages[l.Code] = map[string]*template.Template{}
		for _, p := range []string{"login", "overview", "wizard", "server", "settings"} {
			t, err := base.Clone()
			if err != nil {
				return err
			}
			if _, err := t.ParseFS(templateFS, "templates/"+p+".html"); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			a.pages[l.Code][p] = t
		}
	}
	return nil
}

// lang is the interface language; installs from before i18n have none set and stay German.
func (a *App) lang() string {
	if l := a.Cfg.Get().Language; i18n.Valid(l) {
		return l
	}
	return i18n.Source
}

// t translates a German source text into the interface language.
func (a *App) t(key string, args ...any) string { return i18n.T(a.lang(), key, args...) }

// Handler wires all routes behind auth and CSRF checks.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(fileServer)))

	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.loginPost)
	mux.HandleFunc("GET /setup", a.setupPage)
	mux.HandleFunc("POST /setup", a.setupPost)
	mux.HandleFunc("POST /logout", a.logout)

	mux.HandleFunc("GET /{$}", a.overview)
	mux.HandleFunc("GET /partials/overview", a.overviewPartial)
	mux.HandleFunc("POST /import", a.importPost)

	mux.HandleFunc("GET /servers/new", a.wizardPage)
	mux.HandleFunc("GET /wizard/java", a.wizardJava)
	mux.HandleFunc("GET /wizard/packs", a.wizardPacks)
	mux.HandleFunc("GET /wizard/packversions", a.wizardPackVersions)
	mux.HandleFunc("POST /wizard/preview", a.wizardPreview)
	mux.HandleFunc("POST /servers", a.createServer)

	mux.HandleFunc("GET /servers/{id}", a.serverPage)
	mux.HandleFunc("GET /servers/{id}/events", a.serverEvents)
	mux.HandleFunc("POST /servers/{id}/action/{action}", a.serverAction)
	mux.HandleFunc("POST /servers/{id}/command", a.serverCommand)
	mux.HandleFunc("POST /servers/{id}/settings", a.serverSettings)
	mux.HandleFunc("POST /servers/{id}/players/{list}/{op}", a.serverPlayers)
	mux.HandleFunc("GET /servers/{id}/update", a.updateModal)
	mux.HandleFunc("POST /servers/{id}/update", a.updatePost)
	mux.HandleFunc("GET /servers/{id}/files/list", a.filesList)
	mux.HandleFunc("GET /servers/{id}/files/view", a.filesView)
	mux.HandleFunc("GET /servers/{id}/files/download", a.filesDownload)
	mux.HandleFunc("POST /servers/{id}/files/save", a.filesSave)
	mux.HandleFunc("POST /servers/{id}/files/upload", a.filesUpload)
	mux.HandleFunc("POST /servers/{id}/files/delete", a.filesDelete)
	mux.HandleFunc("GET /servers/{id}/world.zip", a.worldDownload)
	mux.HandleFunc("POST /servers/{id}/world", a.worldUpload)
	mux.HandleFunc("GET /servers/{id}/addons/search", a.addonSearch)
	mux.HandleFunc("POST /servers/{id}/addons/add", a.addonAdd)
	mux.HandleFunc("POST /servers/{id}/addons/remove", a.addonRemove)
	mux.HandleFunc("POST /servers/{id}/addons/upload", a.addonUpload)
	mux.HandleFunc("POST /servers/{id}/addons/uploads/delete", a.addonDeleteUpload)
	mux.HandleFunc("POST /servers/{id}/backups", a.backupCreate)
	mux.HandleFunc("GET /servers/{id}/backups/list", a.backupList)
	mux.HandleFunc("POST /servers/{id}/backups/plan", a.backupPlan)
	mux.HandleFunc("GET /servers/{id}/backups/{name}", a.backupDownload)
	mux.HandleFunc("POST /servers/{id}/backups/{name}/upload", a.backupUpload)
	mux.HandleFunc("POST /servers/{id}/backups/{name}/delete", a.backupDelete)
	mux.HandleFunc("GET /servers/{id}/backups/{name}/restore", a.restoreModal)
	mux.HandleFunc("POST /servers/{id}/backups/{name}/restore", a.restorePost)
	mux.HandleFunc("GET /servers/{id}/uninstall", a.uninstallModal)
	mux.HandleFunc("POST /servers/{id}/uninstall", a.uninstallPost)

	mux.HandleFunc("GET /settings", a.settingsPage)
	mux.HandleFunc("POST /settings", a.settingsPost)
	mux.HandleFunc("POST /settings/password", a.passwordPost)
	mux.HandleFunc("POST /settings/cf-check", a.cfCheck)
	mux.HandleFunc("POST /settings/remote-check", a.remoteCheck)

	return a.security(mux)
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

var publicPaths = map[string]bool{"/login": true, "/setup": true}

func (a *App) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")

		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		// CSRF: state-changing requests must come from htmx or carry our own origin.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, a.t("Ungültige Herkunft der Anfrage"), http.StatusForbidden)
			return
		}
		setup := a.Cfg.Get().PasswordHash == ""
		switch {
		case setup && r.URL.Path != "/setup":
			redirect(w, r, "/setup")
			return
		case !setup && r.URL.Path == "/setup":
			redirect(w, r, "/")
			return
		case !setup && !publicPaths[r.URL.Path] && !a.sess.Valid(r):
			redirect(w, r, "/login")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	return r.Header.Get("HX-Request") == "true"
}

func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if isHX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// ---------------- Rendering ----------------

type Page struct {
	Title   string
	Nav     string
	Rack    []manager.View
	Current string
	Version string
	Data    any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, page string, p Page) {
	p.Version = Version
	if page != "login" {
		p.Rack, _ = a.M.Views(r.Context(), false)
	}
	var buf bytes.Buffer
	if err := a.pages[a.lang()][page].ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "Darstellungsfehler: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// partial renders a named template (from any page set; partials are shared).
func (a *App) partial(w http.ResponseWriter, page, name string, data any) {
	var buf bytes.Buffer
	if err := a.pages[a.lang()][page].ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("partial %s/%s: %v", page, name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func (a *App) renderString(page, name string, data any) (string, error) {
	var buf bytes.Buffer
	err := a.pages[a.lang()][page].ExecuteTemplate(&buf, name, data)
	return buf.String(), err
}

// toast adds an HX-Trigger header that app.js turns into a toast.
func toast(w http.ResponseWriter, msg, kind string, extra ...string) {
	ev := map[string]any{"fc-toast": map[string]string{"msg": msg, "kind": kind}}
	for _, e := range extra {
		ev[e] = true
	}
	b, _ := json.Marshal(ev)
	w.Header().Set("HX-Trigger", asciiJSON(string(b)))
}

// asciiJSON escapes non-ASCII runes; browsers decode header values as Latin-1.
func asciiJSON(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 128:
			b.WriteRune(r)
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, "\\u%04x\\u%04x", r1, r2)
		default:
			fmt.Fprintf(&b, "\\u%04x", r)
		}
	}
	return b.String()
}

// fail reports a translated error as toast without swapping content.
func (a *App) fail(w http.ResponseWriter, key string, args ...any) { fail(w, a.t(key, args...)) }

// toast shows a translated message.
func (a *App) toast(w http.ResponseWriter, kind, key string, args ...any) {
	toast(w, a.t(key, args...), kind)
}

// fail reports an error as toast without swapping content.
func fail(w http.ResponseWriter, msg string) {
	toast(w, msg, "crit")
	w.Header().Set("HX-Reswap", "none")
	w.WriteHeader(http.StatusOK)
}

// assetVersion changes whenever an embedded asset changes, so "immutable" caching is safe.
var assetVersion = func() string {
	h := sha256.New()
	_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

func staticURL(p string) string { return path.Join("/static", p) + "?v=" + assetVersion }
