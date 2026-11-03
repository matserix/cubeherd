package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/api"
	"cubeherd/internal/auth"
	"cubeherd/internal/server"
)

// newAccounts is a dashboard in multi-user mode with a fake local node (servers loc-one and loc-two). It returns
// the owner "admin" as well.
func newAccounts(t *testing.T) (*dashboard, *access.Store) {
	t.Helper()
	d := newDashboard(t, true)
	if err := d.app.openAccounts(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.app.migrateOwner(); err != nil {
		t.Fatal(err)
	}
	st := d.app.store()
	t.Cleanup(func() { st.Close() })
	return d, st
}

func (d *dashboard) cookieFor(t *testing.T, st *access.Store, name string) *http.Cookie {
	t.Helper()
	u, _, err := st.UserByName(name)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.NewSession(u.ID, "127.0.0.1", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: auth.Cookie, Value: tok}
}

func (d *dashboard) as(c *http.Cookie, method, path string, form url.Values, hx bool) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(c)
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	return rec
}

func addUser(t *testing.T, st *access.Store, name string) access.User {
	t.Helper()
	id, err := st.CreateUser(name, name+"@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.UserByID(id)
	return u
}

func giveRole(t *testing.T, st *access.Store, u access.User, role string, sc access.Scope) {
	t.Helper()
	rs, _ := st.ListRoles()
	for _, r := range rs {
		if r.Name == role {
			if _, err := st.AddGrant(access.Grant{PrincipalID: u.ID, RoleID: r.ID, Scope: sc, CreatedBy: "test"}); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no role %s", role)
}

// TestEveryRouteHasARule keeps the table the only way to register a route: a handler added straight to the mux
// would bypass the permission check.
func TestEveryRouteHasARule(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	handle, handleFunc := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		handle += strings.Count(string(b), "mux.Handle(")
		handleFunc += strings.Count(string(b), "mux.HandleFunc(")
	}
	if handle != 1 || handleFunc != 1 {
		t.Fatalf("routes must go through router.handle: found %d mux.Handle (want the static files only) and %d mux.HandleFunc (want the one in router.handle)", handle, handleFunc)
	}
	d, _ := newAccounts(t)
	if len(d.app.routes) < 60 {
		t.Fatalf("only %d routes registered", len(d.app.routes))
	}
	for _, r := range d.app.routes {
		switch r.Rule.kind {
		case kPublic, kOpen:
		case kGlobal, kNode, kServer, kAnywhere, kNodeOrAnywhere:
			if !access.Valid(r.Rule.perm) {
				t.Errorf("%s: unknown permission %q", r.Pattern, r.Rule.perm)
			}
		default:
			t.Errorf("%s has no rule", r.Pattern)
		}
		if r.Rule.kind == kServer && !strings.Contains(r.Pattern, "{id}") {
			t.Errorf("%s needs a server but has no {id}", r.Pattern)
		}
	}
}

var pathVar = regexp.MustCompile(`\{[a-z]+\}`)

// TestRoutesRefuseAnAccountWithoutGrants calls every guarded route as a user who holds nothing: none may answer
// with content or run a handler.
func TestRoutesRefuseAnAccountWithoutGrants(t *testing.T) {
	d, st := newAccounts(t)
	addUser(t, st, "nobody")
	c := d.cookieFor(t, st, "nobody")
	n := 0
	for _, r := range d.app.routes {
		if r.Rule.kind == kPublic || r.Rule.kind == kOpen {
			continue
		}
		method, path, _ := strings.Cut(r.Pattern, " ")
		path = strings.TrimSpace(path)
		path = strings.NewReplacer("{id}", "loc-one", "{name}", "x", "{action}", "start", "{list}", "whitelist", "{op}", "add", "{hash}", "x").Replace(path)
		path = pathVar.ReplaceAllString(path, "1")
		if path == "/{$}" {
			continue
		}
		rec := d.as(c, method, path, url.Values{}, false)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d for a user without grants", method, path, rec.Code)
		}
		n++
	}
	if n < 50 {
		t.Fatalf("only %d routes were exercised", n)
	}
}

func TestViewerSeesAndDoesOnlyWhatTheRoleAllows(t *testing.T) {
	d, st := newAccounts(t)
	u := addUser(t, st, "vera")
	giveRole(t, st, u, access.RoleViewer, access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"})
	c := d.cookieFor(t, st, "vera")

	home := d.as(c, "GET", "/", nil, false).Body.String()
	if !strings.Contains(home, "loc-one") || strings.Contains(home, "loc-two") {
		t.Fatalf("overview should show only the granted server")
	}
	// what the server page offers follows the role: a viewer gets no buttons and only the console and performance tabs
	p := principal(t, st, u)
	caps := capsOf(p, "local", "loc-one")
	head, err := d.app.renderString("server", "srvHead", serverPage{V: api.View{S: &server.Server{ID: "loc-one", Name: "ONE", Node: "local"}, Status: server.StatusRunning, Can: caps}})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"/action/start", "/action/stop", "/action/restart", "/uninstall", "/update"} {
		if strings.Contains(head, bad) {
			t.Errorf("a viewer is offered %s", bad)
		}
	}
	var keys []string
	for _, tb := range allowedTabs(caps) {
		keys = append(keys, tb.Key)
	}
	if got := strings.Join(keys, ","); got != "console,perf,backups" {
		t.Errorf("viewer tabs = %s, want console,perf,backups", got)
	}
	if got := d.as(c, "GET", "/n/local/s/loc-two", nil, false).Code; got != 404 {
		t.Errorf("a server the user may not see answers %d, want 404", got)
	}
	for _, p := range []string{"/n/local/s/loc-one/action/stop", "/n/local/s/loc-one/command"} {
		if got := d.as(c, "POST", p, url.Values{"cmd": {"list"}}, false).Code; got != 403 {
			t.Errorf("POST %s: %d, want 403", p, got)
		}
	}
	if got := d.as(c, "GET", "/settings", nil, false).Code; got != 403 {
		t.Errorf("settings: %d", got)
	}
	if got := d.as(c, "GET", "/access/users", nil, false).Code; got != 403 {
		t.Errorf("access: %d", got)
	}
	// the sidebar and nav hide what is not available
	if strings.Contains(home, `href="/nodes"`) || strings.Contains(home, `href="/settings"`) || strings.Contains(home, "/servers/new") {
		t.Error("navigation shows entries the user may not open")
	}
}

func TestNodeGrantCoversEveryServerOnTheNode(t *testing.T) {
	d, st := newAccounts(t)
	u := addUser(t, st, "olga")
	giveRole(t, st, u, access.RoleOperator, access.Scope{Level: access.LevelNode, Node: "local"})
	c := d.cookieFor(t, st, "olga")
	p := principal(t, st, u)
	for _, id := range []string{"loc-one", "loc-two", "anything-later"} {
		if !p.Can(access.FilesWrite, "local", id) {
			t.Errorf("a node grant should cover %s", id)
		}
	}
	home := d.as(c, "GET", "/", nil, false).Body.String()
	if !strings.Contains(home, "loc-one") || !strings.Contains(home, "loc-two") {
		t.Error("a node grant shows both servers")
	}
	if got := d.as(c, "POST", "/n/local/servers", url.Values{"name": {"x"}}, false).Code; got != 403 {
		t.Errorf("an operator may not create servers: %d", got)
	}
}

func TestAuditRecordsActionsAndDenials(t *testing.T) {
	d, st := newAccounts(t)
	u := addUser(t, st, "mo")
	giveRole(t, st, u, access.RoleViewer, access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"})
	c := d.cookieFor(t, st, "mo")
	d.as(c, "POST", "/n/local/s/loc-one/command", url.Values{"cmd": {"list"}}, false) // denied
	owner := d.cookieFor(t, st, "admin")
	d.as(owner, "POST", "/access/groups", url.Values{"name": {"Mods"}}, true)
	es, _, _ := st.Audit(access.Filter{})
	var denied, group bool
	for _, e := range es {
		denied = denied || e.Actor == "mo" && e.Outcome == access.Denied && e.Action == "server.command"
		group = group || e.Actor == "admin" && e.Action == "group.create" && e.Target == "Mods" && e.Outcome == access.OK
	}
	if !denied || !group {
		t.Fatalf("expected a denied command and a group.create entry: %+v", es)
	}
	if r, _ := st.VerifyAudit(); !r.OK {
		t.Fatalf("chain: %+v", r)
	}
}

func TestScopedAuditViewerSeesOnlyTheirScope(t *testing.T) {
	d, st := newAccounts(t)
	_ = st.Record(access.Entry{Actor: "x", Action: "server.power", Node: "local", Server: "loc-one", Target: "start"})
	_ = st.Record(access.Entry{Actor: "x", Action: "server.power", Node: "local", Server: "loc-two", Target: "stop"})
	rid, _ := st.SaveRole(access.Role{Name: "Auditor", Perms: []access.Perm{access.AuditView, access.AuditExport}})
	u := addUser(t, st, "audi")
	_, _ = st.AddGrant(access.Grant{PrincipalID: u.ID, RoleID: rid, Scope: access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"}})
	c := d.cookieFor(t, st, "audi")
	body := d.as(c, "GET", "/access/audit", nil, false).Body.String()
	if !strings.Contains(body, "start") || strings.Contains(body, ">stop<") {
		t.Error("a scoped auditor sees entries outside their server")
	}
	csv := d.as(c, "GET", "/access/audit/export?format=csv", nil, false).Body.String()
	if strings.Contains(csv, "loc-two") || !strings.Contains(csv, "loc-one") {
		t.Errorf("export is not limited to the scope:\n%s", csv)
	}
	if got := d.as(c, "POST", "/access/audit/verify", nil, false).Code; got != 403 {
		t.Errorf("verifying the whole chain needs the global right: %d", got)
	}
}

func TestLoginWithAccounts(t *testing.T) {
	d, st := newAccounts(t)
	hash, _ := auth.HashPassword("a long enough password")
	id, _ := st.CreateUser("lena", "", hash)
	login := func(name, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"user": {name}, "password": {pw}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.com")
		req.Host = "example.com"
		rec := httptest.NewRecorder()
		d.h.ServeHTTP(rec, req)
		return rec
	}
	if rec := login("lena", "wrong password!!"); rec.Code != 200 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong password: %d cookies=%d", rec.Code, len(rec.Result().Cookies()))
	}
	if rec := login("nobody", "whatever whatever"); rec.Code != 200 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("unknown user: %d", rec.Code)
	}
	rec := login("LENA", "a long enough password") // names are case-insensitive
	if rec.Code != 303 {
		t.Fatalf("login: %d", rec.Code)
	}
	var tok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.Cookie {
			tok = c.Value
			if strings.Contains(c.Value, ".") || len(c.Value) < 40 {
				t.Errorf("the session cookie should be an opaque random token, got %q", c.Value)
			}
		}
	}
	if u, err := st.SessionUser(tok); err != nil || u.ID != id {
		t.Fatalf("session not stored: %v", err)
	}
	// a disabled account cannot sign in or use an old session
	_ = st.UpdateUser(id, "lena", "", true)
	if _, err := st.SessionUser(tok); err == nil {
		t.Fatal("a disabled user's session still works")
	}
	if rec := login("lena", "a long enough password"); len(rec.Result().Cookies()) != 0 {
		t.Fatal("a disabled user can sign in")
	}
	es, _, _ := st.Audit(access.Filter{Action: "login"})
	if len(es) < 4 {
		t.Fatalf("sign-in attempts should be audited: %d", len(es))
	}
}

func TestEnablingAccountsKeepsYouSignedInAsOwner(t *testing.T) {
	d := newDashboard(t, true)
	rec := d.do("POST", "/settings/accounts", url.Values{"current": {"wrong"}}, true)
	if d.app.store() != nil || rec.Header().Get("HX-Redirect") != "" {
		t.Fatal("a wrong password must not enable accounts")
	}
	rec = d.do("POST", "/settings/accounts", url.Values{"current": {"correct horse battery"}}, true)
	st := d.app.store()
	if st == nil || rec.Header().Get("HX-Redirect") != "/access/users" {
		t.Fatalf("accounts not enabled: %d %v", rec.Code, rec.Header())
	}
	defer st.Close()
	u, hash, err := st.UserByName("admin")
	if err != nil || !auth.CheckPassword(hash, "correct horse battery") || st.OwnerCount() != 1 || u.ID == 0 {
		t.Fatalf("the password should become the owner account: %v", err)
	}
	var tok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.Cookie {
			tok = c.Value
		}
	}
	if _, err := st.SessionUser(tok); err != nil {
		t.Fatal("the request should leave a working owner session")
	}
	if !d.cfg.Get().Accounts {
		t.Fatal("the setting is not stored")
	}
}

func TestNobodyCanRaiseThemselvesOrTouchStrongerAccounts(t *testing.T) {
	d, st := newAccounts(t)
	// a user manager who is not an owner: users.manage and nothing else
	rid, _ := st.SaveRole(access.Role{Name: "Account clerk", Perms: []access.Perm{access.UsersManage}})
	clerk := addUser(t, st, "clerk")
	_, _ = st.AddGrant(access.Grant{PrincipalID: clerk.ID, RoleID: rid})
	c := d.cookieFor(t, st, "clerk")
	owner, _, _ := st.UserByName("admin")
	roles, _ := st.ListRoles()
	var ownerRole int64
	for _, r := range roles {
		if r.Name == access.RoleOwner {
			ownerRole = r.ID
		}
	}
	for _, tc := range []struct {
		name, path string
		form       url.Values
	}{
		{"reset the owner's password", "/access/users/" + itoa(owner.ID) + "/password", url.Values{"password": {"hijacked password"}}},
		{"disable the owner", "/access/users/" + itoa(owner.ID), url.Values{"name": {"admin"}, "disabled": {"on"}}},
		{"delete the owner", "/access/users/" + itoa(owner.ID) + "/delete", url.Values{}},
		{"make themselves owner", "/access/grants", url.Values{"principal": {"u:" + itoa(clerk.ID)}, "role": {itoa(ownerRole)}, "scope": {"g"}}},
	} {
		rec := d.as(c, "POST", tc.path, tc.form, true)
		if !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
			t.Errorf("%s: not refused (%d %v)", tc.name, rec.Code, rec.Header())
		}
	}
	if _, hash, _ := st.UserByName("admin"); auth.CheckPassword(hash, "hijacked password") {
		t.Fatal("the owner's password was changed by a clerk")
	}
	if p := principal(t, st, clerk); p.Global(access.SettingsManage) {
		t.Fatal("the clerk gained rights")
	}
	// but they can manage accounts that do not exceed their own rights
	low := addUser(t, st, "lowly")
	rec := d.as(c, "POST", "/access/users/"+itoa(low.ID)+"/password", url.Values{"password": {"new password 123"}}, true)
	if strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatalf("a clerk may set the password of an account without rights: %v", rec.Header())
	}
}

func principal(t *testing.T, st *access.Store, u access.User) *access.Principal {
	t.Helper()
	p, err := st.Principal(u)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRolesCannotBeUsedToEscalate(t *testing.T) {
	d, st := newAccounts(t)
	rid, _ := st.SaveRole(access.Role{Name: "Role admin", Perms: []access.Perm{access.RolesManage, access.UsersManage}})
	u := addUser(t, st, "roly")
	_, _ = st.AddGrant(access.Grant{PrincipalID: u.ID, RoleID: rid})
	c := d.cookieFor(t, st, "roly")
	rec := d.as(c, "POST", "/access/roles", url.Values{"name": {"Sneaky"}, "perm": {"settings.manage", "nodes.manage"}}, true)
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatal("a role editor must not put rights they do not hold into a role")
	}
	if rs, _ := st.ListRoles(); len(rs) != 6 {
		t.Fatalf("a role was created: %d roles", len(rs))
	}
	rec = d.as(c, "POST", "/access/roles", url.Values{"name": {"Fine"}, "perm": {"users.manage"}}, true)
	if strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatalf("a role within their own rights should work: %v", rec.Header())
	}
	// built-in roles stay read-only
	rec = d.as(d.cookieFor(t, st, "admin"), "POST", "/access/roles/1", url.Values{"name": {"Owner"}, "perm": {"server.view"}}, true)
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatal("a built-in role was changed")
	}
}

func TestDisablingAccountsReturnsToOnePassword(t *testing.T) {
	d, st := newAccounts(t)
	rec := d.as(d.cookieFor(t, st, "admin"), "POST", "/access/disable", url.Values{"password": {"brand new password"}, "password2": {"brand new password"}}, true)
	if rec.Header().Get("HX-Redirect") != "/settings" || d.app.store() != nil || d.cfg.Get().Accounts {
		t.Fatalf("not disabled: %d %v", rec.Code, rec.Header())
	}
	if !auth.CheckPassword(d.cfg.Get().PasswordHash, "brand new password") {
		t.Fatal("the new single password is not stored")
	}
}

func TestLastOwnerCannotBeRemovedThroughTheUI(t *testing.T) {
	d, st := newAccounts(t)
	owner, _, _ := st.UserByName("admin")
	c := d.cookieFor(t, st, "admin")
	rec := d.as(c, "POST", "/access/users/"+itoa(owner.ID)+"/delete", url.Values{}, true)
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") || st.UserCount() != 1 {
		t.Fatal("the only owner was deleted")
	}
	gs, _ := st.GrantsOf(owner.ID)
	rec = d.as(c, "POST", "/access/grants/"+itoa(gs[0].ID)+"/delete", url.Values{}, true)
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") || st.OwnerCount() != 1 {
		t.Fatal("the only owner grant was removed")
	}
}

func TestCSVExportDefusesFormulas(t *testing.T) {
	for in, want := range map[string]string{"=1+1": "'=1+1", "@SUM(A1)": "'@SUM(A1)", "plain": "plain", "": ""} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNodeLoadIsASeparateRight(t *testing.T) {
	d, st := newAccounts(t)
	u := addUser(t, st, "vic")
	giveRole(t, st, u, access.RoleViewer, access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"})
	c := d.cookieFor(t, st, "vic")
	home := d.as(c, "GET", "/", nil, false).Body.String()
	if !strings.Contains(home, "loc-one") {
		t.Fatal("the server should be listed")
	}
	if strings.Contains(home, "meter") || strings.Contains(home, "no node yet") || strings.Contains(home, "Agent") {
		t.Error("a user without nodes.view must not see node load tiles or a missing-node notice")
	}
	if got := d.as(c, "GET", "/nodes", nil, false).Code; got != 403 {
		t.Errorf("nodes page: %d", got)
	}
	// with nodes.view on that node the tiles appear
	rid, _ := st.SaveRole(access.Role{Name: "Node watcher", Perms: []access.Perm{access.NodesView}})
	_, _ = st.AddGrant(access.Grant{PrincipalID: u.ID, RoleID: rid, Scope: access.Scope{Level: access.LevelNode, Node: "local"}})
	c = d.cookieFor(t, st, "vic")
	if body := d.as(c, "GET", "/", nil, false).Body.String(); !strings.Contains(body, "meter") {
		t.Error("with nodes.view the node load should be shown")
	}
	if got := d.as(c, "GET", "/nodes", nil, false).Code; got != 200 {
		t.Errorf("nodes page with nodes.view: %d", got)
	}
}
