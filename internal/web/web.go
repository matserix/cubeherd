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

	"cubeherd/internal/access"
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

	sess auth.Sessions
	lim  *auth.Limiter
	accs *accHolder
	sso  *ssoRuntime
	rw   *rateWindow
	// auditStdout, when set, also receives every audit entry as a JSON line.
	auditStdout func([]byte)
	routes      []route
	pages       map[string]map[string]*template.Template // language → page → templates
	start       time.Time
}

func New(reg *nodes.Registry, src *sources.Client, cfg *config.Store, dataDir, listen string) (*App, error) {
	a := &App{Reg: reg, Sources: src, Cfg: cfg, DataDir: dataDir, Listen: listen, lim: auth.NewLimiter(), accs: &accHolder{}, sso: newSSO(), rw: newRateWindow(), start: time.Now(), Updates: update.New()}
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
	if cfg.Get().Accounts {
		if err := a.openAccounts(); err != nil {
			return nil, err
		}
		if _, err := a.migrateOwner(); err != nil {
			return nil, err
		}
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
		for _, p := range []string{"login", "authpage", "twofa", "forbidden", "access", "account", "nodestats", "overview", "wizard", "server", "settings", "nodes", "nodepick", "nodedown"} {
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

// Handler wires all routes behind auth and CSRF checks. Every route is registered with the rule that guards it
// (see access.go); TestEveryRouteHasARule keeps it that way.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(fileServer)))
	rt := &router{a: a, mux: mux}

	rt.handle("GET /metrics", public(), a.p((*App).metricsEndpoint))
	rt.handle("GET /login", public(), a.p((*App).loginPage))
	rt.handle("POST /login", public(), a.p((*App).loginPost))
	rt.handle("GET /login/sso", public(), a.p((*App).ssoStart))
	rt.handle("GET /login/sso/callback", public(), a.p((*App).ssoCallback))
	rt.handle("GET /setup", public(), a.p((*App).setupPage))
	rt.handle("POST /setup", public(), a.p((*App).setupPost))
	rt.handle("POST /logout", open(), a.p((*App).logout))

	rt.handle("GET /{$}", open(), a.p((*App).overview))
	rt.handle("GET /partials/overview", open(), a.p((*App).overviewPartial))
	rt.handle("GET /partials/rack", open(), a.p((*App).rackPartial))
	rt.handle("POST /import", onNode(access.ServerCreate).log("server.import", form("name")), a.h((*App).importPost))

	rt.handle("GET /servers/new", nodeOrAnywhere(access.ServerCreate), a.wizardEntry)
	rt.handle("GET /wizard/java", anywhere(access.ServerCreate), a.p((*App).wizardJava))
	rt.handle("GET /wizard/packs", anywhere(access.ServerCreate), a.p((*App).wizardPacks))
	rt.handle("GET /wizard/packversions", anywhere(access.ServerCreate), a.p((*App).wizardPackVersions))
	rt.handle("POST /wizard/preview", anywhere(access.ServerCreate), a.h((*App).wizardPreview))
	rt.handle("POST /servers", onNode(access.ServerCreate).log("server.create", form("name")), a.h((*App).createServer))

	rt.handle("GET /servers/{id}", onServer(access.ServerView), a.h((*App).serverPage))
	rt.handle("GET /servers/{id}/events", onServer(access.ServerView), a.h((*App).serverEvents))
	rt.handle("POST /servers/{id}/action/{action}", onServer(access.ServerPower).log("server.power", pathValue("action")), a.h((*App).serverAction))
	rt.handle("POST /servers/{id}/command", onServer(access.ServerCommand).log("server.command", form("cmd")), a.h((*App).serverCommand))
	rt.handle("POST /servers/{id}/settings", onServer(access.ServerSettings).log("server.settings"), a.h((*App).serverSettings))
	rt.handle("POST /servers/{id}/players/{list}/{op}", onServer(access.ServerPlayers).log("server.players", playerTarget), a.h((*App).serverPlayers))
	rt.handle("GET /servers/{id}/update", onServer(access.ServerUpdate), a.h((*App).updateModal))
	rt.handle("POST /servers/{id}/update", onServer(access.ServerUpdate).log("server.update", form("version")), a.h((*App).updatePost))
	rt.handle("GET /servers/{id}/files/list", onServer(access.FilesRead), a.h((*App).filesList))
	rt.handle("GET /servers/{id}/files/view", onServer(access.FilesRead), a.h((*App).filesView))
	rt.handle("GET /servers/{id}/files/download", onServer(access.FilesRead).log("files.download", form("path")), a.h((*App).filesDownload))
	rt.handle("POST /servers/{id}/files/save", onServer(access.FilesWrite).log("files.save", form("path")), a.h((*App).filesSave))
	rt.handle("POST /servers/{id}/files/upload", onServer(access.FilesWrite).log("files.upload", form("path")), a.h((*App).filesUpload))
	rt.handle("POST /servers/{id}/files/delete", onServer(access.FilesWrite).log("files.delete", form("path")), a.h((*App).filesDelete))
	rt.handle("GET /servers/{id}/world.zip", onServer(access.WorldExport).log("world.export"), a.h((*App).worldDownload))
	rt.handle("POST /servers/{id}/world", onServer(access.WorldImport).log("world.import"), a.h((*App).worldUpload))
	rt.handle("GET /servers/{id}/addons/search", onServer(access.AddonsManage), a.h((*App).addonSearch))
	rt.handle("POST /servers/{id}/addons/add", onServer(access.AddonsManage).log("addons.add", form("name")), a.h((*App).addonAdd))
	rt.handle("POST /servers/{id}/addons/remove", onServer(access.AddonsManage).log("addons.remove", form("ref")), a.h((*App).addonRemove))
	rt.handle("POST /servers/{id}/addons/upload", onServer(access.AddonsManage).log("addons.upload"), a.h((*App).addonUpload))
	rt.handle("POST /servers/{id}/addons/uploads/delete", onServer(access.AddonsManage).log("addons.remove", form("file")), a.h((*App).addonDeleteUpload))
	rt.handle("POST /servers/{id}/backups", onServer(access.BackupsCreate).log("backup.create"), a.h((*App).backupCreate))
	rt.handle("GET /servers/{id}/backups/list", onServer(access.BackupsView), a.h((*App).backupList))
	rt.handle("POST /servers/{id}/backups/plan", onServer(access.BackupsCreate).log("backup.plan"), a.h((*App).backupPlan))
	rt.handle("GET /servers/{id}/backups/{name}", onServer(access.BackupsView).log("backup.download", pathValue("name")), a.h((*App).backupDownload))
	rt.handle("POST /servers/{id}/backups/{name}/upload", onServer(access.BackupsCreate).log("backup.upload", pathValue("name")), a.h((*App).backupUpload))
	rt.handle("POST /servers/{id}/backups/{name}/verify", onServer(access.BackupsView).log("backup.verify", pathValue("name")), a.h((*App).backupVerify))
	rt.handle("POST /servers/{id}/backups/{name}/delete", onServer(access.BackupsDelete).log("backup.delete", pathValue("name")), a.h((*App).backupDelete))
	rt.handle("GET /servers/{id}/backups/{name}/restore", onServer(access.BackupsRestore), a.h((*App).restoreModal))
	rt.handle("POST /servers/{id}/backups/{name}/restore", onServer(access.BackupsRestore).log("backup.restore", pathValue("name")), a.h((*App).restorePost))
	rt.handle("GET /servers/{id}/stats", onServer(access.StatsServer), a.h((*App).serverStats))
	rt.handle("GET /servers/{id}/uninstall", onServer(access.ServerDelete), a.h((*App).uninstallModal))
	rt.handle("POST /servers/{id}/uninstall", onServer(access.ServerDelete).log("server.delete"), a.h((*App).uninstallPost))

	rt.handle("GET /settings", global(access.SettingsManage), a.p((*App).settingsPage))
	rt.handle("POST /settings", global(access.SettingsManage).log("settings.change"), a.p((*App).settingsPost))
	rt.handle("POST /settings/password", open(), a.p((*App).passwordPost))
	rt.handle("POST /settings/prometheus", global(access.SettingsManage).log("prometheus.token", form("op")), a.p((*App).prometheusToken))
	rt.handle("POST /settings/accounts", global(access.SettingsManage), a.p((*App).enableAccounts))
	rt.handle("POST /settings/cf-check", global(access.SettingsManage), a.p((*App).cfCheck))
	rt.handle("POST /settings/remote-check", global(access.SettingsManage), a.p((*App).remoteCheck))
	rt.handle("POST /update/dismiss", global(access.SettingsManage), a.p((*App).updateDismiss))

	rt.handle("GET /stats", onNode(access.StatsNode), a.h((*App).nodeStats))
	rt.handle("POST /nodes/{id}/stats/purge", global(access.NodesManage).log("stats.purge", form("server")), a.p((*App).purgeStats))
	rt.handle("GET /nodes", anywhereOf(access.NodesView, access.StatsNode), a.p((*App).nodesPage))
	rt.handle("GET /nodes/new", global(access.NodesManage), a.p((*App).nodeNew))
	rt.handle("POST /nodes/probe", global(access.NodesManage), a.p((*App).nodeProbe))
	rt.handle("POST /nodes", global(access.NodesManage).log("node.add", form("name")), a.p((*App).nodeAdd))
	rt.handle("GET /nodes/{id}/edit", global(access.NodesManage), a.p((*App).nodeEdit))
	rt.handle("POST /nodes/{id}/probe", global(access.NodesManage), a.p((*App).nodeReprobe))
	rt.handle("POST /nodes/{id}", global(access.NodesManage).log("node.edit", pathValue("id")), a.p((*App).nodeSave))
	rt.handle("GET /nodes/{id}/remove", global(access.NodesManage), a.p((*App).nodeRemoveModal))
	rt.handle("POST /nodes/{id}/remove", global(access.NodesManage).log("node.remove", pathValue("id")), a.p((*App).nodeRemove))

	a.accessRoutes(rt)
	a.routes = rt.table
	return a.proxy(a.security(a.nodePrefix(mux)))
}

func playerTarget(r *http.Request) string {
	return r.PathValue("list") + "/" + r.PathValue("op") + ": " + strings.TrimSpace(r.FormValue("player"))
}

// SetAuditSink also sends every audit entry as a JSON line to f (for Docker log drivers).
func (a *App) SetAuditSink(f func([]byte)) {
	a.auditStdout = f
	if st := a.store(); st != nil {
		st.AuditStdout = f
	}
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
		if r.Method == http.MethodPost && isPublic(r.URL.Path) {
			// Reachable without a session: keep the body small and the read short (uploads have their own limits).
			r.Body = http.MaxBytesReader(w, r.Body, maxPublicBody)
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(publicReadTimeout))
		}
		setup := a.Cfg.Get().PasswordHash == ""
		if st := a.store(); st != nil {
			setup = st.UserCount() == 0
		}
		authed := false
		if !setup && !isPublic(r.URL.Path) {
			r, authed = a.authenticate(r)
		}
		switch {
		case setup && r.URL.Path != "/setup":
			redirect(w, r, "/setup")
			return
		case !setup && r.URL.Path == "/setup":
			redirect(w, r, "/")
			return
		case !setup && !isPublic(r.URL.Path) && !authed:
			redirect(w, r, "/login")
			return
		case authed && a.store() != nil:
			if to := a.twoFAGate(r); to != "" {
				redirect(w, r, to)
				return
			}
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
	Accounts    bool              // the multi-user mode is on
	TwoFABy     string            // set while the grace period for the second factor runs: the date it ends
	P           *access.Principal // the signed-in user's rights; nil in single-user mode (everything allowed)
	Account     string            // name of the signed-in user, "" in single-user mode
}

func (a *App) render(w http.ResponseWriter, r *http.Request, page string, p Page) {
	p.Version = Version
	p.P = principalOf(r)
	p.Accounts = a.store() != nil
	if p.P != nil {
		p.Account = p.P.User.Name
		if dl := a.twoFADeadline(p.P, a.store().SessionMethod(tokenOf(r))); !dl.IsZero() && time.Now().Before(dl) {
			p.TwoFABy = dl.Format("2006-01-02")
		}
	}
	if page != "login" && page != "authpage" {
		p.Nodes = a.refresh(r.Context(), false, rackMaxAge)
		for _, n := range p.Nodes {
			p.RackCount += len(n.Views)
		}
		if cfg := a.Cfg.Get(); !cfg.NoUpdateCheck && p.P.Global(access.SettingsManage) {
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
