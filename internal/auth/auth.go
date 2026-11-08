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

const Cookie = "cubeherd_session"

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
}

func (s Sessions) sign(payload string) string {
	secret, pw := s.Secret()
	key, _ := hex.DecodeString(secret)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload + "|" + pw))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s Sessions) Issue(w http.ResponseWriter, secure bool) {
	days := s.Days()
	if days <= 0 {
		days = 7
	}
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	payload := strconv.FormatInt(exp.Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: Cookie, Value: payload + "." + s.sign(payload), Path: "/", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure,
	})
}

// Set stores an opaque server-side session token in the cookie (accounts mode).
func (s Sessions) Set(w http.ResponseWriter, token string, secure bool) {
	days := s.Days()
	if days <= 0 {
		days = 7
	}
	http.SetCookie(w, &http.Cookie{
		Name: Cookie, Value: token, Path: "/", Expires: time.Now().Add(time.Duration(days) * 24 * time.Hour),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure,
	})
}

func (s Sessions) Clear(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: Cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure})
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

// Limiter slows down repeated failed logins per client IP or other caller-chosen key (such as a user name,
// which a caller supplies and so does not control what keys exist, but does control their number).
type Limiter struct {
	mu   sync.Mutex
	fail map[string]*attempt
}

type attempt struct {
	n    int
	last time.Time
}

// maxKeys bounds memory under a flood of distinct keys (for example made-up user names on /login). Once it is
// reached, FailKey makes room by evicting one entry via evict, which never removes a key that is already being
// throttled while a key that is not yet throttled remains, so a flood of new keys cannot clear a lockout early.
const maxKeys = 1000

func NewLimiter() *Limiter { return &Limiter{fail: map[string]*attempt{}} }

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// Wait returns how long the client must wait before the next attempt.
func (l *Limiter) Wait(r *http.Request) time.Duration { return l.WaitKey(clientIP(r)) }

func (l *Limiter) Fail(r *http.Request) { l.FailKey(clientIP(r)) }

// WaitKey, FailKey and ResetKey do the same for an arbitrary key, such as a user name.
func (l *Limiter) WaitKey(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wait(key)
}

// blockThreshold is how many failures a key may have before it is throttled at all.
const blockThreshold = 5

func (l *Limiter) wait(key string) time.Duration {
	a := l.fail[key]
	if a == nil || a.n < blockThreshold {
		return 0
	}
	block := time.Duration(1<<min(a.n-blockThreshold, 8)) * time.Second
	if d := time.Until(a.last.Add(block)); d > 0 {
		return d
	}
	return 0
}

func (l *Limiter) ResetKey(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fail, key)
}

func (l *Limiter) FailKey(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.fail[key]
	if a == nil {
		if len(l.fail) >= maxKeys {
			l.evict()
		}
		a = &attempt{}
		l.fail[key] = a
	}
	a.n++
	a.last = time.Now()
}

// evict makes room for one new key by dropping an existing one: preferably the oldest key that is not yet
// throttled (n < blockThreshold), so a flood of brand-new, still-untouched keys cannot push out a key whose
// lockout is already running. Only once every key is already throttled does it fall back to the oldest of those.
func (l *Limiter) evict() {
	var victim string
	var victimLast time.Time
	victimBlocking := true
	for k, a := range l.fail {
		blocking := a.n >= blockThreshold
		switch {
		case victim == "":
			victim, victimLast, victimBlocking = k, a.last, blocking
		case victimBlocking && !blocking:
			victim, victimLast, victimBlocking = k, a.last, blocking
		case blocking == victimBlocking && a.last.Before(victimLast):
			victim, victimLast = k, a.last
		}
	}
	if victim != "" {
		delete(l.fail, victim)
	}
}

func (l *Limiter) Reset(r *http.Request) { l.ResetKey(clientIP(r)) }
