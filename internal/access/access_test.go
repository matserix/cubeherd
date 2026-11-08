package access

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cubeherd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func roleID(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	rs, _ := s.ListRoles()
	for _, r := range rs {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("role %s missing", name)
	return 0
}

func user(t *testing.T, s *Store, name string) User {
	t.Helper()
	id, err := s.CreateUser(name, "", "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByID(id)
	return u
}

func grant(t *testing.T, s *Store, u User, role string, sc Scope) int64 {
	t.Helper()
	id, err := s.AddGrant(Grant{PrincipalID: u.ID, RoleID: roleID(t, s, role), Scope: sc})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func principal(t *testing.T, s *Store, u User) *Principal {
	t.Helper()
	p, err := s.Principal(u)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScopesInheritAndStayNarrow(t *testing.T) {
	s := open(t)
	u := user(t, s, "anna")
	grant(t, s, u, RoleOperator, Scope{LevelNode, "berlin", ""})
	grant(t, s, u, RoleViewer, Scope{LevelServer, "hamburg", "survival"})
	p := principal(t, s, u)
	for _, c := range []struct {
		perm         Perm
		node, server string
		want         bool
	}{
		{ServerPower, "berlin", "any", true},        // node grant covers every server on it
		{ServerPower, "hamburg", "survival", false}, // viewer only
		{ServerView, "hamburg", "survival", true},
		{ServerView, "hamburg", "creative", false}, // other server on the same node
		{ServerView, "paris", "x", false},
		{ServerCreate, "berlin", "", false}, // operator cannot create
		{SettingsManage, "", "", false},
		{FilesRead, "berlin", "s", true},
	} {
		if got := p.Can(c.perm, c.node, c.server); got != c.want {
			t.Errorf("Can(%s,%q,%q) = %v, want %v", c.perm, c.node, c.server, got, c.want)
		}
	}
}

func TestGlobalOnlyPermissionsIgnoreNarrowGrants(t *testing.T) {
	s := open(t)
	u := user(t, s, "bob")
	grant(t, s, u, RoleOwner, Scope{LevelNode, "berlin", ""}) // owner on one node
	p := principal(t, s, u)
	if p.Global(UsersManage) || p.Can(NodesManage, "berlin", "") || p.Can(SettingsManage, "berlin", "") {
		t.Fatal("global-only permissions must not apply from a node grant")
	}
	if !p.Can(ServerCreate, "berlin", "") || !p.Can(ServerDelete, "berlin", "x") {
		t.Fatal("node-level permissions should apply")
	}
	if p.Can(ServerCreate, "berlin", "x") != true {
		// a server-scoped question is still covered by the node grant
		t.Fatal("node grant covers servers on it")
	}
	// server.create on a server grant has no effect (needs a node)
	v := user(t, s, "vic")
	grant(t, s, v, RoleOwner, Scope{LevelServer, "berlin", "x"})
	if principal(t, s, v).Can(ServerCreate, "berlin", "x") {
		t.Fatal("server.create must not be effective on a server grant")
	}
}

func TestNoGrantNoAccess(t *testing.T) {
	s := open(t)
	p := principal(t, s, user(t, s, "nobody"))
	for _, i := range Catalog {
		if p.Can(i.ID, "n", "s") || p.Anywhere(i.ID) {
			t.Fatalf("%s allowed without a grant", i.ID)
		}
	}
	var none *Principal
	if !none.Can(ServerDelete, "a", "b") {
		t.Fatal("single-user mode allows everything")
	}
}

func TestGroupGrantsAndExpiry(t *testing.T) {
	s := open(t)
	u := user(t, s, "carl")
	gid, _ := s.CreateGroup("mods", "", "")
	_ = s.AddMember(gid, u.ID)
	if _, err := s.AddGrant(Grant{Group: true, PrincipalID: gid, RoleID: roleID(t, s, RoleModerator), Scope: Scope{}}); err != nil {
		t.Fatal(err)
	}
	if !principal(t, s, u).Can(ServerPower, "n", "s") {
		t.Fatal("group grant should apply")
	}
	_ = s.RemoveMember(gid, u.ID)
	if principal(t, s, u).Can(ServerPower, "n", "s") {
		t.Fatal("leaving the group should remove the access")
	}
	_ = s.AddMember(gid, u.ID)
	_, _ = s.AddGrant(Grant{PrincipalID: u.ID, RoleID: roleID(t, s, RoleOperator), Expires: time.Now().Add(-time.Hour)})
	if principal(t, s, u).Can(FilesWrite, "n", "s") {
		t.Fatal("an expired grant must not apply")
	}
}

func TestLastOwnerIsProtected(t *testing.T) {
	s := open(t)
	a, b := user(t, s, "a"), user(t, s, "b")
	ga := grant(t, s, a, RoleOwner, Scope{})
	if err := s.DeleteUser(a.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("delete last owner: %v", err)
	}
	if err := s.UpdateUser(a.ID, "a", "", true); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disable last owner: %v", err)
	}
	if err := s.DeleteGrant(ga); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("revoke last owner grant: %v", err)
	}
	grant(t, s, b, RoleOwner, Scope{})
	if err := s.DeleteUser(a.ID); err != nil {
		t.Fatalf("with a second owner deleting must work: %v", err)
	}
	if s.OwnerCount() != 1 {
		t.Fatal("one owner should remain")
	}
}

func TestCustomRoles(t *testing.T) {
	s := open(t)
	id, err := s.SaveRole(Role{Name: "Backup keeper", Perms: []Perm{ServerView, BackupsView, BackupsCreate}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRole(Role{Name: "backup keeper"}); !errors.Is(err, ErrExists) {
		t.Fatalf("names are unique ignoring case: %v", err)
	}
	if _, err := s.SaveRole(Role{ID: roleID(t, s, RoleViewer), Name: "x"}); !errors.Is(err, ErrBuiltin) {
		t.Fatalf("built-in roles are read-only: %v", err)
	}
	if _, err := s.SaveRole(Role{Name: "bad", Perms: []Perm{"made.up"}}); err == nil {
		t.Fatal("unknown permissions are refused")
	}
	u := user(t, s, "dora")
	_, _ = s.AddGrant(Grant{PrincipalID: u.ID, RoleID: id, Scope: Scope{LevelServer, "n", "s"}})
	if err := s.DeleteRole(id); !errors.Is(err, ErrInUse) {
		t.Fatalf("a role in use cannot be deleted: %v", err)
	}
	p := principal(t, s, u)
	if !p.Can(BackupsCreate, "n", "s") || p.Can(BackupsRestore, "n", "s") {
		t.Fatal("custom role permissions not applied exactly")
	}
}

func TestEscalationGuards(t *testing.T) {
	s := open(t)
	u := user(t, s, "ed")
	grant(t, s, u, RoleOperator, Scope{LevelNode, "berlin", ""})
	p := principal(t, s, u)
	owner, _ := s.Role(roleID(t, s, RoleOwner))
	viewer, _ := s.Role(roleID(t, s, RoleViewer))
	if p.CanGrant(owner, Scope{LevelNode, "berlin", ""}) {
		t.Fatal("an operator must not hand out Owner")
	}
	if !p.CanGrant(viewer, Scope{LevelServer, "berlin", "s"}) {
		t.Fatal("handing out a role you hold on that scope is fine")
	}
	if p.CanGrant(viewer, Scope{LevelNode, "hamburg", ""}) {
		t.Fatal("not on a node where you hold nothing")
	}
	if p.CanDefine([]Perm{ServerView}) {
		t.Fatal("a node grant is not enough to define roles (needs global permissions)")
	}
}

func TestSessions(t *testing.T) {
	s := open(t)
	u := user(t, s, "fay")
	tok, err := s.NewSession(u.ID, "1.2.3.4", "ua", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionUser(tok); err != nil || got.ID != u.ID {
		t.Fatalf("session lookup: %v %v", got, err)
	}
	other, _ := s.NewSession(u.ID, "", "", time.Hour)
	if n, _ := s.EndUserSessions(u.ID, tok); n != 1 {
		t.Fatalf("ended %d, want 1", n)
	}
	if _, err := s.SessionUser(other); err == nil {
		t.Fatal("ended session still valid")
	}
	_ = s.UpdateUser(u.ID, u.Name, "", true)
	if _, err := s.SessionUser(tok); err == nil {
		t.Fatal("disabling must end sessions")
	}
	exp, _ := s.NewSession(u.ID, "", "", -time.Second)
	_ = s.UpdateUser(u.ID, u.Name, "", false)
	if _, err := s.SessionUser(exp); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestAuditChain(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		if err := s.Record(Entry{Actor: "x", Action: "server.power", Node: "n", Server: "s", Detail: Detail("i", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := s.VerifyAudit(); !r.OK || r.Entries != 5 {
		t.Fatalf("fresh chain: %+v", r)
	}
	_, _ = s.db.Exec("UPDATE audit SET target='tampered' WHERE id=3")
	if r, _ := s.VerifyAudit(); r.OK || r.BadID != 3 {
		t.Fatalf("edit not detected: %+v", r)
	}
	_, _ = s.db.Exec("UPDATE audit SET target='' WHERE id=3")
	_, _ = s.db.Exec("DELETE FROM audit WHERE id=3")
	if r, _ := s.VerifyAudit(); r.OK || r.BadID != 4 {
		t.Fatalf("removal not detected: %+v", r)
	}
}

func TestAuditFilterAndScope(t *testing.T) {
	s := open(t)
	_ = s.Record(Entry{Actor: "anna", Action: "server.power", Node: "a", Server: "x"})
	_ = s.Record(Entry{Actor: "bob", Action: "files.save", Node: "a", Server: "y", Target: "server.properties"})
	_ = s.Record(Entry{Actor: "bob", Action: "login.ok"})
	es, total, _ := s.Audit(Filter{Only: []Scope{{LevelServer, "a", "x"}}})
	if total != 1 || es[0].Actor != "anna" {
		t.Fatalf("server scope: %d %v", total, es)
	}
	if _, total, _ := s.Audit(Filter{Only: []Scope{{LevelNode, "a", ""}}}); total != 2 {
		t.Fatalf("node scope: %d", total)
	}
	if _, total, _ := s.Audit(Filter{Actor: "bo", Action: "files."}); total != 1 {
		t.Fatalf("filter: %d", total)
	}
	if _, total, _ := s.Audit(Filter{Text: "100%"}); total != 0 {
		t.Fatal("wildcards must be escaped")
	}
}

func TestBuiltinsResyncAndSnapshot(t *testing.T) {
	s := open(t)
	_, _ = s.db.Exec("DELETE FROM role_perms")
	if err := s.syncBuiltins(); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Role(roleID(t, s, RoleViewer))
	if !r.Has(ServerView) || r.Has(ServerPower) {
		t.Fatalf("viewer perms: %v", r.Perms)
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := s.Snapshot(dst); err != nil {
		t.Fatal(err)
	}
	c, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestAuditChainSurvivesConcurrentWriters(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := s.Record(Entry{Actor: "w", Action: "server.power", Detail: Detail("i", i)}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if r, err := s.VerifyAudit(); err != nil || !r.OK || r.Entries != 200 {
		t.Fatalf("chain after concurrent writes: %+v %v", r, err)
	}
}

func TestPruneKeepsTheChainVerifiable(t *testing.T) {
	s := open(t)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 3; i++ {
		_ = s.Record(Entry{Time: old, Actor: "x", Action: "login.ok"})
	}
	for i := 0; i < 3; i++ {
		_ = s.Record(Entry{Actor: "x", Action: "login.ok"})
	}
	n, err := s.PruneAudit(24 * time.Hour)
	if err != nil || n != 3 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	if r, _ := s.VerifyAudit(); !r.OK || r.Entries != 4 { // 3 kept + the "pruned" note
		t.Fatalf("after pruning: %+v", r)
	}
	if es, _, _ := s.Audit(Filter{Action: "audit.pruned"}); len(es) != 1 {
		t.Fatal("pruning must leave a note")
	}
}

func TestSecretsRoundTripAndNeedTheKey(t *testing.T) {
	s := open(t)
	enc, err := s.seal("hunter2")
	if err != nil || enc == "hunter2" {
		t.Fatal("not encrypted")
	}
	if got, err := s.open(enc); err != nil || got != "hunter2" {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(s.path), "access.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.open(enc); err == nil {
		t.Fatal("a secret opened without its key")
	}
}

func TestUsersFromBeforeTheEmailFlagCountAsConfirmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cubeherd.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO users(name,email,created_at) VALUES('olli','olli@example.com',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _, err := s.UserByName("olli")
	if err != nil || !u.EmailVerified {
		t.Fatalf("an existing account must not be locked out by the new flag: %+v %v", u, err)
	}
	if err := s.SetEmailVerified(u.ID, false); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(u.ID); u.EmailVerified {
		t.Fatal("flag not stored")
	}
}
