package web

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/auth"
	"cubeherd/internal/config"
)

// accHolder lets the accounts store be switched on while the dashboard runs. App is copied per request, so the
// holder is shared by pointer.
type accHolder struct{ p atomic.Pointer[access.Store] }

// store is the accounts database, nil in single-user mode.
func (a *App) store() *access.Store {
	if a.accs == nil {
		return nil
	}
	return a.accs.p.Load()
}

type (
	principalKey struct{}
	tokenKey     struct{}
)

// principalOf is the signed-in user's rights; nil in single-user mode, where the one password holder may do everything.
func principalOf(r *http.Request) *access.Principal { return principalFrom(r.Context()) }

func principalFrom(ctx context.Context) *access.Principal {
	p, _ := ctx.Value(principalKey{}).(*access.Principal)
	return p
}

func tokenOf(r *http.Request) string {
	t, _ := r.Context().Value(tokenKey{}).(string)
	return t
}

// openAccounts opens the database. It is called at startup when accounts are on and when an owner switches them on.
func (a *App) openAccounts() error {
	if a.store() != nil {
		return nil
	}
	st, err := access.Open(a.DataDir + "/cubeherd.db")
	if err != nil {
		return err
	}
	if v := a.auditStdout; v != nil {
		st.AuditStdout = v
	}
	if a.accs == nil {
		a.accs = &accHolder{}
	}
	a.accs.p.Store(st)
	return nil
}

// migrateOwner turns the single password into the first owner account, so switching accounts on keeps you signed in.
func (a *App) migrateOwner() (access.User, error) {
	st := a.store()
	if st.UserCount() > 0 {
		return access.User{}, nil
	}
	hash := a.Cfg.Get().PasswordHash
	if hash == "" {
		return access.User{}, nil
	}
	id, err := st.CreateUser("admin", "", hash)
	if err != nil {
		return access.User{}, err
	}
	if err := st.GrantRoleToUser(id, access.RoleOwner, "system"); err != nil {
		return access.User{}, err
	}
	_ = st.Record(access.Entry{Actor: "system", Action: "accounts.migrated", Target: "admin"})
	return st.UserByID(id)
}

// ---------------- Authentication ----------------

func isPublic(path string) bool {
	switch path {
	case "/login", "/setup", "/login/sso", "/login/sso/callback", "/metrics", "/login/2fa", "/forgot", "/register", "/resend":
		return true
	}
	for _, p := range []string{"/reset/", "/verify/", "/notme/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// authenticate resolves the session. In accounts mode the request carries the user's rights on from here.
func (a *App) authenticate(r *http.Request) (*http.Request, bool) {
	st := a.store()
	if st == nil {
		return r, a.sess.Valid(r)
	}
	c, err := r.Cookie(auth.Cookie)
	if err != nil {
		return r, false
	}
	u, err := st.SessionUser(c.Value)
	if err != nil {
		return r, false
	}
	p, err := st.Principal(u)
	if err != nil {
		return r, false
	}
	ctx := context.WithValue(r.Context(), principalKey{}, p)
	ctx = context.WithValue(ctx, tokenKey{}, c.Value)
	return r.WithContext(ctx), true
}

func (a *App) sessionTTL() time.Duration {
	days := a.Cfg.Get().SessionDays
	if days <= 0 {
		days = 7
	}
	return time.Duration(days) * 24 * time.Hour
}

// startSession signs a user in: a new server-side session and its cookie.
func (a *App) startSession(w http.ResponseWriter, r *http.Request, u access.User) error {
	st := a.store()
	tok, err := st.NewSession(u.ID, clientAddr(r), r.UserAgent(), a.sessionTTL())
	if err != nil {
		return err
	}
	a.sess.Set(w, tok, a.secure(r))
	st.TouchLogin(u.ID)
	return nil
}

func clientAddr(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return strings.Trim(r.RemoteAddr, "[]") // the proxy middleware leaves the bare address
}

// ---------------- Authorisation ----------------

type kind int

const (
	kPublic         kind = iota + 1 // reachable without signing in
	kOpen                           // any signed-in user; the handler filters what it shows
	kGlobal                         // a permission on the whole dashboard
	kNode                           // a permission on the node the URL names (or the default node)
	kServer                         // a permission on the server the URL names
	kAnywhere                       // a permission on at least one scope
	kNodeOrAnywhere                 // kNode when the URL names a node, else kAnywhere
)

// rule says what a route needs and what of it is written to the audit log.
type rule struct {
	kind   kind
	perm   access.Perm
	alt    access.Perm                // kAnywhere: this permission also does
	alts   []access.Perm              // kServer: any of these does as well
	action string                     // audit action, "" = not logged
	target func(*http.Request) string // what the action was about
}

func public() rule                       { return rule{kind: kPublic} }
func open() rule                         { return rule{kind: kOpen} }
func global(p access.Perm) rule          { return rule{kind: kGlobal, perm: p} }
func onNode(p access.Perm) rule          { return rule{kind: kNode, perm: p} }
func onServer(p access.Perm) rule        { return rule{kind: kServer, perm: p} }
func anywhere(p access.Perm) rule        { return rule{kind: kAnywhere, perm: p} }
func anywhereOf(p, alt access.Perm) rule { return rule{kind: kAnywhere, perm: p, alt: alt} }
func onServerAny(p access.Perm, alts ...access.Perm) rule {
	return rule{kind: kServer, perm: p, alts: alts}
}
func nodeOrAnywhere(p access.Perm) rule { return rule{kind: kNodeOrAnywhere, perm: p} }
func (r rule) log(action string, target ...func(*http.Request) string) rule {
	r.action = action
	if len(target) > 0 {
		r.target = target[0]
	}
	return r
}

func form(name string) func(*http.Request) string {
	return func(r *http.Request) string { return strings.TrimSpace(r.FormValue(name)) }
}
func pathValue(name string) func(*http.Request) string {
	return func(r *http.Request) string { return r.PathValue(name) }
}

type route struct {
	Pattern string
	Rule    rule
}

// router registers every route together with the rule that guards it; there is no way to add one without a rule.
type router struct {
	a     *App
	mux   *http.ServeMux
	table []route
}

// accountsOnly lists the paths that exist only in the multi-user mode; without it they answer 404.
func accountsOnly(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	for _, p := range []string{"/access/", "/account/", "/forgot", "/reset/", "/register", "/resend", "/verify/", "/notme/", "/login/2fa"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (rt *router) handle(pattern string, rl rule, h http.HandlerFunc) {
	rt.table = append(rt.table, route{pattern, rl})
	if accountsOnly(pattern) {
		inner := h
		h = func(w http.ResponseWriter, r *http.Request) {
			if rt.a.store() == nil {
				http.NotFound(w, r)
				return
			}
			inner(w, r)
		}
	}
	rt.mux.HandleFunc(pattern, rt.a.guard(rl, h))
}

// nodeOf is the node a request is about: the one in the URL, else the default.
func (a *App) nodeOf(r *http.Request) string {
	if id, ok := explicitNode(r); ok {
		return id
	}
	if info, ok := a.Reg.Default(); ok {
		return info.ID
	}
	return ""
}

func (a *App) allowed(p *access.Principal, rl rule, r *http.Request) bool {
	switch rl.kind {
	case kPublic, kOpen:
		return true
	case kGlobal:
		return p.Global(rl.perm)
	case kNode:
		return p.Can(rl.perm, a.nodeOf(r), "")
	case kServer:
		node, id := a.nodeOf(r), r.PathValue("id")
		if p.Can(rl.perm, node, id) {
			return true
		}
		for _, alt := range rl.alts {
			if p.Can(alt, node, id) {
				return true
			}
		}
		return false
	case kAnywhere:
		return p.Anywhere(rl.perm) || rl.alt != "" && p.Anywhere(rl.alt)
	case kNodeOrAnywhere:
		if id, ok := explicitNode(r); ok {
			return p.Can(rl.perm, id, "")
		}
		return p.Anywhere(rl.perm)
	}
	return false
}

func (a *App) guard(rl rule, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := a.store()
		if st == nil || rl.kind == kPublic {
			next(w, r)
			return
		}
		p := principalOf(r)
		if !a.allowed(p, rl, r) {
			a.deny(w, r, rl, p)
			return
		}
		if rl.action == "" {
			next(w, r)
			return
		}
		pre := a.entityName(rl.action, r.PathValue("id")) // looked up before the handler may delete it
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r)
		out := access.OK
		if sw.failed {
			out = access.Fail
		}
		target := pre
		if target == "" && rl.target != nil {
			target = rl.target(r)
		}
		a.record(r, rl.action, target, out)
	}
}

// deny answers a request the user may not make. A server the user cannot see does not exist for them.
func (a *App) deny(w http.ResponseWriter, r *http.Request, rl rule, p *access.Principal) {
	a.record(r, string(rl.perm), r.Method+" "+r.URL.Path, access.Denied)
	if rl.kind == kServer && !p.Can(access.ServerView, a.nodeOf(r), r.PathValue("id")) {
		if isHX(r) {
			a.fail(w, "Server nicht gefunden.")
		} else {
			http.NotFound(w, r)
		}
		return
	}
	if isHX(r) {
		a.fail(w, "Dafür fehlt dir die Berechtigung.")
		return
	}
	w.WriteHeader(http.StatusForbidden)
	a.render(w, r, "forbidden", Page{Title: a.t("Kein Zugriff"), Nav: "forbidden", Data: string(rl.perm)})
}

// record writes an audit entry for the request's user; it does nothing in single-user mode.
func (a *App) record(r *http.Request, action, target, outcome string, detail ...any) {
	st := a.store()
	if st == nil {
		return
	}
	e := access.Entry{Action: action, Target: target, Outcome: outcome, IP: clientAddr(r), Detail: access.Detail(detail...)}
	if p := principalOf(r); p != nil {
		e.ActorID, e.Actor = p.User.ID, p.User.Name
	}
	if strings.HasPrefix(r.URL.Path, "/servers/") || strings.HasPrefix(r.URL.Path, "/nodes/") {
		e.Node = a.nodeOf(r)
		e.Server = r.PathValue("id")
		if strings.HasPrefix(r.URL.Path, "/nodes/") {
			e.Node, e.Server = r.PathValue("id"), ""
		}
	} else if _, ok := explicitNode(r); ok || r.URL.Path == "/servers" || r.URL.Path == "/import" {
		e.Node = a.nodeOf(r)
	}
	if err := st.Record(e); err != nil {
		log.Printf("audit: %v", err)
	}
}

// recordAs writes an entry for a named actor outside a signed-in request (sign-in attempts, the CLI).
func (a *App) recordAs(r *http.Request, actor access.User, name, action, target, outcome string, detail ...any) {
	st := a.store()
	if st == nil {
		return
	}
	if name == "" {
		name = actor.Name
	}
	if err := st.Record(access.Entry{ActorID: actor.ID, Actor: name, IP: clientAddr(r), Action: action, Target: target, Outcome: outcome, Detail: access.Detail(detail...)}); err != nil {
		log.Printf("audit: %v", err)
	}
}

// statusWriter notes whether a response is an error, either by status or by the failure toast the handlers send.
type statusWriter struct {
	http.ResponseWriter
	wrote  bool
	failed bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.wrote = true
		h := s.Header()
		s.failed = code >= 400 || (h.Get("HX-Reswap") == "none" && strings.Contains(h.Get("HX-Trigger"), "crit"))
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ---------------- Account mode handlers ----------------

var errBadLogin = errors.New("login")

// loginAccounts checks name and password and starts a session.
func (a *App) loginAccounts(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	name := strings.TrimSpace(r.FormValue("user"))
	show := func(msg string) {
		d := a.loginData("/login")
		d.Error, d.Name = msg, name
		a.render(w, r, "login", Page{Nav: "login", Data: d})
	}
	if d := a.lim.Wait(r); d > 0 {
		show(a.t("Zu viele Versuche. Bitte %d Sekunden warten.", int(d.Seconds())+1))
		return
	}
	if d := a.lim.WaitKey("user:" + strings.ToLower(name)); d > 0 {
		show(a.t("Zu viele Versuche. Bitte %d Sekunden warten.", int(d.Seconds())+1))
		return
	}
	if st.LocalSignInOff() {
		if ps, _ := st.Providers(); len(ps) > 0 {
			show(a.t("Die Anmeldung mit Passwort ist ausgeschaltet. Bitte melde dich über den Anbieter an."))
			return
		}
	}
	u, hash, err := st.UserByName(name)
	ok := err == nil && !u.Disabled && auth.CheckPassword(hash, r.FormValue("password"))
	if err != nil {
		auth.CheckPassword(dummyHash, r.FormValue("password")) // same time whether or not the name exists
	}
	if !ok {
		a.lim.Fail(r)
		a.lim.FailKey("user:" + strings.ToLower(name))
		time.Sleep(400 * time.Millisecond)
		reason := "wrong password"
		switch {
		case err != nil:
			reason = "unknown user"
		case u.Disabled:
			reason = "account disabled"
		}
		a.recordAs(r, u, name, "login.fail", name, access.Fail, "reason", reason)
		show(a.t("Name oder Passwort ist falsch."))
		return
	}
	a.lim.Reset(r)
	a.lim.ResetKey("user:" + strings.ToLower(name))
	a.afterFirstFactor(w, r, u, func(msg, hint string) {
		d := a.loginData("/login")
		d.Error, d.Name, d.Hint = msg, name, hint
		a.render(w, r, "login", Page{Nav: "login", Data: d})
	})
}

// dummyHash is compared against when a name is unknown, so timing does not reveal which names exist.
var dummyHash, _ = auth.HashPassword("cubeherd-dummy-password")

// setupAccounts creates the first owner on a fresh install that starts in accounts mode.
func (a *App) setupAccounts(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	name := strings.TrimSpace(r.FormValue("user"))
	if name == "" {
		name = "admin"
	}
	fail := func(msg string) {
		d := a.loginData("/setup")
		d.Setup, d.Error, d.Name = true, msg, name
		a.render(w, r, "login", Page{Nav: "login", Data: d})
	}
	pw := r.FormValue("password")
	if len(pw) < 10 {
		fail(a.t("Das Passwort braucht mindestens 10 Zeichen."))
		return
	}
	if pw != r.FormValue("password2") {
		fail(a.t("Die Passwörter stimmen nicht überein."))
		return
	}
	if st.UserCount() > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	hash, err := auth.HashPassword(pw)
	var id int64
	if err == nil {
		id, err = st.CreateUser(name, "", hash)
	}
	if err == nil {
		err = st.GrantRoleToUser(id, access.RoleOwner, "setup")
	}
	if err != nil {
		fail(err.Error())
		return
	}
	u, _ := st.UserByID(id)
	if err := a.startSession(w, r, u); err != nil {
		fail(err.Error())
		return
	}
	a.recordAs(r, u, "", "accounts.setup", u.Name, access.OK)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// enableAccounts is the settings action that switches the multi-user mode on.
func (a *App) enableAccounts(w http.ResponseWriter, r *http.Request) {
	if a.store() != nil {
		redirect(w, r, "/access/users")
		return
	}
	if !auth.CheckPassword(a.Cfg.Get().PasswordHash, r.FormValue("current")) {
		a.lim.Fail(r)
		a.fail(w, "Das aktuelle Passwort ist falsch.")
		return
	}
	if err := a.openAccounts(); err != nil {
		a.fail(w, "Die Datenbank konnte nicht geöffnet werden: %s", err.Error())
		return
	}
	u, err := a.migrateOwner()
	if err == nil {
		err = a.Cfg.Update(func(s *config.Settings) { s.Accounts = true })
	}
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	if u.ID != 0 {
		_ = a.startSession(w, r, u)
	}
	redirect(w, r, "/access/users")
}

// entityName names the user, group, role or node an access action is about, so the log reads "user.delete carla"
// instead of "user.delete 4".
func (a *App) entityName(action, id string) string {
	st := a.store()
	n, err := strconv.ParseInt(id, 10, 64)
	switch {
	case strings.HasPrefix(action, "node."):
		return a.nodeName(id)
	case st == nil || err != nil:
		return ""
	case strings.HasPrefix(action, "user."):
		if u, err := st.UserByID(n); err == nil {
			return u.Name
		}
	case strings.HasPrefix(action, "group."):
		gs, _ := st.ListGroups()
		for _, g := range gs {
			if g.ID == n {
				return g.Name
			}
		}
	case strings.HasPrefix(action, "role."):
		if r, err := st.Role(n); err == nil {
			return r.Name
		}
	}
	return ""
}
