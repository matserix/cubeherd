package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/auth"
	"cubeherd/internal/config"
)

func (a *App) accessRoutes(rt *router) {
	users, roles, sso := global(access.UsersManage), global(access.RolesManage), global(access.SSOManage)

	rt.handle("GET /account", open(), a.p((*App).accountPage))
	rt.handle("POST /account/sessions/end", open(), a.p((*App).accountEndOthers))
	rt.handle("POST /account/sessions/{hash}/end", open(), a.p((*App).accountEndSession))

	rt.handle("GET /access/users", users, a.p((*App).usersPage))
	rt.handle("GET /access/users/new", users, a.p((*App).userNew))
	rt.handle("POST /access/users", users.log("user.create", form("name")), a.p((*App).userCreate))
	rt.handle("GET /access/users/{id}", users, a.p((*App).userPage))
	rt.handle("POST /access/users/{id}", users.log("user.update", pathValue("id")), a.p((*App).userSave))
	rt.handle("POST /access/users/{id}/password", users.log("user.password-reset", pathValue("id")), a.p((*App).userPassword))
	rt.handle("POST /access/users/{id}/sessions/end", users.log("user.sessions-ended", pathValue("id")), a.p((*App).userEndSessions))
	rt.handle("POST /access/users/{id}/groups", users.log("user.groups", pathValue("id")), a.p((*App).userGroups))
	rt.handle("POST /access/users/{id}/delete", users.log("user.delete", pathValue("id")), a.p((*App).userDelete))

	rt.handle("GET /access/groups", users, a.p((*App).groupsPage))
	rt.handle("POST /access/groups", users.log("group.create", form("name")), a.p((*App).groupCreate))
	rt.handle("POST /access/groups/{id}", users.log("group.update", pathValue("id")), a.p((*App).groupSave))
	rt.handle("POST /access/groups/{id}/delete", users.log("group.delete", pathValue("id")), a.p((*App).groupDelete))

	rt.handle("GET /access/roles", roles, a.p((*App).rolesPage))
	rt.handle("GET /access/roles/new", roles, a.p((*App).roleNew))
	rt.handle("GET /access/roles/{id}", roles, a.p((*App).roleEdit))
	rt.handle("POST /access/roles", roles.log("role.create", form("name")), a.p((*App).roleSave))
	rt.handle("POST /access/roles/{id}", roles.log("role.update", pathValue("id")), a.p((*App).roleSave))
	rt.handle("POST /access/roles/{id}/delete", roles.log("role.delete", pathValue("id")), a.p((*App).roleDelete))

	rt.handle("GET /access/grants", users, a.p((*App).grantsPage))
	rt.handle("GET /access/grants/new", users, a.p((*App).grantNew))
	rt.handle("POST /access/grants", users, a.p((*App).grantAdd))
	rt.handle("POST /access/grants/{id}/delete", users.log("grant.remove", pathValue("id")), a.p((*App).grantDelete))

	rt.handle("GET /access/audit", anywhere(access.AuditView), a.p((*App).auditPage))
	rt.handle("GET /access/audit/export", anywhere(access.AuditExport), a.p((*App).auditExport))
	rt.handle("GET /access/audit/{id}", anywhere(access.AuditView), a.p((*App).auditEntry))
	rt.handle("POST /access/audit/verify", global(access.AuditView), a.p((*App).auditVerify))
	rt.handle("POST /access/audit/retention", users.log("audit.retention", form("days")), a.p((*App).auditRetention))

	rt.handle("GET /access/sso", sso, a.p((*App).ssoPage))
	rt.handle("POST /access/sso", sso.log("sso.change"), a.p((*App).ssoSave))
	rt.handle("GET /access/mail", global(access.SettingsManage), a.p((*App).mailPage))
	rt.handle("POST /access/mail", global(access.SettingsManage).log("mail.config"), a.p((*App).mailSave))
	rt.handle("POST /access/mail/test", global(access.SettingsManage).log("mail.test", form("to")), a.p((*App).mailTest))
	rt.handle("POST /access/disable", users.log("accounts.disabled"), a.p((*App).disableAccounts))
}

// ---------------- Shared pieces ----------------

// accTab is one tab of the Access area.
type accTab struct{ Key, Label, URL string }

func (a *App) accTabs(p *access.Principal) []accTab {
	var out []accTab
	if p.Global(access.UsersManage) {
		out = append(out, accTab{"users", "Benutzer", "/access/users"}, accTab{"groups", "Gruppen", "/access/groups"})
	}
	if p.Global(access.RolesManage) {
		out = append(out, accTab{"roles", "Rollen", "/access/roles"})
	}
	if p.Global(access.UsersManage) {
		out = append(out, accTab{"grants", "Zuweisungen", "/access/grants"})
	}
	if p.Global(access.SSOManage) {
		out = append(out, accTab{"sso", "Anmeldung", "/access/sso"})
	}
	if p.Global(access.SettingsManage) {
		out = append(out, accTab{"mail", "E-Mail", "/access/mail"})
	}
	if p.Anywhere(access.AuditView) {
		out = append(out, accTab{"audit", "Audit-Log", "/access/audit"})
	}
	return out
}

type accessData struct {
	Tab  string
	Tabs []accTab
	Err  string

	Users  []userRow
	User   *userDetail
	Groups []groupRow
	Roles  []roleRow
	Role   *roleEdit
	Grants []grantRow
	Opts   *grantOpts
	Audit  *auditView
	SSO    *ssoData
	Mail   *mailData
	DBSize int64
}

func (a *App) accessPage(w http.ResponseWriter, r *http.Request, tab string, d accessData) {
	d.Tab, d.Tabs = tab, a.accTabs(principalOf(r))
	d.DBSize = a.store().Size()
	a.render(w, r, "access", Page{Title: a.t("Zugriff"), Nav: "access", Data: d})
}

// scopeLabel names a scope for people: the node and server names, not their ids.
func (a *App) scopeLabel(sc access.Scope) string {
	switch sc.Level {
	case access.LevelNode:
		return a.t("Node %s", a.nodeName(sc.Node))
	case access.LevelServer:
		return a.t("Server %s", a.serverName(sc.Node, sc.Server)+" · "+a.nodeName(sc.Node))
	}
	return a.t("Alles")
}

func (a *App) nodeName(id string) string {
	if info, ok := a.Reg.Get(id); ok {
		return info.Name
	}
	return id
}

func (a *App) serverName(node, id string) string {
	if info, ok := a.Reg.Get(node); ok {
		if s, err := info.Node.Server(id); err == nil {
			return s.Name
		}
	}
	return id
}

type grantRow struct {
	access.Grant
	Scope     access.Scope
	Label     string // where it applies
	Who       string
	Sensitive bool
	Expired   bool
	Perms     int
}

func (a *App) grantRows(gs []access.Grant, roles map[int64]access.Role) []grantRow {
	out := make([]grantRow, 0, len(gs))
	for _, g := range gs {
		row := grantRow{Grant: g, Scope: g.Scope, Label: a.scopeLabel(g.Scope), Who: g.PrincipalName, Expired: g.Expired()}
		r := roles[g.RoleID]
		for _, pm := range r.Perms {
			if i, ok := access.Lookup(pm); ok && g.Scope.Level <= i.Lowest {
				row.Perms++
				row.Sensitive = row.Sensitive || i.Sensitive
			}
		}
		out = append(out, row)
	}
	return out
}

func roleMap(st *access.Store) map[int64]access.Role {
	rs, _ := st.ListRoles()
	m := make(map[int64]access.Role, len(rs))
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

func id64(r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return n, err == nil
}

// covers reports whether the actor holds everything the grants give, which is what it takes to change someone who has them.
func covers(p *access.Principal, st *access.Store, gs []access.Grant) bool {
	roles := roleMap(st)
	for _, g := range gs {
		if g.Expired() {
			continue
		}
		if !p.CanGrant(roles[g.RoleID], g.Scope) {
			return false
		}
	}
	return true
}

func (a *App) coversUser(w http.ResponseWriter, r *http.Request, target int64) bool {
	st, p := a.store(), principalOf(r)
	gs, err := st.GrantsOf(target)
	if err == nil && covers(p, st, gs) {
		return true
	}
	a.fail(w, "Dieses Konto hat mehr Rechte als du und lässt sich nicht ändern.")
	return false
}

func (a *App) storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrLastOwner):
		a.fail(w, "Das wäre der letzte Owner. Es muss mindestens ein aktiver Owner bleiben.")
	case errors.Is(err, access.ErrExists):
		a.fail(w, "Diesen Namen gibt es schon.")
	case errors.Is(err, access.ErrBuiltin):
		a.fail(w, "Eingebaute Rollen lassen sich nicht ändern. Dupliziere sie stattdessen.")
	case errors.Is(err, access.ErrInUse):
		a.fail(w, "Die Rolle ist noch zugewiesen. Entferne zuerst die Zuweisungen.")
	case errors.Is(err, access.ErrNotFound):
		a.fail(w, "Nicht gefunden.")
	default:
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
	}
}

// ---------------- Users ----------------

type userRow struct {
	access.User
	Roles    []string
	Groups   []string
	Owner    bool
	Self     bool
	Sessions int
}

func (a *App) usersPage(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	us, err := st.ListUsers()
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	self := principalOf(r).User.ID
	var rows []userRow
	for _, u := range us {
		row := userRow{User: u, Self: u.ID == self}
		gs, _ := st.GrantsOf(u.ID)
		for _, g := range gs {
			if !g.Expired() && !slices.Contains(row.Roles, g.RoleName) {
				row.Roles = append(row.Roles, g.RoleName)
			}
			if g.RoleName == access.RoleOwner && g.Scope.Level == access.LevelGlobal && !g.Expired() {
				row.Owner = true
			}
		}
		grps, _ := st.UserGroups(u.ID)
		for _, g := range grps {
			row.Groups = append(row.Groups, g.Name)
		}
		ss, _ := st.Sessions(u.ID, "")
		row.Sessions = len(ss)
		rows = append(rows, row)
	}
	a.accessPage(w, r, "users", accessData{Users: rows})
}

func (a *App) userNew(w http.ResponseWriter, r *http.Request) {
	a.partial(w, "access", "userNew", nil)
}

func (a *App) userCreate(w http.ResponseWriter, r *http.Request) {
	name, email, pw := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("email")), r.FormValue("password")
	if name == "" {
		a.fail(w, "Bitte gib einen Namen an.")
		return
	}
	hash := ""
	if pw != "" {
		if len(pw) < 10 {
			a.fail(w, "Das Passwort braucht mindestens 10 Zeichen.")
			return
		}
		var err error
		if hash, err = auth.HashPassword(pw); err != nil {
			a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
			return
		}
	}
	id, err := a.store().CreateUser(name, email, hash)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/users/"+strconv.FormatInt(id, 10))
}

type groupSel struct {
	access.Group
	Member bool
	Mapped bool
}

type userDetail struct {
	access.User
	Self       bool
	Owner      bool
	Groups     []groupSel
	Grants     []grantRow // given to the user directly
	ViaGroups  []grantRow // given through a group
	Effective  []effectiveRow
	Sessions   []access.Session
	Identities []access.Identity
	Opts       *grantOpts
}

type effectiveRow struct {
	Label string
	Perms []access.Perm
}

func (a *App) userPage(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, err := st.UserByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := &userDetail{User: u, Self: u.ID == principalOf(r).User.ID}
	roles := roleMap(st)
	all, _ := st.ListGroups()
	for _, g := range all {
		sel := groupSel{Group: g}
		for _, m := range g.Members {
			sel.Member = sel.Member || m.ID == u.ID
		}
		d.Groups = append(d.Groups, sel)
	}
	gs, _ := st.GrantsOf(u.ID)
	var direct, via []access.Grant
	for _, g := range gs {
		if g.Group {
			via = append(via, g)
		} else {
			direct = append(direct, g)
		}
	}
	d.Grants, d.ViaGroups = a.grantRows(direct, roles), a.grantRows(via, roles)
	if p, err := st.Principal(u); err == nil {
		for _, s := range p.Summary() {
			if len(s.Perms) > 0 {
				d.Effective = append(d.Effective, effectiveRow{Label: a.scopeLabel(s.Scope), Perms: s.Perms})
			}
		}
		d.Owner = p.Global(access.UsersManage)
	}
	d.Sessions, _ = st.Sessions(u.ID, tokenOf(r))
	d.Identities, _ = st.Identities(u.ID)
	d.Opts = a.grantOptions(r, u.ID)
	a.accessPage(w, r, "users", accessData{User: d})
}

func (a *App) userSave(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, err := st.UserByID(id)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	if !a.coversUser(w, r, id) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.fail(w, "Bitte gib einen Namen an.")
		return
	}
	disabled := checked(r, "disabled")
	if u.ID == principalOf(r).User.ID && disabled {
		a.fail(w, "Du kannst dein eigenes Konto nicht deaktivieren.")
		return
	}
	if err := st.UpdateUser(id, name, r.FormValue("email"), disabled); err != nil {
		a.storeErr(w, err)
		return
	}
	_ = st.SetEmailVerified(id, checked(r, "emailVerified"))
	redirect(w, r, "/access/users/"+strconv.FormatInt(id, 10))
}

func (a *App) userPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.coversUser(w, r, id) {
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 {
		a.fail(w, "Das Passwort braucht mindestens 10 Zeichen.")
		return
	}
	hash, err := auth.HashPassword(pw)
	if err == nil {
		keep := ""
		if id == principalOf(r).User.ID {
			keep = tokenOf(r)
		}
		err = a.store().SetPassword(id, hash, keep)
	}
	if err != nil {
		a.storeErr(w, err)
		return
	}
	a.toast(w, "ok", "Passwort gesetzt. Alle Sitzungen des Benutzers sind beendet.")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) userEndSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.coversUser(w, r, id) {
		return
	}
	keep := ""
	if id == principalOf(r).User.ID {
		keep = tokenOf(r)
	}
	if _, err := a.store().EndUserSessions(id, keep); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/users/"+strconv.FormatInt(id, 10))
}

func (a *App) userGroups(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.coversUser(w, r, id) {
		return
	}
	_ = r.ParseForm()
	want := map[int64]bool{}
	for _, v := range r.Form["group"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			want[n] = true
		}
	}
	all, _ := st.ListGroups()
	for _, g := range all {
		member := slices.ContainsFunc(g.Members, func(u access.User) bool { return u.ID == id })
		if want[g.ID] == member {
			continue
		}
		// joining or leaving a group changes the user's rights by what the group's grants give
		gg, _ := st.ListGrants()
		var mine []access.Grant
		for _, x := range gg {
			if x.Group && x.PrincipalID == g.ID {
				mine = append(mine, x)
			}
		}
		if !covers(p, st, mine) {
			a.fail(w, "Die Gruppe %s hat mehr Rechte als du.", g.Name)
			return
		}
		var err error
		if want[g.ID] {
			err = st.AddMember(g.ID, id)
		} else {
			err = st.RemoveMember(g.ID, id)
		}
		if err != nil {
			a.storeErr(w, err)
			return
		}
	}
	redirect(w, r, "/access/users/"+strconv.FormatInt(id, 10))
}

func (a *App) userDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if id == principalOf(r).User.ID {
		a.fail(w, "Du kannst dein eigenes Konto nicht löschen.")
		return
	}
	if !a.coversUser(w, r, id) {
		return
	}
	if err := a.store().DeleteUser(id); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/users")
}

// ---------------- Groups ----------------

type groupRow struct {
	access.Group
	Grants []grantRow
	Pick   []memberPick // everyone who could be a member, members first
}

type memberPick struct {
	access.User
	Member bool
}

func (a *App) groupsPage(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	gs, _ := st.ListGroups()
	all, _ := st.ListGrants()
	roles := roleMap(st)
	users, _ := st.ListUsers()
	var rows []groupRow
	for _, g := range gs {
		row := groupRow{Group: g}
		in := map[int64]bool{}
		for _, m := range g.Members {
			in[m.ID] = true
			row.Pick = append(row.Pick, memberPick{m, true})
		}
		for _, u := range users {
			if !in[u.ID] {
				row.Pick = append(row.Pick, memberPick{u, false})
			}
		}
		var mine []access.Grant
		for _, x := range all {
			if x.Group && x.PrincipalID == g.ID {
				mine = append(mine, x)
			}
		}
		row.Grants = a.grantRows(mine, roles)
		rows = append(rows, row)
	}
	a.accessPage(w, r, "groups", accessData{Groups: rows})
}

func (a *App) groupCreate(w http.ResponseWriter, r *http.Request) {
	if _, err := a.store().CreateGroup(r.FormValue("name"), r.FormValue("oidc")); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/groups")
}

// groupSave renames a group and sets its members from the form.
func (a *App) groupSave(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	all, _ := st.ListGrants()
	var mine []access.Grant
	for _, x := range all {
		if x.Group && x.PrincipalID == id {
			mine = append(mine, x)
		}
	}
	if !covers(p, st, mine) {
		a.fail(w, "Die Gruppe hat mehr Rechte als du.")
		return
	}
	if strings.TrimSpace(r.FormValue("name")) == "" {
		a.fail(w, "Bitte gib einen Namen an.")
		return
	}
	if err := st.UpdateGroup(id, r.FormValue("name"), r.FormValue("oidc")); err != nil {
		a.storeErr(w, err)
		return
	}
	_ = r.ParseForm()
	want := map[int64]bool{}
	for _, v := range r.Form["member"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			want[n] = true
		}
	}
	gs, _ := st.ListGroups()
	for _, g := range gs {
		if g.ID != id {
			continue
		}
		have := map[int64]bool{}
		for _, m := range g.Members {
			have[m.ID] = true
			if !want[m.ID] {
				if err := st.RemoveMember(id, m.ID); err != nil {
					a.storeErr(w, err)
					return
				}
			}
		}
		for u := range want {
			if !have[u] {
				if err := st.AddMember(id, u); err != nil {
					a.storeErr(w, err)
					return
				}
			}
		}
	}
	redirect(w, r, "/access/groups")
}

func (a *App) groupDelete(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	all, _ := st.ListGrants()
	var mine []access.Grant
	for _, x := range all {
		if x.Group && x.PrincipalID == id {
			mine = append(mine, x)
		}
	}
	if !covers(p, st, mine) {
		a.fail(w, "Die Gruppe hat mehr Rechte als du.")
		return
	}
	if err := st.DeleteGroup(id); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/groups")
}

// ---------------- Grants ----------------

type grantOpts struct {
	Users    []access.User
	Groups   []access.Group
	Roles    []access.Role
	Nodes    []scopeNode
	Fixed    string // preselected principal, "u:12" or "g:3"
	Return   string // where to go after saving
	Scope    string
	RoleName string
}

type scopeNode struct {
	ID      string
	Name    string
	Servers []scopeServer
}

type scopeServer struct{ ID, Name string }

func (a *App) grantOptions(r *http.Request, userID int64) *grantOpts {
	st := a.store()
	o := &grantOpts{Return: "/access/grants"}
	o.Users, _ = st.ListUsers()
	o.Groups, _ = st.ListGroups()
	o.Roles, _ = st.ListRoles()
	if userID != 0 {
		o.Fixed = "u:" + strconv.FormatInt(userID, 10)
		o.Return = "/access/users/" + strconv.FormatInt(userID, 10)
	}
	for _, n := range a.Reg.Refresh(r.Context(), false, rackMaxAge) {
		sn := scopeNode{ID: n.ID, Name: n.Name}
		for _, v := range n.Views {
			sn.Servers = append(sn.Servers, scopeServer{v.S.ID, v.S.Name})
		}
		o.Nodes = append(o.Nodes, sn)
	}
	return o
}

func (a *App) grantsPage(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	gs, _ := st.ListGrants()
	a.accessPage(w, r, "grants", accessData{Grants: a.grantRows(gs, roleMap(st)), Opts: a.grantOptions(r, 0)})
}

func (a *App) grantNew(w http.ResponseWriter, r *http.Request) {
	uid, _ := strconv.ParseInt(r.FormValue("user"), 10, 64)
	o := a.grantOptions(r, uid)
	if uid == 0 {
		o.Fixed = r.FormValue("principal")
	}
	a.partial(w, "access", "grantNew", o)
}

// parseScope reads the scope select: "g", "n:<node>" or "s:<node>:<server>".
func parseScope(v string) (access.Scope, bool) {
	parts := strings.SplitN(v, ":", 3)
	switch {
	case v == "g":
		return access.Scope{}, true
	case len(parts) == 2 && parts[0] == "n" && parts[1] != "":
		return access.Scope{Level: access.LevelNode, Node: parts[1]}, true
	case len(parts) == 3 && parts[0] == "s" && parts[1] != "" && parts[2] != "":
		return access.Scope{Level: access.LevelServer, Node: parts[1], Server: parts[2]}, true
	}
	return access.Scope{}, false
}

func (a *App) grantAdd(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	who := r.FormValue("principal")
	kind, idStr, _ := strings.Cut(who, ":")
	pid, err := strconv.ParseInt(idStr, 10, 64)
	rid, err2 := strconv.ParseInt(r.FormValue("role"), 10, 64)
	sc, ok := parseScope(r.FormValue("scope"))
	if err != nil || err2 != nil || !ok || (kind != "u" && kind != "g") {
		a.fail(w, "Bitte wähle Benutzer, Rolle und Bereich.")
		return
	}
	role, err := st.Role(rid)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	if !p.CanGrant(role, sc) {
		a.fail(w, "Du kannst keine Rolle vergeben, deren Rechte du dort nicht selbst hast.")
		return
	}
	g := access.Grant{Group: kind == "g", PrincipalID: pid, RoleID: rid, Scope: sc, CreatedBy: p.User.Name}
	if d := strings.TrimSpace(r.FormValue("expires")); d != "" {
		t, err := time.ParseInLocation("2006-01-02", d, displayLoc())
		if err != nil {
			a.fail(w, "Ungültiges Datum.")
			return
		}
		g.Expires = t.Add(24*time.Hour - time.Second)
	}
	id, err := st.AddGrant(g)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	a.record(r, "grant.add", role.Name+" → "+kind+":"+idStr+" · "+a.scopeLabel(sc), access.OK, "grant", id)
	redirect(w, r, firstNonEmpty(r.FormValue("return"), "/access/grants"))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" && strings.HasPrefix(s, "/access/") {
			return s
		}
	}
	return "/access/grants"
}

func (a *App) grantDelete(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	g, err := st.Grant(id)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	if !covers(p, st, []access.Grant{g}) {
		a.fail(w, "Du kannst keine Zuweisung entfernen, die mehr Rechte gibt als du hast.")
		return
	}
	if err := st.DeleteGrant(id); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, firstNonEmpty(r.FormValue("return"), "/access/grants"))
}

// ---------------- Account (own) ----------------

type accountData struct {
	User       access.User
	Sessions   []access.Session
	Identities []access.Identity
	Effective  []effectiveRow
	Groups     []string
	HasPW      bool
}

func (a *App) accountPage(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	if p == nil {
		redirect(w, r, "/settings")
		return
	}
	d := accountData{User: p.User, HasPW: p.User.HasPassword}
	d.Sessions, _ = st.Sessions(p.User.ID, tokenOf(r))
	d.Identities, _ = st.Identities(p.User.ID)
	grps, _ := st.UserGroups(p.User.ID)
	for _, g := range grps {
		d.Groups = append(d.Groups, g.Name)
	}
	for _, s := range p.Summary() {
		if len(s.Perms) > 0 {
			d.Effective = append(d.Effective, effectiveRow{Label: a.scopeLabel(s.Scope), Perms: s.Perms})
		}
	}
	a.render(w, r, "account", Page{Title: a.t("Mein Konto"), Nav: "account", Data: d})
}

func (a *App) accountEndOthers(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if _, err := a.store().EndUserSessions(p.User.ID, tokenOf(r)); err != nil {
		a.storeErr(w, err)
		return
	}
	a.record(r, "user.sessions-ended", p.User.Name, access.OK)
	redirect(w, r, "/account")
}

func (a *App) accountEndSession(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if err := a.store().EndSessionHash(p.User.ID, r.PathValue("hash")); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/account")
}

// accountPassword changes the signed-in user's own password and ends their other sessions.
func (a *App) accountPassword(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	_, hash, err := st.UserByName(p.User.Name)
	if err != nil || (hash != "" && !auth.CheckPassword(hash, r.FormValue("current"))) {
		a.lim.Fail(r)
		a.fail(w, "Das aktuelle Passwort ist falsch.")
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 || pw != r.FormValue("password2") {
		a.fail(w, "Neues Passwort: mindestens 10 Zeichen und beide Felder gleich.")
		return
	}
	nh, err := auth.HashPassword(pw)
	if err == nil {
		err = st.SetPassword(p.User.ID, nh, tokenOf(r))
	}
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	a.record(r, "user.password", p.User.Name, access.OK)
	a.toast(w, "ok", "Passwort geändert. Andere Sitzungen sind abgemeldet.")
	w.WriteHeader(http.StatusNoContent)
}

// ---------------- Switching the mode off ----------------

// disableAccounts returns to the single password. The owner sets it now, since the old one may be long outdated.
func (a *App) disableAccounts(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !p.Global(access.SettingsManage) || !p.Global(access.RolesManage) {
		a.fail(w, "Dafür fehlt dir die Berechtigung.")
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 || pw != r.FormValue("password2") {
		a.fail(w, "Neues Passwort: mindestens 10 Zeichen und beide Felder gleich.")
		return
	}
	hash, err := auth.HashPassword(pw)
	if err == nil {
		err = a.Cfg.Update(func(s *config.Settings) { s.Accounts, s.PasswordHash = false, hash })
	}
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	// the record of this step goes to the database that is about to be set aside
	a.record(r, "accounts.disabled", p.User.Name, access.OK)
	st := a.store()
	a.accs.p.Store(nil)
	_ = st.Close()
	a.sess.Issue(w, a.secure(r))
	redirect(w, r, "/settings")
}

// RunMaintenance prunes old audit entries while the dashboard runs.
func (a *App) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		if st := a.store(); st != nil {
			days := a.Cfg.Get().AuditDays
			if days == 0 {
				days = 365
			}
			if days > 0 {
				if _, err := st.PruneAudit(time.Duration(days) * 24 * time.Hour); err != nil {
					log.Printf("audit retention: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// accessHome is the first page of the Access area the user may open.
func accessHome(p Page) string {
	switch {
	case p.P.Global(access.UsersManage):
		return "/access/users"
	case p.P.Global(access.RolesManage):
		return "/access/roles"
	case p.P.Global(access.SSOManage):
		return "/access/sso"
	}
	return "/access/audit"
}
