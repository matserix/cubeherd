// Package auth handles the single admin login with a signed, stateless session cookie.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const Cookie = "fc_session"

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// Sessions signs cookies with HMAC-SHA256. Changing the password hash invalidates all sessions,
// because the hash prefix is part of the signed payload.
type Sessions struct {
	Secret func() (secret, pwHash string)
	Days   func() int
	Secure bool
}

func (s Sessions) sign(payload string) string {
	secret, pw := s.Secret()
	key, _ := hex.DecodeString(secret)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload + "|" + pw))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s Sessions) Issue(w http.ResponseWriter) {
	days := s.Days()
	if days <= 0 {
		days = 7
	}
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	payload := strconv.FormatInt(exp.Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: Cookie, Value: payload + "." + s.sign(payload), Path: "/", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.Secure,
	})
}

func (s Sessions) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: Cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s Sessions) Valid(r *http.Request) bool {
	c, err := r.Cookie(Cookie)
	if err != nil {
		return false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	return err == nil && time.Now().Unix() < exp
}

// Limiter slows down repeated failed logins per client IP.
type Limiter struct {
	mu   sync.Mutex
	fail map[string]*attempt
}

type attempt struct {
	n    int
	last time.Time
}

func NewLimiter() *Limiter { return &Limiter{fail: map[string]*attempt{}} }

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// Wait returns how long the client must wait before the next attempt.
func (l *Limiter) Wait(r *http.Request) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.fail[clientIP(r)]
	if a == nil || a.n < 5 {
		return 0
	}
	block := time.Duration(1<<min(a.n-5, 8)) * time.Second
	if d := time.Until(a.last.Add(block)); d > 0 {
		return d
	}
	return 0
}

func (l *Limiter) Fail(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ip := clientIP(r)
	a := l.fail[ip]
	if a == nil {
		if len(l.fail) > 1000 {
			l.fail = map[string]*attempt{}
		}
		a = &attempt{}
		l.fail[ip] = a
	}
	a.n++
	a.last = time.Now()
}

func (l *Limiter) Reset(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fail, clientIP(r))
}
