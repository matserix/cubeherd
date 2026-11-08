package access

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure Go driver, keeps the build free of cgo
)

// Store is the accounts database.
type Store struct {
	db   *sql.DB
	path string

	auditMu sync.Mutex // serialises writes to the audit hash chain
	// AuditStdout, when set, receives every audit entry as one JSON line.
	AuditStdout func(line []byte)
}

var (
	ErrNotFound  = errors.New("not found")
	ErrExists    = errors.New("already exists")
	ErrLastOwner = errors.New("this would remove the last owner")
	ErrBuiltin   = errors.New("built-in roles cannot be changed")
	ErrInUse     = errors.New("still in use")
)

// Open opens (and migrates) the database at path; the file is created with mode 0600.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close()
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	if err := s.syncBuiltins(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path is the database file.
func (s *Store) Path() string { return s.path }

// Snapshot writes a consistent copy of the database to dst.
func (s *Store) Snapshot(dst string) error {
	_ = os.Remove(dst)
	_, err := s.db.Exec("VACUUM INTO ?", dst)
	return err
}

// Size is the size of the database file in bytes.
func (s *Store) Size() int64 {
	var pages, size int64
	_ = s.db.QueryRow("PRAGMA page_count").Scan(&pages)
	_ = s.db.QueryRow("PRAGMA page_size").Scan(&size)
	return pages * size
}

var migrations = []string{`
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL DEFAULT '',
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_login_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE identities (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issuer TEXT NOT NULL,
  subject TEXT NOT NULL,
  email TEXT NOT NULL DEFAULT '',
  UNIQUE(issuer, subject)
);
CREATE TABLE groups (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  oidc_value TEXT NOT NULL DEFAULT ''
);
CREATE TABLE group_members (
  group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  mapped INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (group_id, user_id)
);
CREATE TABLE roles (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  builtin INTEGER NOT NULL DEFAULT 0,
  description TEXT NOT NULL DEFAULT ''
);
CREATE TABLE role_perms (
  role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  perm TEXT NOT NULL,
  PRIMARY KEY (role_id, perm)
);
CREATE TABLE grants (
  id INTEGER PRIMARY KEY,
  principal_type TEXT NOT NULL CHECK (principal_type IN ('user','group')),
  principal_id INTEGER NOT NULL,
  role_id INTEGER NOT NULL REFERENCES roles(id),
  scope_type TEXT NOT NULL CHECK (scope_type IN ('global','node','server')),
  node_id TEXT NOT NULL DEFAULT '',
  server_id TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX grants_principal ON grants(principal_type, principal_id);
CREATE TABLE sessions (
  id_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE audit (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  actor_id INTEGER NOT NULL DEFAULT 0,
  actor_name TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  node_id TEXT NOT NULL DEFAULT '',
  server_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT 'ok',
  detail TEXT NOT NULL DEFAULT '',
  prev_hash TEXT NOT NULL DEFAULT '',
  hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_ts ON audit(ts);
CREATE INDEX audit_actor ON audit(actor_id);
CREATE INDEX audit_scope ON audit(node_id, server_id);
CREATE TABLE kv (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`, `
ALTER TABLE users ADD COLUMN email_verified INTEGER NOT NULL DEFAULT 1;
`, `
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_step INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE users ADD COLUMN force_reset INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN method TEXT NOT NULL DEFAULT 'password';
CREATE TABLE recovery_codes (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash TEXT NOT NULL,
  PRIMARY KEY (user_id, hash)
);
CREATE TABLE tokens (
  id_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used INTEGER NOT NULL DEFAULT 0,
  data TEXT NOT NULL DEFAULT ''
);
CREATE INDEX tokens_user ON tokens(user_id, kind);
CREATE TABLE devices (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  ua TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, hash)
);
`, `
INSERT OR IGNORE INTO role_perms(role_id, perm) SELECT role_id, 'server.resources' FROM role_perms WHERE perm='server.settings';
INSERT OR IGNORE INTO role_perms(role_id, perm) SELECT role_id, 'server.network' FROM role_perms WHERE perm='server.settings';
INSERT OR IGNORE INTO role_perms(role_id, perm) SELECT role_id, 'server.advanced' FROM role_perms WHERE perm='server.settings';
`, `
-- A group's mapped claim value used to be matched against every configured provider, so two providers that
-- happen to send the same group claim value could map a sign-in through either of them onto the same group.
-- "" keeps matching any provider (a single-provider setup is unaffected); a non-empty value scopes the match.
ALTER TABLE groups ADD COLUMN oidc_provider TEXT NOT NULL DEFAULT '';
`}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("the database was written by a newer Cubeherd (schema %d)", v)
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// syncBuiltins creates the built-in roles and rewrites their permissions, so a new release can add permissions.
func (s *Store) syncBuiltins() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range builtins {
		if _, err := tx.Exec(`INSERT INTO roles(name, builtin, description) VALUES(?,1,?)
			ON CONFLICT(name) DO UPDATE SET builtin=1, description=excluded.description`, b.Name, b.Description); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow("SELECT id FROM roles WHERE name=?", b.Name).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM role_perms WHERE role_id=?", id); err != nil {
			return err
		}
		for _, p := range b.Perms {
			if _, err := tx.Exec("INSERT INTO role_perms(role_id, perm) VALUES(?,?)", id, string(p)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// KV is a tiny key-value table for provider settings and the like.
func (s *Store) KV(key string) string {
	var v string
	_ = s.db.QueryRow("SELECT value FROM kv WHERE key=?", key).Scan(&v)
	return v
}

func (s *Store) SetKV(key, value string) error {
	_, err := s.db.Exec("INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
