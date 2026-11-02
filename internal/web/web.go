// Package web serves the dashboard: server-rendered HTML, htmx partials and SSE streams.
package web

import (
	"bytes"
	"context"
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
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"cubeherd/internal/auth"
	"cubeherd/internal/config"
	"cubeherd/internal/i18n"
	"cubeherd/internal/node"
	"cubeherd/internal/nodes"
	"cubeherd/internal/sources"
	"cubeherd/internal/update"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Version is set at build time (-ldflags "-X cubeherd/internal/web.Version=0.2.4"); "dev" for plain go builds.
var Version = "dev"

type App struct {
	// M is the node this request works on. Handlers registered with h run on a per-request copy of App
	// whose M is the node named in the URL (/n/<node>/...), or the default node for the old /servers/... URLs.
	M        node.Node
	NodeID   string // "" on pages that are not scoped to one node
	NodeName string

	Reg     *nodes.Registry
	Sources *sources.Client
	Cfg     *config.Store
	DataDir string
	Listen  string
	Updates *update.Checker

	sess  auth.Sessions
	lim   *auth.Limiter
	pages map[string]map[string]*template.Template // language → page → templates
	start time.Time
}

func New(reg *nodes.Registry, src *sources.Client, cfg *config.Store, dataDir, listen string) (*App, error) {
	a := &App{Reg: reg, Sources: src, Cfg: cfg, DataDir: dataDir, Listen: listen, lim: auth.NewLimiter(), start: time.Now(), Updates: update.New()}
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
	displayLoc = func() *time.Location {
		if tz := cfg.Get().Timezone; tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				return loc
			}
		}
		return time.Local
	}
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
		for _, p := range []string{"login", "overview", "wizard", "server", "settings", "nodes", "nodepick", "nodedown"} {
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

// rackMaxAge is how old a remote node's answer may be when the sidebar is drawn.
const rackMaxAge = 3 * time.Second

// Handler wires all routes behind auth and CSRF checks.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(fileServer)))

	mux.HandleFunc("GET /login", a.p((*App).loginPage))
	mux.HandleFunc("POST /login", a.p((*App).loginPost))
	mux.HandleFunc("GET /setup", a.p((*App).setupPage))
	mux.HandleFunc("POST /setup", a.p((*App).setupPost))
	mux.HandleFunc("POST /logout", a.p((*App).logout))

	mux.HandleFunc("GET /{$}", a.p((*App).overview))
	mux.HandleFunc("GET /partials/overview", a.p((*App).overviewPartial))
	mux.HandleFunc("GET /partials/rack", a.p((*App).rackPartial))
	mux.HandleFunc("POST /import", a.h((*App).importPost))

	mux.HandleFunc("GET /servers/new", a.wizardEntry)
	mux.HandleFunc("GET /wizard/java", a.p((*App).wizardJava))
	mux.HandleFunc("GET /wizard/packs", a.p((*App).wizardPacks))
	mux.HandleFunc("GET /wizard/packversions", a.p((*App).wizardPackVersions))
	mux.HandleFunc("POST /wizard/preview", a.h((*App).wizardPreview))
	mux.HandleFunc("POST /servers", a.h((*App).createServer))

	mux.HandleFunc("GET /servers/{id}", a.h((*App).serverPage))
	mux.HandleFunc("GET /servers/{id}/events", a.h((*App).serverEvents))
	mux.HandleFunc("POST /servers/{id}/action/{action}", a.h((*App).serverAction))
	mux.HandleFunc("POST /servers/{id}/command", a.h((*App).serverCommand))
	mux.HandleFunc("POST /servers/{id}/settings", a.h((*App).serverSettings))
	mux.HandleFunc("POST /servers/{id}/players/{list}/{op}", a.h((*App).serverPlayers))
	mux.HandleFunc("GET /servers/{id}/update", a.h((*App).updateModal))
	mux.HandleFunc("POST /servers/{id}/update", a.h((*App).updatePost))
	mux.HandleFunc("GET /servers/{id}/files/list", a.h((*App).filesList))
	mux.HandleFunc("GET /servers/{id}/files/view", a.h((*App).filesView))
	mux.HandleFunc("GET /servers/{id}/files/download", a.h((*App).filesDownload))
	mux.HandleFunc("POST /servers/{id}/files/save", a.h((*App).filesSave))
	mux.HandleFunc("POST /servers/{id}/files/upload", a.h((*App).filesUpload))
	mux.HandleFunc("POST /servers/{id}/files/delete", a.h((*App).filesDelete))
	mux.HandleFunc("GET /servers/{id}/world.zip", a.h((*App).worldDownload))
	mux.HandleFunc("POST /servers/{id}/world", a.h((*App).worldUpload))
	mux.HandleFunc("GET /servers/{id}/addons/search", a.h((*App).addonSearch))
	mux.HandleFunc("POST /servers/{id}/addons/add", a.h((*App).addonAdd))
	mux.HandleFunc("POST /servers/{id}/addons/remove", a.h((*App).addonRemove))
	mux.HandleFunc("POST /servers/{id}/addons/upload", a.h((*App).addonUpload))
	mux.HandleFunc("POST /servers/{id}/addons/uploads/delete", a.h((*App).addonDeleteUpload))
	mux.HandleFunc("POST /servers/{id}/backups", a.h((*App).backupCreate))
	mux.HandleFunc("GET /servers/{id}/backups/list", a.h((*App).backupList))
	mux.HandleFunc("POST /servers/{id}/backups/plan", a.h((*App).backupPlan))
	mux.HandleFunc("GET /servers/{id}/backups/{name}", a.h((*App).backupDownload))
	mux.HandleFunc("POST /servers/{id}/backups/{name}/upload", a.h((*App).backupUpload))
	mux.HandleFunc("POST /servers/{id}/backups/{name}/verify", a.h((*App).backupVerify))
	mux.HandleFunc("POST /servers/{id}/backups/{name}/delete", a.h((*App).backupDelete))
	mux.HandleFunc("GET /servers/{id}/backups/{name}/restore", a.h((*App).restoreModal))
	mux.HandleFunc("POST /servers/{id}/backups/{name}/restore", a.h((*App).restorePost))
	mux.HandleFunc("GET /servers/{id}/uninstall", a.h((*App).uninstallModal))
	mux.HandleFunc("POST /servers/{id}/uninstall", a.h((*App).uninstallPost))

	mux.HandleFunc("GET /settings", a.p((*App).settingsPage))
	mux.HandleFunc("POST /settings", a.p((*App).settingsPost))
	mux.HandleFunc("POST /settings/password", a.p((*App).passwordPost))
	mux.HandleFunc("POST /settings/cf-check", a.p((*App).cfCheck))
	mux.HandleFunc("POST /settings/remote-check", a.p((*App).remoteCheck))
	mux.HandleFunc("POST /update/dismiss", a.p((*App).updateDismiss))

	mux.HandleFunc("GET /nodes", a.p((*App).nodesPage))
	mux.HandleFunc("GET /nodes/new", a.p((*App).nodeNew))
	mux.HandleFunc("POST /nodes/probe", a.p((*App).nodeProbe))
	mux.HandleFunc("POST /nodes", a.p((*App).nodeAdd))
	mux.HandleFunc("GET /nodes/{id}/edit", a.p((*App).nodeEdit))
	mux.HandleFunc("POST /nodes/{id}/probe", a.p((*App).nodeReprobe))
	mux.HandleFunc("POST /nodes/{id}", a.p((*App).nodeSave))
	mux.HandleFunc("GET /nodes/{id}/remove", a.p((*App).nodeRemoveModal))
	mux.HandleFunc("POST /nodes/{id}/remove", a.p((*App).nodeRemove))

	return a.security(a.nodePrefix(mux))
}

type nodeKey struct{}

// nodePrefix maps /n/<node>/... onto the plain routes and remembers the node. /n/<node>/s/... is /servers/...,
// so /n/vps/s/survival/events reaches the handler registered for /servers/{id}/events.
func (a *App) nodePrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/n/")
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		id, tail, _ := strings.Cut(rest, "/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		tail = "/" + tail
		switch {
		case tail == "/":
			redirect(w, r, "/?node="+url.QueryEscape(id))
			return
		case tail == "/s" || strings.HasPrefix(tail, "/s/"):
			tail = "/servers" + strings.TrimPrefix(tail, "/s")
		}
		r2 := r.Clone(context.WithValue(r.Context(), nodeKey{}, id))
		r2.URL.Path, r2.URL.RawPath = tail, ""
		next.ServeHTTP(w, r2)
	})
}

// explicitNode is the node named in the URL, if any.
func explicitNode(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(nodeKey{}).(string)
	return id, ok
}

// h runs a handler on the node the URL names (/n/<node>/...), else on the default node, which keeps the old
// /servers/... links working. Links in the rendered HTML are scoped to that node (see scopeLinks).
func (a *App) h(fn func(*App, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var info nodes.Info
		var ok bool
		if id, named := explicitNode(r); named {
			info, ok = a.Reg.Get(id)
		} else {
			info, ok = a.Reg.Default()
		}
		if !ok {
			if isHX(r) {
				a.fail(w, "Dieser Node existiert nicht.")
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if !info.Local {
			if st := a.Reg.Status(r.Context(), info.ID); st.State == nodes.Offline {
				a.nodeDown(w, r, info, st)
				return
			}
		}
		b := *a
		b.M, b.NodeID, b.NodeName = info.Node, info.ID, info.Name
		fn(&b, w, r)
	}
}

// nodeDown answers a request for a node that does not respond, without waiting for every call to time out.
func (a *App) nodeDown(w http.ResponseWriter, r *http.Request, info nodes.Info, st nodes.Status) {
	if isHX(r) {
		a.fail(w, "Der Node %s ist nicht erreichbar.", info.Name)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	a.render(w, r, "nodedown", Page{Title: info.Name, Nav: "nodes", Data: struct {
		Name   string
		Status nodes.Status
	}{info.Name, st}})
}

// p runs a handler that belongs to no single node. M is the default node, or nil when there is none.
func (a *App) p(fn func(*App, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := *a
		if info, ok := a.Reg.Default(); ok {
			b.M = info.Node
		}
		fn(&b, w, r)
	}
}

// srvURL is the address of a server page on the current node.
func (a *App) srvURL(id string) string {
	return "/n/" + url.PathEscape(a.NodeID) + "/s/" + url.PathEscape(id)
}

var scopeAttr = regexp.MustCompile(`\b(href|action|hx-get|hx-post|sse-connect)="(/servers(?:/[^"]*)?|/wizard/[^"]*|/import)"`)

// scopeLinks points the links of a node-scoped page at that node: /servers/x becomes /n/<node>/s/x. The
// templates keep writing plain /servers/... URLs; this is the one place that decides which node they mean.
// The overview and the sidebar write their per-server links explicitly because they mix nodes.
func (a *App) scopeLinks(html string) string {
	if a.NodeID == "" {
		return html
	}
	prefix := "/n/" + url.PathEscape(a.NodeID)
	return scopeAttr.ReplaceAllStringFunc(html, func(m string) string {
		sub := scopeAttr.FindStringSubmatch(m)
		path := sub[2]
		if path == "/servers/new" {
			return m // the node picker, not a page of this node
		}
		if path == "/servers" || strings.HasPrefix(path, "/servers/") {
			path = "/s" + strings.TrimPrefix(path, "/servers")
		}
		return sub[1] + `="` + prefix + path + `"`
	})
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

var publicPaths = map[string]bool{"/login": true, "/setup": true}

const (
	maxPublicBody     = 64 << 10 // POST /login and /setup carry a few short fields
	publicReadTimeout = 15 * time.Second
)

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
		if r.Method == http.MethodPost && publicPaths[r.URL.Path] {
			// Reachable without a session: keep the body small and the read short (uploads have their own limits).
			r.Body = http.MaxBytesReader(w, r.Body, maxPublicBody)
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(publicReadTimeout))
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
	Title       string
	Nav         string
	Nodes       []nodes.View // every node with its servers, for the sidebar
	RackCount   int
	Current     string // id of the server being shown ...
	CurrentNode string // ... and the node it is on
	Version     string
	Update      *update.Release // newer Cubeherd release, nil if none or dismissed
	Data        any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, page string, p Page) {
	p.Version = Version
	if page != "login" {
		p.Nodes = a.Reg.Refresh(r.Context(), false, rackMaxAge)
		for _, n := range p.Nodes {
			p.RackCount += len(n.Views)
		}
		if cfg := a.Cfg.Get(); !cfg.NoUpdateCheck {
			if rel := a.Updates.Available(Version); rel != nil && rel.Tag != cfg.DismissedUpdate {
				p.Update = rel
			}
		}
	}
	var buf bytes.Buffer
	if err := a.pages[a.lang()][page].ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "Darstellungsfehler: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(a.scopeLinks(buf.String())))
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
	w.Write([]byte(a.scopeLinks(buf.String())))
}

func (a *App) renderString(page, name string, data any) (string, error) {
	var buf bytes.Buffer
	err := a.pages[a.lang()][page].ExecuteTemplate(&buf, name, data)
	return a.scopeLinks(buf.String()), err
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
