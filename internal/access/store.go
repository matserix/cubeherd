package access

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
)

func now() int64 { return time.Now().Unix() }

func isUnique(err error) bool {
	var se *sqlite.Error
	// 2067 = SQLITE_CONSTRAINT_UNIQUE, 1555 = SQLITE_CONSTRAINT_PRIMARYKEY
	return errors.As(err, &se) && (se.Code() == 2067 || se.Code() == 1555)
}

// ---------------- Users ----------------

type User struct {
	ID            int64
	Name          string
	Email         string
	TOTP          bool   // two-factor sign-in is set up
	Status        string // StatusActive or StatusPending (registered, not yet approved)
	ForceReset    bool   // the password must be reset before the next sign-in
	EmailVerified bool   // the address was confirmed (admin-created accounts and accounts from before the column count as confirmed)
	HasPassword   bool
	Disabled      bool
	Created       time.Time
	LastLogin     time.Time
}

const userCols = "id, name, email, email_verified, totp_enabled, status, force_reset, password_hash != '', disabled, created_at, last_login_at"

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, last int64
	err := sc.Scan(&u.ID, &u.Name, &u.Email, &u.EmailVerified, &u.TOTP, &u.Status, &u.ForceReset, &u.HasPassword, &u.Disabled, &created, &last)
	u.Created = time.Unix(created, 0)
	if last > 0 {
		u.LastLogin = time.Unix(last, 0)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

// Account states.
const (
	StatusActive  = "active"
	StatusPending = "pending" // registered itself and waits for approval
)

// NewUser describes an account to create. Zero values give an active account with a confirmed address.
type NewUser struct {
	Name, Email, PasswordHash string
	Unverified                bool // the e-mail address still has to be confirmed
	Status                    string
}

// CreateUserWith adds an account with the given state.
func (s *Store) CreateUserWith(n NewUser) (int64, error) {
	name := strings.TrimSpace(n.Name)
	if name == "" {
		return 0, errors.New("a user name is required")
	}
	if n.Status == "" {
		n.Status = StatusActive
	}
	r, err := s.db.Exec("INSERT INTO users(name, email, password_hash, created_at, email_verified, status) VALUES(?,?,?,?,?,?)",
		name, strings.TrimSpace(n.Email), n.PasswordHash, now(), !n.Unverified, n.Status)
	if isUnique(err) {
		return 0, ErrExists
	}
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// SetStatus approves (StatusActive) a pending account.
func (s *Store) SetStatus(id int64, status string) error {
	_, err := s.db.Exec("UPDATE users SET status=? WHERE id=?", status, id)
	return err
}

// PendingUsers lists the accounts that wait for approval, oldest first.
func (s *Store) PendingUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT "+userCols+" FROM users WHERE status=? ORDER BY created_at", StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetForceReset requires (or stops requiring) a password reset before the next sign-in.
func (s *Store) SetForceReset(id int64, on bool) error {
	_, err := s.db.Exec("UPDATE users SET force_reset=? WHERE id=?", on, id)
	return err
}

// CreateUser adds an account; hash may be empty for accounts that only sign in through SSO.
func (s *Store) CreateUser(name, email, hash string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("a user name is required")
	}
	r, err := s.db.Exec("INSERT INTO users(name, email, password_hash, created_at) VALUES(?,?,?,?)", name, strings.TrimSpace(email), hash, now())
	if isUnique(err) {
		return 0, ErrExists
	}
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// SetEmailVerified marks the user's address as confirmed or not.
func (s *Store) SetEmailVerified(id int64, ok bool) error {
	_, err := s.db.Exec("UPDATE users SET email_verified=? WHERE id=?", ok, id)
	return err
}

func (s *Store) UserByID(id int64) (User, error) {
	return scanUser(s.db.QueryRow("SELECT "+userCols+" FROM users WHERE id=?", id))
}

// UserByName returns the account and its password hash (empty when it has none).
func (s *Store) UserByName(name string) (User, string, error) {
	var hash string
	if err := s.db.QueryRow("SELECT password_hash FROM users WHERE name=?", strings.TrimSpace(name)).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, "", ErrNotFound
		}
		return User{}, "", err
	}
	u, err := scanUser(s.db.QueryRow("SELECT "+userCols+" FROM users WHERE name=?", strings.TrimSpace(name)))
	return u, hash, err
}

func (s *Store) UserByEmail(email string) (User, error) {
	if email == "" {
		return User{}, ErrNotFound
	}
	return scanUser(s.db.QueryRow("SELECT "+userCols+" FROM users WHERE email=? COLLATE NOCASE AND email != '' ORDER BY id LIMIT 1", email))
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT " + userCols + " FROM users ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser changes name, e-mail and the disabled flag. Disabling ends the user's sessions.
func (s *Store) UpdateUser(id int64, name, email string, disabled bool) error {
	return s.guarded(func(tx *sql.Tx) error {
		r, err := tx.Exec("UPDATE users SET name=?, email=?, disabled=? WHERE id=?", strings.TrimSpace(name), strings.TrimSpace(email), disabled, id)
		if isUnique(err) {
			return ErrExists
		}
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if disabled {
			_, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", id)
		}
		return err
	})
}

// SetPassword stores a new hash and ends every session of the user except the one named by keep (a session token, may be "").
func (s *Store) SetPassword(id int64, hash, keep string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE users SET password_hash=?, force_reset=0 WHERE id=?", hash, id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE user_id=? AND id_hash != ?", id, hashToken(keep)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteUser(id int64) error {
	return s.guarded(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM grants WHERE principal_type='user' AND principal_id=?", id); err != nil {
			return err
		}
		r, err := tx.Exec("DELETE FROM users WHERE id=?", id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) TouchLogin(id int64) {
	_, _ = s.db.Exec("UPDATE users SET last_login_at=? WHERE id=?", now(), id)
}

// UserCount is the number of accounts.
func (s *Store) UserCount() int {
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	return n
}

// ---------------- Identities (SSO) ----------------

func (s *Store) LinkIdentity(userID int64, issuer, subject, email string) error {
	_, err := s.db.Exec("INSERT INTO identities(user_id, issuer, subject, email) VALUES(?,?,?,?)", userID, issuer, subject, email)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) UserByIdentity(issuer, subject string) (User, error) {
	return scanUser(s.db.QueryRow("SELECT "+userCols+" FROM users WHERE id=(SELECT user_id FROM identities WHERE issuer=? AND subject=?)", issuer, subject))
}

type Identity struct {
	ID      int64
	Issuer  string
	Subject string
	Email   string
}

func (s *Store) Identities(userID int64) ([]Identity, error) {
	rows, err := s.db.Query("SELECT id, issuer, subject, email FROM identities WHERE user_id=? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.ID, &i.Issuer, &i.Subject, &i.Email); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) Unlink(userID, identityID int64) error {
	_, err := s.db.Exec("DELETE FROM identities WHERE id=? AND user_id=?", identityID, userID)
	return err
}

// ---------------- Groups ----------------

type Group struct {
	ID      int64
	Name    string
	OIDC    string // value of the identity provider's group claim that maps to this group
	Members []User
}

func (s *Store) CreateGroup(name, oidc string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("a group name is required")
	}
	r, err := s.db.Exec("INSERT INTO groups(name, oidc_value) VALUES(?,?)", name, strings.TrimSpace(oidc))
	if isUnique(err) {
		return 0, ErrExists
	}
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) UpdateGroup(id int64, name, oidc string) error {
	r, err := s.db.Exec("UPDATE groups SET name=?, oidc_value=? WHERE id=?", strings.TrimSpace(name), strings.TrimSpace(oidc), id)
	if isUnique(err) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteGroup(id int64) error {
	return s.guarded(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM grants WHERE principal_type='group' AND principal_id=?", id); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM groups WHERE id=?", id)
		return err
	})
}

func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.db.Query("SELECT id, name, oidc_value FROM groups ORDER BY name")
	if err != nil {
		return nil, err
	}
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.OIDC); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	for i := range out {
		mr, err := s.db.Query("SELECT "+userCols+" FROM users WHERE id IN (SELECT user_id FROM group_members WHERE group_id=?) ORDER BY name", out[i].ID)
		if err != nil {
			return nil, err
		}
		for mr.Next() {
			u, err := scanUser(mr)
			if err != nil {
				mr.Close()
				return nil, err
			}
			out[i].Members = append(out[i].Members, u)
		}
		mr.Close()
	}
	return out, nil
}

func (s *Store) AddMember(groupID, userID int64) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO group_members(group_id, user_id) VALUES(?,?)", groupID, userID)
	return err
}

func (s *Store) RemoveMember(groupID, userID int64) error {
	return s.guarded(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM group_members WHERE group_id=? AND user_id=?", groupID, userID)
		return err
	})
}

// UserGroups returns the groups a user belongs to.
func (s *Store) UserGroups(userID int64) ([]Group, error) {
	rows, err := s.db.Query("SELECT id, name, oidc_value FROM groups WHERE id IN (SELECT group_id FROM group_members WHERE user_id=?) ORDER BY name", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.OIDC); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetMappedGroups makes the groups that an identity provider names the user's mapped memberships.
func (s *Store) SetMappedGroups(userID int64, claimValues []string) error {
	return s.guarded(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM group_members WHERE user_id=? AND mapped=1", userID); err != nil {
			return err
		}
		for _, v := range claimValues {
			if v == "" {
				continue
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO group_members(group_id, user_id, mapped) SELECT id, ?, 1 FROM groups WHERE oidc_value=?", userID, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------- Roles ----------------

type Role struct {
	ID          int64
	Name        string
	Description string
	Builtin     bool
	Perms       []Perm
	Grants      int // how many grants use the role
}

func (r Role) Has(p Perm) bool { return slices.Contains(r.Perms, p) }

func (s *Store) ListRoles() ([]Role, error) {
	rows, err := s.db.Query(`SELECT id, name, description, builtin, (SELECT COUNT(*) FROM grants WHERE role_id=roles.id) FROM roles ORDER BY builtin DESC, id`)
	if err != nil {
		return nil, err
	}
	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.Builtin, &r.Grants); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	perms, err := s.rolePerms()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Perms = perms[out[i].ID]
	}
	return out, nil
}

func (s *Store) rolePerms() (map[int64][]Perm, error) {
	rows, err := s.db.Query("SELECT role_id, perm FROM role_perms")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64][]Perm{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		if Valid(Perm(p)) {
			m[id] = append(m[id], Perm(p))
		}
	}
	return m, rows.Err()
}

func (s *Store) Role(id int64) (Role, error) {
	roles, err := s.ListRoles()
	if err != nil {
		return Role{}, err
	}
	for _, r := range roles {
		if r.ID == id {
			return r, nil
		}
	}
	return Role{}, ErrNotFound
}

// SaveRole creates a custom role (ID 0) or updates one. Built-in roles are read-only.
func (s *Store) SaveRole(r Role) (int64, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return 0, errors.New("a role name is required")
	}
	for _, p := range r.Perms {
		if !Valid(p) {
			return 0, errors.New("unknown permission " + string(p))
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if r.ID == 0 {
		res, err := tx.Exec("INSERT INTO roles(name, builtin, description) VALUES(?,0,?)", r.Name, strings.TrimSpace(r.Description))
		if isUnique(err) {
			return 0, ErrExists
		}
		if err != nil {
			return 0, err
		}
		r.ID, _ = res.LastInsertId()
	} else {
		var builtin bool
		if err := tx.QueryRow("SELECT builtin FROM roles WHERE id=?", r.ID).Scan(&builtin); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, ErrNotFound
			}
			return 0, err
		}
		if builtin {
			return 0, ErrBuiltin
		}
		if _, err := tx.Exec("UPDATE roles SET name=?, description=? WHERE id=?", r.Name, strings.TrimSpace(r.Description), r.ID); err != nil {
			if isUnique(err) {
				return 0, ErrExists
			}
			return 0, err
		}
		if _, err := tx.Exec("DELETE FROM role_perms WHERE role_id=?", r.ID); err != nil {
			return 0, err
		}
	}
	for _, p := range slices.Compact(slices.Sorted(slices.Values(r.Perms))) {
		if _, err := tx.Exec("INSERT INTO role_perms(role_id, perm) VALUES(?,?)", r.ID, string(p)); err != nil {
			return 0, err
		}
	}
	return r.ID, tx.Commit()
}

// DeleteRole removes a custom role that no grant uses.
func (s *Store) DeleteRole(id int64) error {
	r, err := s.Role(id)
	if err != nil {
		return err
	}
	if r.Builtin {
		return ErrBuiltin
	}
	if r.Grants > 0 {
		return ErrInUse
	}
	_, err = s.db.Exec("DELETE FROM roles WHERE id=?", id)
	return err
}

// ---------------- Grants ----------------

type Scope struct {
	Level  Level
	Node   string
	Server string
}

func (sc Scope) typ() string {
	switch sc.Level {
	case LevelNode:
		return "node"
	case LevelServer:
		return "server"
	}
	return "global"
}

func scopeFromType(t, node, server string) Scope {
	switch t {
	case "node":
		return Scope{LevelNode, node, ""}
	case "server":
		return Scope{LevelServer, node, server}
	}
	return Scope{}
}

type Grant struct {
	ID            int64
	Group         bool // principal is a group
	PrincipalID   int64
	PrincipalName string
	RoleID        int64
	RoleName      string
	Scope         Scope
	CreatedBy     string
	Created       time.Time
	Expires       time.Time // zero = never
}

func (s *Store) AddGrant(g Grant) (int64, error) {
	t := "user"
	if g.Group {
		t = "group"
	}
	var exp int64
	if !g.Expires.IsZero() {
		exp = g.Expires.Unix()
	}
	node, srv := g.Scope.Node, g.Scope.Server
	if g.Scope.Level == LevelGlobal {
		node, srv = "", ""
	} else if g.Scope.Level == LevelNode {
		srv = ""
	}
	if g.Scope.Level != LevelGlobal && node == "" || g.Scope.Level == LevelServer && srv == "" {
		return 0, errors.New("the scope needs a node and, for a server grant, a server")
	}
	r, err := s.db.Exec(`INSERT INTO grants(principal_type, principal_id, role_id, scope_type, node_id, server_id, created_by, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, t, g.PrincipalID, g.RoleID, g.Scope.typ(), node, srv, g.CreatedBy, now(), exp)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) Grant(id int64) (Grant, error) {
	gs, err := s.ListGrants()
	if err != nil {
		return Grant{}, err
	}
	for _, g := range gs {
		if g.ID == id {
			return g, nil
		}
	}
	return Grant{}, ErrNotFound
}

func (s *Store) DeleteGrant(id int64) error {
	return s.guarded(func(tx *sql.Tx) error {
		r, err := tx.Exec("DELETE FROM grants WHERE id=?", id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListGrants returns every grant, including expired ones (marked by Expires in the past).
func (s *Store) ListGrants() ([]Grant, error) {
	rows, err := s.db.Query(`SELECT g.id, g.principal_type, g.principal_id,
		CASE g.principal_type WHEN 'user' THEN COALESCE((SELECT name FROM users WHERE id=g.principal_id),'?')
		 ELSE COALESCE((SELECT name FROM groups WHERE id=g.principal_id),'?') END,
		g.role_id, r.name, g.scope_type, g.node_id, g.server_id, g.created_by, g.created_at, g.expires_at
		FROM grants g JOIN roles r ON r.id=g.role_id ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var pt, st, node, srv string
		var created, exp int64
		if err := rows.Scan(&g.ID, &pt, &g.PrincipalID, &g.PrincipalName, &g.RoleID, &g.RoleName, &st, &node, &srv, &g.CreatedBy, &created, &exp); err != nil {
			return nil, err
		}
		g.Group = pt == "group"
		g.Scope = scopeFromType(st, node, srv)
		g.Created = time.Unix(created, 0)
		if exp > 0 {
			g.Expires = time.Unix(exp, 0)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GrantsOf returns the grants a user holds directly or through a group.
func (s *Store) GrantsOf(userID int64) ([]Grant, error) {
	all, err := s.ListGrants()
	if err != nil {
		return nil, err
	}
	groups, err := s.UserGroups(userID)
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, g := range all {
		if !g.Group && g.PrincipalID == userID || g.Group && slices.ContainsFunc(groups, func(x Group) bool { return x.ID == g.PrincipalID }) {
			out = append(out, g)
		}
	}
	return out, nil
}

// guarded runs fn in a transaction and rolls it back when it would leave the dashboard without an enabled owner.
func (s *Store) guarded(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before int
	if err := ownerCount(tx).Scan(&before); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	var after int
	if err := ownerCount(tx).Scan(&after); err != nil {
		return err
	}
	if before > 0 && after == 0 {
		return ErrLastOwner
	}
	return tx.Commit()
}

type queryer interface {
	QueryRow(string, ...any) *sql.Row
}

func ownerCount(q queryer) *sql.Row {
	return q.QueryRow(`SELECT COUNT(DISTINCT u.id) FROM users u JOIN grants g ON g.scope_type='global'
		AND g.role_id=(SELECT id FROM roles WHERE name=? AND builtin=1)
		AND (g.expires_at=0 OR g.expires_at>?)
		AND ((g.principal_type='user' AND g.principal_id=u.id)
		  OR (g.principal_type='group' AND g.principal_id IN (SELECT group_id FROM group_members WHERE user_id=u.id)))
		WHERE u.disabled=0`, RoleOwner, now())
}

// OwnerCount is the number of enabled accounts that hold the Owner role globally.
func (s *Store) OwnerCount() int {
	var n int
	_ = ownerCount(s.db).Scan(&n)
	return n
}

// GrantRoleToUser is a shortcut used for the first account and the recovery command.
func (s *Store) GrantRoleToUser(userID int64, role string, by string) error {
	var rid int64
	if err := s.db.QueryRow("SELECT id FROM roles WHERE name=?", role).Scan(&rid); err != nil {
		return ErrNotFound
	}
	_, err := s.AddGrant(Grant{PrincipalID: userID, RoleID: rid, CreatedBy: by})
	return err
}

// ---------------- Sessions ----------------

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

type Session struct {
	Hash      string
	UserID    int64
	Created   time.Time
	LastSeen  time.Time
	Expires   time.Time
	IP        string
	UserAgent string
	Current   bool
}

// NewSession starts a session and returns its secret token (the database only keeps a hash of it).
func (s *Store) NewSession(userID int64, ip, ua string, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	if len(ua) > 200 {
		ua = ua[:200]
	}
	t := now()
	_, err := s.db.Exec("INSERT INTO sessions(id_hash, user_id, created_at, last_seen_at, expires_at, ip, user_agent) VALUES(?,?,?,?,?,?,?)",
		hashToken(tok), userID, t, t, t+int64(ttl.Seconds()), ip, ua)
	if err == nil {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", t)
	}
	return tok, err
}

// SessionUser resolves a session token to its user; expired sessions and disabled users fail.
func (s *Store) SessionUser(tok string) (User, error) {
	if tok == "" {
		return User{}, ErrNotFound
	}
	h := hashToken(tok)
	u, err := scanUser(s.db.QueryRow("SELECT "+userCols+` FROM users WHERE disabled=0 AND id=(SELECT user_id FROM sessions WHERE id_hash=? AND expires_at>?)`, h, now()))
	if err != nil {
		return User{}, err
	}
	_, _ = s.db.Exec("UPDATE sessions SET last_seen_at=? WHERE id_hash=? AND last_seen_at < ?", now(), h, now()-60)
	return u, nil
}

func (s *Store) EndSession(tok string) {
	_, _ = s.db.Exec("DELETE FROM sessions WHERE id_hash=?", hashToken(tok))
}

// EndSessionHash ends the session with the given hash if it belongs to the user.
func (s *Store) EndSessionHash(userID int64, hash string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id_hash=? AND user_id=?", hash, userID)
	return err
}

// EndUserSessions ends every session of a user except the one named by keep (a token, may be "").
func (s *Store) EndUserSessions(userID int64, keep string) (int64, error) {
	r, err := s.db.Exec("DELETE FROM sessions WHERE user_id=? AND id_hash != ?", userID, hashToken(keep))
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func (s *Store) Sessions(userID int64, current string) ([]Session, error) {
	rows, err := s.db.Query("SELECT id_hash, created_at, last_seen_at, expires_at, ip, user_agent FROM sessions WHERE user_id=? AND expires_at>? ORDER BY last_seen_at DESC", userID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cur := hashToken(current)
	var out []Session
	for rows.Next() {
		var x Session
		var c, l, e int64
		if err := rows.Scan(&x.Hash, &c, &l, &e, &x.IP, &x.UserAgent); err != nil {
			return nil, err
		}
		x.UserID, x.Created, x.LastSeen, x.Expires, x.Current = userID, time.Unix(c, 0), time.Unix(l, 0), time.Unix(e, 0), x.Hash == cur
		out = append(out, x)
	}
	return out, rows.Err()
}

// SessionCount is the number of live sessions.
func (s *Store) SessionCount() int {
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE expires_at>?", now()).Scan(&n)
	return n
}

// EnsureOwner makes sure the user holds the Owner role globally.
func (s *Store) EnsureOwner(userID int64) error {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM grants WHERE principal_type='user' AND principal_id=? AND scope_type='global' AND expires_at=0
		AND role_id=(SELECT id FROM roles WHERE name=? AND builtin=1)`, userID, RoleOwner).Scan(&n)
	if err != nil || n > 0 {
		return err
	}
	return s.GrantRoleToUser(userID, RoleOwner, "cli")
}
