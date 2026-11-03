package access

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Audit outcomes.
const (
	OK     = "ok"
	Fail   = "fail"
	Denied = "denied"
)

// Entry is one row of the audit log.
type Entry struct {
	ID      int64
	Time    time.Time
	ActorID int64
	Actor   string
	IP      string
	Action  string // dotted, for example server.power or user.created
	Node    string
	Server  string
	Target  string // what the action was about: a user, a file path, a command
	Outcome string
	Detail  string // JSON object with extra fields
	Hash    string
	Prev    string
}

func (e Entry) digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", e.Prev, e.Time.UnixMilli(), e.ActorID, e.Actor, e.IP, e.Action, e.Node, e.Server, e.Target, e.Outcome, e.Detail)
	return hex.EncodeToString(h.Sum(nil))
}

// Detail builds the JSON detail of an entry from key/value pairs.
func Detail(kv ...any) string {
	if len(kv) < 2 {
		return ""
	}
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// Record appends an entry to the hash chain.
func (s *Store) Record(e Entry) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		e.Outcome = OK
	}
	if len(e.Target) > 500 {
		e.Target = e.Target[:500] + "…"
	}
	_ = s.db.QueryRow("SELECT hash FROM audit ORDER BY id DESC LIMIT 1").Scan(&e.Prev)
	e.Hash = e.digest()
	r, err := s.db.Exec(`INSERT INTO audit(ts, actor_id, actor_name, ip, action, node_id, server_id, target, outcome, detail, prev_hash, hash)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, e.Time.UnixMilli(), e.ActorID, e.Actor, e.IP, e.Action, e.Node, e.Server, e.Target, e.Outcome, e.Detail, e.Prev, e.Hash)
	if err != nil {
		return err
	}
	if s.AuditStdout != nil {
		e.ID, _ = r.LastInsertId()
		s.AuditStdout(e.JSON())
	}
	return nil
}

// JSON is the entry as one JSON line.
func (e Entry) JSON() []byte {
	m := map[string]any{"id": e.ID, "time": e.Time.UTC().Format(time.RFC3339), "actor": e.Actor, "ip": e.IP, "action": e.Action, "outcome": e.Outcome}
	if e.Node != "" {
		m["node"] = e.Node
	}
	if e.Server != "" {
		m["server"] = e.Server
	}
	if e.Target != "" {
		m["target"] = e.Target
	}
	if e.Detail != "" {
		m["detail"] = json.RawMessage(e.Detail)
	}
	b, _ := json.Marshal(m)
	return b
}

// Filter narrows an audit query.
type Filter struct {
	Actor   string // substring of the user name
	Action  string // prefix, for example "server." or "user.created"
	Outcome string
	Node    string
	Server  string
	Text    string // substring of target or detail
	From    time.Time
	To      time.Time
	// Only limits the result to entries inside these scopes; nil means everything (a global audit.view).
	Only   []Scope
	Limit  int
	Offset int
}

func (f Filter) where() (string, []any) {
	var c []string
	var a []any
	if f.Actor != "" {
		c = append(c, "actor_name LIKE ? ESCAPE '\\'")
		a = append(a, "%"+like(f.Actor)+"%")
	}
	if f.Action != "" {
		c = append(c, "action LIKE ? ESCAPE '\\'")
		a = append(a, like(f.Action)+"%")
	}
	if f.Outcome != "" {
		c = append(c, "outcome=?")
		a = append(a, f.Outcome)
	}
	if f.Node != "" {
		c = append(c, "node_id=?")
		a = append(a, f.Node)
	}
	if f.Server != "" {
		c = append(c, "server_id=?")
		a = append(a, f.Server)
	}
	if f.Text != "" {
		c = append(c, "(target LIKE ? ESCAPE '\\' OR detail LIKE ? ESCAPE '\\')")
		a = append(a, "%"+like(f.Text)+"%", "%"+like(f.Text)+"%")
	}
	if !f.From.IsZero() {
		c = append(c, "ts>=?")
		a = append(a, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		c = append(c, "ts<?")
		a = append(a, f.To.UnixMilli())
	}
	if f.Only != nil {
		var or []string
		for _, sc := range f.Only {
			switch sc.Level {
			case LevelGlobal:
				or = append(or, "1=1")
			case LevelNode:
				or = append(or, "node_id=?")
				a = append(a, sc.Node)
			case LevelServer:
				or = append(or, "(node_id=? AND server_id=?)")
				a = append(a, sc.Node, sc.Server)
			}
		}
		if len(or) == 0 {
			or = []string{"1=0"}
		}
		c = append(c, "("+strings.Join(or, " OR ")+")")
	}
	if len(c) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(c, " AND "), a
}

func like(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

const auditCols = "id, ts, actor_id, actor_name, ip, action, node_id, server_id, target, outcome, detail, prev_hash, hash"

func scanEntry(sc interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	var ts int64
	err := sc.Scan(&e.ID, &ts, &e.ActorID, &e.Actor, &e.IP, &e.Action, &e.Node, &e.Server, &e.Target, &e.Outcome, &e.Detail, &e.Prev, &e.Hash)
	e.Time = time.UnixMilli(ts)
	return e, err
}

// Audit returns the newest entries matching the filter and the total number of matches.
func (s *Store) Audit(f Filter) ([]Entry, int, error) {
	w, args := f.where()
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit"+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := s.db.Query("SELECT "+auditCols+" FROM audit"+w+" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// AuditAll streams every matching entry, oldest first, to fn (used by the export).
func (s *Store) AuditAll(f Filter, fn func(Entry) error) error {
	w, args := f.where()
	rows, err := s.db.Query("SELECT "+auditCols+" FROM audit"+w+" ORDER BY id", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AuditEntry returns one entry.
func (s *Store) AuditEntry(id int64) (Entry, error) {
	e, err := scanEntry(s.db.QueryRow("SELECT "+auditCols+" FROM audit WHERE id=?", id))
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return e, err
}

// VerifyResult is the outcome of checking the hash chain.
type VerifyResult struct {
	Entries int
	OK      bool
	BadID   int64 // first entry whose hash or link does not match
	Reason  string
}

// VerifyAudit recomputes the chain. Entries removed by retention are fine at the start; a break anywhere else is not.
func (s *Store) VerifyAudit() (VerifyResult, error) {
	res := VerifyResult{OK: true}
	prev := ""
	first := true
	err := s.AuditAll(Filter{}, func(e Entry) error {
		res.Entries++
		if !first && e.Prev != prev {
			res.OK, res.BadID, res.Reason = false, e.ID, "link to the previous entry is broken (an entry was removed or inserted)"
			return errStop
		}
		if e.digest() != e.Hash {
			res.OK, res.BadID, res.Reason = false, e.ID, "content does not match its hash (the entry was changed)"
			return errStop
		}
		prev, first = e.Hash, false
		return nil
	})
	if err != nil && err != errStop {
		return res, err
	}
	return res, nil
}

var errStop = fmt.Errorf("stop")

// PruneAudit deletes entries older than the given age and records that it did. Zero keeps everything.
func (s *Store) PruneAudit(olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cut := time.Now().Add(-olderThan)
	s.auditMu.Lock()
	r, err := s.db.Exec("DELETE FROM audit WHERE ts<?", cut.UnixMilli())
	s.auditMu.Unlock()
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		_ = s.Record(Entry{Action: "audit.pruned", Actor: "system", Detail: Detail("removed", n, "before", cut.UTC().Format(time.RFC3339))})
	}
	return n, nil
}

// AuditCount is the number of entries.
func (s *Store) AuditCount() int {
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&n)
	return n
}
