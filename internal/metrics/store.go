// Package metrics records the load of a node and its servers in a small SQLite file of its own and answers
// history queries. It is the storage half of the optional statistics feature; sampling lives in the manager.
package metrics

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, keeps the build free of cgo

	"cubeherd/internal/api"
)

// Resolutions in seconds: raw samples, five-minute and hourly buckets.
const (
	Res1m = 60
	Res5m = 300
	Res1h = 3600
)

// Config is the retention of the three resolutions and the size cap.
type Config struct {
	Keep1m, Keep5m, Keep1h time.Duration
	MaxBytes               int64
}

// ConfigFrom turns the setting into durations; zero values become the defaults.
func ConfigFrom(c api.MetricsConfig) Config {
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return Config{
		Keep1m:   time.Duration(pick(c.Keep1mHours, 48)) * time.Hour,
		Keep5m:   time.Duration(pick(c.Keep5mDays, 30)) * 24 * time.Hour,
		Keep1h:   time.Duration(pick(c.Keep1hDays, 365)) * 24 * time.Hour,
		MaxBytes: int64(pick(c.MaxMB, 256)) << 20,
	}
}

// Row is one raw sample of the node (Scope "node") or of a server (its id).
type Row struct {
	Scope string
	P     api.MetricPoint
}

// Store is metrics.db plus the write buffer.
type Store struct {
	db   *sql.DB
	path string

	now func() time.Time // replaced in tests

	mu        sync.Mutex
	cfg       Config
	buf       []Row
	events    []event
	flushedAt time.Time
	capped    bool
}

type event struct {
	ts            int64
	scope, k, det string
}

const schema = `
CREATE TABLE IF NOT EXISTS samples (
  res INTEGER NOT NULL, scope TEXT NOT NULL, ts INTEGER NOT NULL,
  up REAL, cpu REAL, cpu_max REAL, mem REAL, mem_max REAL, disk REAL,
  players REAL, players_max REAL, tps REAL, tps_min REAL, mspt REAL, mspt_max REAL,
  n INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (res, scope, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS events (ts INTEGER NOT NULL, scope TEXT NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS events_scope ON events(scope, ts);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

const schemaVersion = "1"

// Open opens dir/metrics.db. The data is disposable: a file that cannot be read is set aside and a new one is made.
func Open(dir string, cfg Config) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "metrics.db")
	s, err := open(path, cfg)
	if err != nil {
		_ = os.Rename(path, path+".broken-"+time.Now().Format("20060102-150405"))
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")
		s, err = open(path, cfg)
	}
	return s, err
}

func open(path string, cfg Config) (*Store, error) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close()
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	var v string
	_ = db.QueryRow("SELECT value FROM meta WHERE key='schema'").Scan(&v)
	switch v {
	case "":
		_, err = db.Exec("INSERT INTO meta(key,value) VALUES('schema',?)", schemaVersion)
	case schemaVersion:
	default:
		err = fmt.Errorf("metrics.db was written by a newer Cubeherd (schema %s)", v)
	}
	if err == nil {
		var n int
		err = db.QueryRow("SELECT COUNT(*) FROM samples LIMIT 1").Scan(&n) // a damaged file fails here
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path, cfg: cfg, now: time.Now}
	s.flushedAt = s.now()
	return s, nil
}

// SetConfig changes the retention; it applies at the next maintenance.
func (s *Store) SetConfig(c Config) { s.mu.Lock(); s.cfg = c; s.mu.Unlock() }

func (s *Store) Config() Config { s.mu.Lock(); defer s.mu.Unlock(); return s.cfg }

// Close flushes what is buffered and closes the file.
func (s *Store) Close() error {
	_ = s.Flush()
	return s.db.Close()
}

// ---------------- Writing ----------------

// Add buffers raw samples. While the file is over its size cap they are dropped.
func (s *Store) Add(rows ...Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capped {
		return
	}
	for _, r := range rows {
		p := r.P
		// A raw sample is its own average and maximum, which keeps the thinning a plain AVG/MAX.
		if p.CPUMax == nil {
			p.CPUMax = p.CPU
		}
		if p.MemMax == nil {
			p.MemMax = p.Mem
		}
		if p.PlayersMax == nil {
			p.PlayersMax = p.Players
		}
		if p.TPSMin == nil {
			p.TPSMin = p.TPS
		}
		if p.MSPTMax == nil {
			p.MSPTMax = p.MSPT
		}
		r.P = p
		s.buf = append(s.buf, r)
	}
}

// Event records something that happened to a server.
func (s *Store) Event(scope, kind, detail string) {
	s.mu.Lock()
	s.events = append(s.events, event{s.now().Unix(), scope, kind, detail})
	s.mu.Unlock()
}

// Buffered is the number of samples waiting for the next flush.
func (s *Store) Buffered() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.buf) }

// Flush writes the buffer in one transaction.
func (s *Store) Flush() error {
	s.mu.Lock()
	rows, evs := s.buf, s.events
	s.buf, s.events = nil, nil
	s.mu.Unlock()
	if len(rows) == 0 && len(evs) == 0 {
		s.mu.Lock()
		s.flushedAt = s.now() // nothing waits, so everything sampled so far is in the file
		s.mu.Unlock()
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.requeue(rows, evs)
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT OR REPLACE INTO samples(res,scope,ts,up,cpu,cpu_max,mem,mem_max,disk,players,players_max,tps,tps_min,mspt,mspt_max,n)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`)
	if err != nil {
		s.requeue(rows, evs)
		return err
	}
	defer st.Close()
	for _, r := range rows {
		p := r.P
		if _, err := st.Exec(Res1m, r.Scope, p.T, nf(p.Up), nf(p.CPU), nf(p.CPUMax), nf(p.Mem), nf(p.MemMax), nf(p.Disk),
			nf(p.Players), nf(p.PlayersMax), nf(p.TPS), nf(p.TPSMin), nf(p.MSPT), nf(p.MSPTMax)); err != nil {
			s.requeue(rows, evs)
			return err
		}
	}
	for _, e := range evs {
		if _, err := tx.Exec("INSERT INTO events(ts,scope,kind,detail) VALUES(?,?,?,?)", e.ts, e.scope, e.k, e.det); err != nil {
			s.requeue(rows, evs)
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		s.requeue(rows, evs)
		return err
	}
	s.mu.Lock()
	s.flushedAt = s.now()
	s.mu.Unlock()
	return nil
}

func (s *Store) requeue(rows []Row, evs []event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf)+len(rows) > 20000 { // a file that stays unwritable must not grow the buffer without end
		return
	}
	s.buf = append(rows, s.buf...)
	s.events = append(evs, s.events...)
}

func nf(p *float64) any {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	return *p
}

// ---------------- Thinning and retention ----------------

func (s *Store) meta(key string) (int64, bool) {
	var v int64
	if err := s.db.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key=?", key).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

func (s *Store) setMeta(key string, v int64) error {
	_, err := s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, fmt.Sprint(v))
	return err
}

// Maintain flushes, builds the coarser rows from the finer ones, removes what is past retention and checks the size cap.
func (s *Store) Maintain() error {
	if err := s.Flush(); err != nil {
		return err
	}
	s.mu.Lock()
	cfg, flushed := s.cfg, s.flushedAt
	s.mu.Unlock()
	// Rows are stamped when sampling starts and added when it ends, so leave a margin before a bucket counts as complete.
	complete := flushed.Add(-2 * time.Minute).Unix()
	if err := s.rollup(Res1m, Res5m, complete); err != nil {
		return err
	}
	if err := s.rollup(Res5m, Res1h, complete); err != nil {
		return err
	}
	now := s.now().Unix()
	w5, _ := s.meta("rolled300")
	w1h, _ := s.meta("rolled3600")
	cut := func(res int, keep time.Duration, built int64) error {
		c := now - int64(keep.Seconds())
		if built > 0 && built < c {
			c = built // never drop rows that are not yet part of a coarser bucket
		}
		_, err := s.db.Exec("DELETE FROM samples WHERE res=? AND ts<?", res, c)
		return err
	}
	if err := cut(Res1m, cfg.Keep1m, w5); err != nil {
		return err
	}
	if err := cut(Res5m, cfg.Keep5m, w1h); err != nil {
		return err
	}
	if err := cut(Res1h, cfg.Keep1h, 0); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM events WHERE ts<?", now-int64(cfg.Keep1h.Seconds())); err != nil {
		return err
	}
	used := s.Size()
	s.mu.Lock()
	s.capped = cfg.MaxBytes > 0 && used > cfg.MaxBytes
	s.mu.Unlock()
	return nil
}

// rollup builds the dst buckets that are complete by now from the src rows, from where the last run stopped.
func (s *Store) rollup(src, dst int, complete int64) error {
	key := fmt.Sprintf("rolled%d", dst)
	from, ok := s.meta(key)
	if !ok {
		if err := s.db.QueryRow("SELECT COALESCE(MIN(ts),0) FROM samples WHERE res=?", src).Scan(&from); err != nil || from == 0 {
			return err
		}
		from = from / int64(dst) * int64(dst)
	}
	upto := complete / int64(dst) * int64(dst)
	if upto <= from {
		return nil
	}
	_, err := s.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO samples(res,scope,ts,up,cpu,cpu_max,mem,mem_max,disk,players,players_max,tps,tps_min,mspt,mspt_max,n)
		SELECT %d, scope, (ts/%d)*%d, AVG(up), AVG(cpu), MAX(cpu_max), AVG(mem), MAX(mem_max), AVG(disk), AVG(players), MAX(players_max),
		       AVG(tps), MIN(tps_min), AVG(mspt), MAX(mspt_max), SUM(n)
		FROM samples WHERE res=? AND ts>=? AND ts<? GROUP BY scope, (ts/%d)`, dst, dst, dst, dst), src, from, upto)
	if err != nil {
		return err
	}
	return s.setMeta(key, upto)
}

// Size is the space metrics.db really uses, without free pages.
func (s *Store) Size() int64 {
	var pages, free, size int64
	_ = s.db.QueryRow("PRAGMA page_count").Scan(&pages)
	_ = s.db.QueryRow("PRAGMA freelist_count").Scan(&free)
	_ = s.db.QueryRow("PRAGMA page_size").Scan(&size)
	return (pages - free) * size
}

// Purge deletes the history of one scope ("node" or a server id), or of everything when scope is "".
func (s *Store) Purge(scope string) error {
	s.mu.Lock()
	keep := s.buf[:0]
	for _, r := range s.buf {
		if scope != "" && r.Scope != scope {
			keep = append(keep, r)
		}
	}
	s.buf = keep
	s.mu.Unlock()
	if scope == "" {
		if _, err := s.db.Exec("DELETE FROM samples"); err != nil {
			return err
		}
		_, err := s.db.Exec("DELETE FROM events")
		return err
	}
	if _, err := s.db.Exec("DELETE FROM samples WHERE scope=?", scope); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM events WHERE scope=?", scope)
	return err
}

// ---------------- Reading ----------------

var cols = "ts, up, cpu, cpu_max, mem, mem_max, disk, players, players_max, tps, tps_min, mspt, mspt_max, n"

type acc struct {
	w   float64
	sum [13]float64
	cnt [13]float64
	max [13]float64
	min [13]float64
	t   int64
}

// field indexes of acc
const (
	fUp = iota
	fCPU
	fCPUMax
	fMem
	fMemMax
	fDisk
	fPlayers
	fPlayersMax
	fTPS
	fTPSMin
	fMSPT
	fMSPTMax
)

func (a *acc) add(v [12]sql.NullFloat64, n float64) {
	for i, x := range v {
		if !x.Valid {
			continue
		}
		a.sum[i] += x.Float64 * n
		a.cnt[i] += n
		if a.cnt[i] == n || x.Float64 > a.max[i] {
			a.max[i] = x.Float64
		}
		if a.cnt[i] == n || x.Float64 < a.min[i] {
			a.min[i] = x.Float64
		}
	}
}

func (a *acc) point() api.MetricPoint {
	avg := func(i int) *float64 {
		if a.cnt[i] == 0 {
			return nil
		}
		return api.Float(a.sum[i] / a.cnt[i])
	}
	hi := func(i int) *float64 {
		if a.cnt[i] == 0 {
			return nil
		}
		return api.Float(a.max[i])
	}
	lo := func(i int) *float64 {
		if a.cnt[i] == 0 {
			return nil
		}
		return api.Float(a.min[i])
	}
	return api.MetricPoint{T: a.t, Up: avg(fUp), CPU: avg(fCPU), CPUMax: hi(fCPUMax), Mem: avg(fMem), MemMax: hi(fMemMax), Disk: avg(fDisk),
		Players: avg(fPlayers), PlayersMax: hi(fPlayersMax), TPS: avg(fTPS), TPSMin: lo(fTPSMin), MSPT: avg(fMSPT), MSPTMax: hi(fMSPTMax)}
}

// Query returns the series of one scope between from and to, thinned to at most points buckets, and its events.
// It uses the finest resolution whose retention still covers from.
func (s *Store) Query(scope string, from, to time.Time, points int) (api.History, error) {
	if err := s.Flush(); err != nil {
		return api.History{}, err
	}
	if points < 10 {
		points = 10
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	now := s.now()
	if to.After(now) {
		to = now
	}
	res := Res1h
	switch {
	case !from.Before(now.Add(-cfg.Keep1m)):
		res = Res1m
	case !from.Before(now.Add(-cfg.Keep5m)):
		res = Res5m
	}
	h := api.History{Scope: scope, Res: res, From: from.Unix(), To: to.Unix(), Enabled: true}
	span := to.Unix() - from.Unix()
	step := res
	if span/int64(res) > int64(points) {
		step = int(math.Ceil(float64(span)/float64(points)/float64(res))) * res
	}
	h.Step = step
	rows, err := s.db.Query("SELECT "+cols+" FROM samples WHERE res=? AND scope=? AND ts>=? AND ts<? ORDER BY ts", res, scope, from.Unix(), to.Unix()+1)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	var cur *acc
	flush := func() {
		if cur != nil {
			h.Points = append(h.Points, cur.point())
		}
	}
	for rows.Next() {
		var ts, n int64
		var v [12]sql.NullFloat64
		if err := rows.Scan(&ts, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7], &v[8], &v[9], &v[10], &v[11], &n); err != nil {
			return h, err
		}
		b := ts / int64(step) * int64(step)
		if cur == nil || cur.t != b {
			flush()
			cur = &acc{t: b}
		}
		cur.add(v, float64(n))
	}
	if err := rows.Err(); err != nil {
		return h, err
	}
	flush()
	er, err := s.db.Query("SELECT ts, kind, detail FROM events WHERE scope=? AND ts>=? AND ts<? ORDER BY ts LIMIT 500", scope, from.Unix(), to.Unix()+1)
	if err != nil {
		return h, err
	}
	defer er.Close()
	for er.Next() {
		var e api.MetricEvent
		if err := er.Scan(&e.T, &e.Kind, &e.Detail); err != nil {
			return h, err
		}
		h.Events = append(h.Events, e)
	}
	return h, er.Err()
}

// Latest is the newest raw sample of every scope seen in the last ten minutes. It reads the buffer first, so it
// costs no flush.
func (s *Store) Latest() (map[string]api.MetricPoint, error) {
	out := map[string]api.MetricPoint{}
	s.mu.Lock()
	for _, r := range s.buf {
		if old, ok := out[r.Scope]; !ok || r.P.T >= old.T {
			out[r.Scope] = r.P
		}
	}
	s.mu.Unlock()
	since := s.now().Add(-10 * time.Minute).Unix()
	rows, err := s.db.Query(`SELECT scope, `+cols+` FROM samples WHERE res=? AND ts>=? AND ts=(SELECT MAX(ts) FROM samples x WHERE x.res=? AND x.scope=samples.scope)`, Res1m, since, Res1m)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var ts, n int64
		var v [12]sql.NullFloat64
		if err := rows.Scan(&scope, &ts, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7], &v[8], &v[9], &v[10], &v[11], &n); err != nil {
			return out, err
		}
		if _, ok := out[scope]; ok {
			continue
		}
		a := &acc{t: ts}
		a.add(v, 1)
		out[scope] = a.point()
	}
	return out, rows.Err()
}

// Range is the time covered by the stored samples.
func (s *Store) Range() (since, last int64) {
	_ = s.db.QueryRow("SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM samples").Scan(&since, &last)
	return
}

// Capped reports that the size cap stopped recording.
func (s *Store) Capped() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.capped }

// Scopes lists the scopes that have samples, for tests and purging.
func (s *Store) Scopes() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT scope FROM samples")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	sort.Strings(out)
	return out, rows.Err()
}

var ErrOff = errors.New("statistics are not recorded on this node")
