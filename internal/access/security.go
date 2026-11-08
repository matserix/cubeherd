package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------- Two-factor sign-in (TOTP, RFC 6238) ----------------

// NewTOTPSecret returns a random shared secret in the base32 form authenticator apps expect.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func totpKey(secret string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
}

// totpAt is the six-digit code for one 30 second step.
func totpAt(key []byte, step int64) string {
	m := hmac.New(sha1.New, key)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], uint64(step))
	m.Write(c[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// TOTPURI is the otpauth address that authenticator apps read from the QR code.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// checkTOTP accepts the code for the current step and the steps either side of it (clock drift), and returns the
// step that matched. Steps at or before lastStep are refused, so a code cannot be used twice.
func checkTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	key, err := totpKey(secret)
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if err != nil || len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / 30
	for d := int64(-1); d <= 1; d++ {
		step := cur + d
		if step > lastStep && subtle.ConstantTimeCompare([]byte(totpAt(key, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// ErrBadCode means a two-factor or recovery code did not match.
var ErrBadCode = errors.New("the code is wrong or was already used")

func (s *Store) totpState(userID int64) (secret string, enabled bool, last int64, err error) {
	var enc string
	err = s.db.QueryRow("SELECT totp_secret, totp_enabled, totp_step FROM users WHERE id=?", userID).Scan(&enc, &enabled, &last)
	if err != nil {
		return "", false, 0, err
	}
	secret, err = s.open(enc)
	return
}

// EnableTOTP turns two-factor sign-in on once the user proves the app works by entering a current code. It returns
// ten single-use recovery codes, shown once.
func (s *Store) EnableTOTP(userID int64, secret, code string, at time.Time) ([]string, error) {
	step, ok := checkTOTP(secret, code, at, 0)
	if !ok {
		return nil, ErrBadCode
	}
	enc, err := s.seal(secret)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec("UPDATE users SET totp_secret=?, totp_enabled=1, totp_step=? WHERE id=?", enc, step, userID); err != nil {
		return nil, err
	}
	return s.NewRecoveryCodes(userID)
}

// VerifyTOTP checks a code at sign-in. A code that was accepted once is refused afterwards.
func (s *Store) VerifyTOTP(userID int64, code string, at time.Time) bool {
	secret, enabled, last, err := s.totpState(userID)
	if err != nil || !enabled {
		return false
	}
	step, ok := checkTOTP(secret, code, at, last)
	if !ok {
		return false
	}
	// only the first request to present this step wins
	r, err := s.db.Exec("UPDATE users SET totp_step=? WHERE id=? AND totp_step<?", step, userID, step)
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n == 1
}

// DisableTOTP removes the second factor and its recovery codes.
func (s *Store) DisableTOTP(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE users SET totp_secret='', totp_enabled=0, totp_step=0 WHERE id=?", userID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM recovery_codes WHERE user_id=?", userID); err != nil {
		return err
	}
	return tx.Commit()
}

func normaliseRecovery(c string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(c)))
}

// NewRecoveryCodes replaces the user's recovery codes with ten new ones and returns them (the database keeps hashes).
func (s *Store) NewRecoveryCodes(userID int64) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM recovery_codes WHERE user_id=?", userID); err != nil {
		return nil, err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	var out []string
	for i := 0; i < 10; i++ {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		raw := strings.ToLower(enc.EncodeToString(b))[:10]
		if _, err := tx.Exec("INSERT INTO recovery_codes(user_id, hash) VALUES(?,?)", userID, hashToken(raw)); err != nil {
			return nil, err
		}
		out = append(out, raw[:5]+"-"+raw[5:])
	}
	return out, tx.Commit()
}

// UseRecoveryCode accepts a recovery code once.
func (s *Store) UseRecoveryCode(userID int64, code string) bool {
	r, err := s.db.Exec("DELETE FROM recovery_codes WHERE user_id=? AND hash=?", userID, hashToken(normaliseRecovery(code)))
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n == 1
}

// RecoveryCodesLeft is how many unused recovery codes remain.
func (s *Store) RecoveryCodesLeft(userID int64) int {
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM recovery_codes WHERE user_id=?", userID).Scan(&n)
	return n
}

// ---------------- Single-use tokens (reset, verification, "this was not me", pending sign-in) ----------------

// Token kinds.
const (
	TokenReset  = "reset"
	TokenVerify = "verify"
	TokenNotMe  = "notme"
	TokenLogin2 = "login2"     // a password was accepted, the second factor is still to come
	TokenSetup2 = "totp-setup" // a secret shown on the setup page, not yet confirmed
)

// Token is what a valid token stands for.
type Token struct {
	UserID  int64
	Kind    string
	Data    string
	Expires time.Time
}

// ErrToken means the token is unknown, expired or already used.
var ErrToken = errors.New("the link is invalid or has expired")

// NewToken makes a random token for a user; the database stores only its hash. Older unused tokens of the same
// kind for the user stay valid until they expire, except for reset and verify, where a new one replaces them.
func (s *Store) NewToken(userID int64, kind string, ttl time.Duration, data string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	if kind == TokenReset || kind == TokenVerify || kind == TokenLogin2 {
		_, _ = s.db.Exec("DELETE FROM tokens WHERE user_id=? AND kind=?", userID, kind)
	}
	t := now()
	_, err := s.db.Exec("INSERT INTO tokens(id_hash, user_id, kind, created_at, expires_at, data) VALUES(?,?,?,?,?,?)",
		hashToken(tok), userID, kind, t, t+int64(ttl.Seconds()), data)
	if err == nil {
		_, _ = s.db.Exec("DELETE FROM tokens WHERE expires_at < ?", t-86400)
	}
	return tok, err
}

// PeekToken checks a token without using it.
func (s *Store) PeekToken(tok, kind string) (Token, error) {
	var t Token
	var exp int64
	var used bool
	err := s.db.QueryRow("SELECT user_id, kind, data, expires_at, used FROM tokens WHERE id_hash=? AND kind=?", hashToken(tok), kind).Scan(&t.UserID, &t.Kind, &t.Data, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (used || exp <= now()) {
		return Token{}, ErrToken
	}
	if err != nil {
		return Token{}, err
	}
	t.Expires = time.Unix(exp, 0)
	return t, nil
}

// ConsumeToken uses a token; only the first caller gets it.
func (s *Store) ConsumeToken(tok, kind string) (Token, error) {
	t, err := s.PeekToken(tok, kind)
	if err != nil {
		return Token{}, err
	}
	r, err := s.db.Exec("UPDATE tokens SET used=1 WHERE id_hash=? AND used=0 AND expires_at>?", hashToken(tok), now())
	if err != nil {
		return Token{}, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return Token{}, ErrToken
	}
	return t, nil
}

// SealSecret and OpenSecret let the web layer keep a short-lived secret in a token's data field.
func (s *Store) SealSecret(plain string) (string, error) { return s.seal(plain) }
func (s *Store) OpenSecret(enc string) (string, error)   { return s.open(enc) }

// ---------------- Known devices ----------------

// SeenDevice records a sign-in from a device (identified by a random cookie value) and reports whether the user had
// signed in from it before.
func (s *Store) SeenDevice(userID int64, device, ip, ua string) bool {
	if device == "" {
		return false
	}
	h := hashToken(device)
	if len(ua) > 200 {
		ua = ua[:200]
	}
	r, err := s.db.Exec("UPDATE devices SET last_seen=?, ip=?, ua=? WHERE user_id=? AND hash=?", now(), ip, ua, userID, h)
	if err == nil {
		if n, _ := r.RowsAffected(); n == 1 {
			return true
		}
	}
	_, _ = s.db.Exec("INSERT OR IGNORE INTO devices(user_id, hash, first_seen, last_seen, ip, ua) VALUES(?,?,?,?,?,?)", userID, h, now(), now(), ip, ua)
	return false
}

// ---------------- Sessions with a method ----------------

// NewSessionVia is NewSession that also notes how the user signed in ("password" or "sso").
func (s *Store) NewSessionVia(userID int64, ip, ua string, ttl time.Duration, method string) (string, error) {
	tok, err := s.NewSession(userID, ip, ua, ttl)
	if err == nil {
		_, _ = s.db.Exec("UPDATE sessions SET method=? WHERE id_hash=?", method, hashToken(tok))
	}
	return tok, err
}

// SessionMethod is how the session was started.
func (s *Store) SessionMethod(tok string) string {
	var m string
	_ = s.db.QueryRow("SELECT method FROM sessions WHERE id_hash=?", hashToken(tok)).Scan(&m)
	return m
}

// ---------------- Account security settings ----------------

// Registration modes.
const (
	RegClosed   = "closed"
	RegOpen     = "open"
	RegApproval = "approval"
)

// Security is the admin's policy for accounts.
type Security struct {
	Registration    string `json:"registration"`    // closed | open | approval
	RequireVerified bool   `json:"requireVerified"` // a self-registered account must confirm its e-mail address first
	Domains         string `json:"domains,omitempty"`
	Notice          string `json:"notice"` // off | new | every
	TwoFA           string `json:"twofa"`  // off | privileged | all
	GraceDays       int    `json:"graceDays"`
	TrustSSO        bool   `json:"trustSso"`
	TwoFASince      int64  `json:"twofaSince,omitempty"` // when the policy was switched on, for the grace period
}

var secCache struct {
	sync.Mutex
	st  *Store
	at  time.Time
	val Security
}

// Security returns the policy (defaults: nothing on).
func (s *Store) Security() Security {
	secCache.Lock()
	defer secCache.Unlock()
	if secCache.st == s && time.Since(secCache.at) < 3*time.Second {
		return secCache.val
	}
	v := Security{Registration: RegClosed, Notice: "off", TwoFA: "off", GraceDays: 7}
	if raw := s.KV("sec.policy"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &v)
	}
	secCache.st, secCache.at, secCache.val = s, time.Now(), v
	return v
}

// SaveSecurity stores the policy; switching two-factor on notes the time for the grace period.
func (s *Store) SaveSecurity(v Security) error {
	old := s.Security()
	if v.TwoFA != "off" && (old.TwoFA == "off" || old.TwoFA != v.TwoFA && old.TwoFASince == 0) {
		v.TwoFASince = now()
	} else if v.TwoFA == "off" {
		v.TwoFASince = 0
	} else {
		v.TwoFASince = old.TwoFASince
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := s.SetKV("sec.policy", string(b)); err != nil {
		return err
	}
	secCache.Lock()
	secCache.st = nil
	secCache.Unlock()
	return nil
}

// TOTPCode is the current code for a secret; tests and the lab use it in place of an authenticator app.
func TOTPCode(secret string, at time.Time) string {
	key, err := totpKey(secret)
	if err != nil {
		return ""
	}
	return totpAt(key, at.Unix()/30)
}
