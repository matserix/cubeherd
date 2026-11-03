package access

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Provider is one configured identity provider (OpenID Connect). Google is the same protocol with a preset.
type Provider struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"` // "google" or "oidc"
	Label         string `json:"label"`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"clientId"`
	ClientSecret  string `json:"-"`
	SecretEnc     string `json:"secret"` // AES-GCM, key in access.key next to the database
	Scopes        string `json:"scopes,omitempty"`
	Domains       string `json:"domains,omitempty"` // allowed e-mail domains, comma separated; "" = any
	HostedDomain  string `json:"hd,omitempty"`      // Google Workspace domain
	LinkByEmail   bool   `json:"linkByEmail,omitempty"`
	AutoProvision bool   `json:"autoProvision,omitempty"`
	DefaultRole   int64  `json:"defaultRole,omitempty"` // role given globally to auto-provisioned users, 0 = none
	GroupClaim    string `json:"groupClaim,omitempty"`
}

// GoogleIssuer is the issuer of the Google preset.
const GoogleIssuer = "https://accounts.google.com"

func (p Provider) AllowsDomain(email string) bool {
	if strings.TrimSpace(p.Domains) == "" {
		return true
	}
	_, dom, ok := strings.Cut(strings.ToLower(email), "@")
	if !ok {
		return false
	}
	for _, d := range strings.FieldsFunc(strings.ToLower(p.Domains), func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\n' }) {
		if strings.TrimPrefix(d, "@") == dom {
			return true
		}
	}
	return false
}

// ---------------- Storage ----------------

func (s *Store) secretKey() ([]byte, error) {
	p := filepath.Join(filepath.Dir(s.path), "access.key")
	if b, err := os.ReadFile(p); err == nil && len(b) == 32 {
		return b, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, os.WriteFile(p, k, 0o600)
}

func (s *Store) seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *Store) open(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	if err != nil || len(raw) < g.NonceSize() {
		return "", errors.New("the stored client secret is unreadable")
	}
	out, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
	if err != nil {
		return "", errors.New("the stored client secret does not match this installation's key")
	}
	return string(out), nil
}

// Providers lists the configured providers with their client secrets decrypted.
func (s *Store) Providers() ([]Provider, error) {
	var ps []Provider
	if raw := s.KV("sso.providers"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &ps); err != nil {
			return nil, err
		}
	}
	for i := range ps {
		sec, err := s.open(ps[i].SecretEnc)
		if err != nil {
			return nil, err
		}
		ps[i].ClientSecret = sec
	}
	return ps, nil
}

// SaveProviders stores the providers; secrets are encrypted before they reach the database.
func (s *Store) SaveProviders(ps []Provider) error {
	for i := range ps {
		enc, err := s.seal(ps[i].ClientSecret)
		if err != nil {
			return err
		}
		ps[i].SecretEnc = enc
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	return s.SetKV("sso.providers", string(b))
}

// LocalSignInOff reports whether password sign-in is switched off (only SSO works).
func (s *Store) LocalSignInOff() bool { return s.KV("sso.nolocal") == "1" }

func (s *Store) SetLocalSignInOff(off bool) error {
	v := "0"
	if off {
		v = "1"
	}
	return s.SetKV("sso.nolocal", v)
}

// HasSSOOwner reports whether an enabled global owner has a linked identity, so password sign-in can be switched off safely.
func (s *Store) HasSSOOwner() bool {
	us, _ := s.ListUsers()
	for _, u := range us {
		if u.Disabled {
			continue
		}
		p, err := s.Principal(u)
		if err != nil || !p.Global(UsersManage) {
			continue
		}
		if ids, _ := s.Identities(u.ID); len(ids) > 0 {
			return true
		}
	}
	return false
}

// ---------------- Protocol ----------------

// Claims are the parts of an ID token Cubeherd looks at.
type Claims struct {
	Issuer, Subject, Email, Name, Username, HostedDomain, Nonce string
	EmailVerified                                               bool
	Groups                                                      []string
}

type discovery struct {
	Issuer  string `json:"issuer"`
	Auth    string `json:"authorization_endpoint"`
	Token   string `json:"token_endpoint"`
	JWKSURI string `json:"jwks_uri"`
}

// OIDC talks to identity providers. One instance caches discovery documents and signing keys.
type OIDC struct {
	HTTP *http.Client

	mu   sync.Mutex
	disc map[string]cachedDisc
	keys map[string]cachedKeys
}

type cachedDisc struct {
	d  discovery
	at time.Time
}

type cachedKeys struct {
	keys []jwk
	at   time.Time
}

func NewOIDC() *OIDC {
	return &OIDC{HTTP: &http.Client{Timeout: 10 * time.Second}, disc: map[string]cachedDisc{}, keys: map[string]cachedKeys{}}
}

func (o *OIDC) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func (o *OIDC) discover(ctx context.Context, issuer string) (discovery, error) {
	issuer = strings.TrimRight(issuer, "/")
	o.mu.Lock()
	c, ok := o.disc[issuer]
	o.mu.Unlock()
	if ok && time.Since(c.at) < time.Hour {
		return c.d, nil
	}
	var d discovery
	if err := o.getJSON(ctx, issuer+"/.well-known/openid-configuration", &d); err != nil {
		return d, err
	}
	if strings.TrimRight(d.Issuer, "/") != issuer || d.Auth == "" || d.Token == "" || d.JWKSURI == "" {
		return d, errors.New("the discovery document does not match the issuer")
	}
	o.mu.Lock()
	o.disc[issuer] = cachedDisc{d, time.Now()}
	o.mu.Unlock()
	return d, nil
}

// Check fetches the discovery document, which proves the issuer URL is right.
func (o *OIDC) Check(ctx context.Context, issuer string) error {
	_, err := o.discover(ctx, issuer)
	return err
}

// Pending is what the dashboard remembers between sending the user away and their return.
type Pending struct {
	Provider string
	Verifier string
	Nonce    string
	Test     bool
	Created  time.Time
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Begin builds the URL the browser is sent to and the state to remember for the return.
func (o *OIDC) Begin(ctx context.Context, p Provider, redirect string, test bool) (authURL, state string, pend Pending, err error) {
	d, err := o.discover(ctx, p.Issuer)
	if err != nil {
		return "", "", Pending{}, err
	}
	verifier := randString(48)
	sum := sha256.Sum256([]byte(verifier))
	state, nonce := randString(24), randString(24)
	scopes := strings.Join(strings.Fields(p.Scopes), " ")
	if scopes == "" {
		scopes = "openid email profile"
	}
	q := url.Values{
		"response_type": {"code"}, "client_id": {p.ClientID}, "redirect_uri": {redirect}, "scope": {scopes},
		"state": {state}, "nonce": {nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	if p.HostedDomain != "" {
		q.Set("hd", p.HostedDomain)
	}
	if p.Kind == "google" {
		q.Set("prompt", "select_account")
	}
	sep := "?"
	if strings.Contains(d.Auth, "?") {
		sep = "&"
	}
	return d.Auth + sep + q.Encode(), state, Pending{Provider: p.ID, Verifier: verifier, Nonce: nonce, Test: test, Created: time.Now()}, nil
}

// Exchange trades the code for tokens and returns the validated claims of the ID token.
func (o *OIDC) Exchange(ctx context.Context, p Provider, pend Pending, code, redirect string) (Claims, error) {
	d, err := o.discover(ctx, p.Issuer)
	if err != nil {
		return Claims{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {p.ClientID}, "code_verifier": {pend.Verifier}}
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return Claims{}, err
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil || resp.StatusCode != 200 {
		if tok.Error != "" {
			return Claims{}, fmt.Errorf("the provider refused the code: %s %s", tok.Error, tok.Desc)
		}
		return Claims{}, fmt.Errorf("token request failed (%s)", resp.Status)
	}
	if tok.IDToken == "" {
		return Claims{}, errors.New("the provider returned no ID token")
	}
	return o.verify(ctx, d, p, tok.IDToken, pend.Nonce)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (o *OIDC) jwks(ctx context.Context, uri string, force bool) ([]jwk, error) {
	o.mu.Lock()
	c, ok := o.keys[uri]
	o.mu.Unlock()
	if ok && !force && time.Since(c.at) < 10*time.Minute {
		return c.keys, nil
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := o.getJSON(ctx, uri, &set); err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.keys[uri] = cachedKeys{set.Keys, time.Now()}
	o.mu.Unlock()
	return set.Keys, nil
}

func b64(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(s); return b }

func (k jwk) verify(alg string, signed, sig []byte) bool {
	h := sha256.Sum256(signed)
	switch {
	case alg == "RS256" && k.Kty == "RSA":
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(b64(k.N)), E: int(new(big.Int).SetBytes(b64(k.E)).Int64())}
		return pub.E > 1 && rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) == nil
	case alg == "ES256" && k.Kty == "EC" && k.Crv == "P-256":
		if len(sig) != 64 {
			return false
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(b64(k.X)), Y: new(big.Int).SetBytes(b64(k.Y))}
		return pub.Curve.IsOnCurve(pub.X, pub.Y) && ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	}
	return false
}

// verify checks signature, issuer, audience, expiry and nonce of an ID token.
func (o *OIDC) verify(ctx context.Context, d discovery, p Provider, token, nonce string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("the ID token is malformed")
	}
	var hdr struct{ Alg, Kid string }
	if err := json.Unmarshal(b64(parts[0]), &hdr); err != nil {
		return Claims{}, errors.New("the ID token header is malformed")
	}
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" {
		return Claims{}, fmt.Errorf("the ID token uses an unsupported algorithm (%s)", hdr.Alg)
	}
	signed, sig := []byte(parts[0]+"."+parts[1]), b64(parts[2])
	ok := false
	for attempt := 0; attempt < 2 && !ok; attempt++ {
		keys, err := o.jwks(ctx, d.JWKSURI, attempt == 1)
		if err != nil {
			return Claims{}, err
		}
		for _, k := range keys {
			if (hdr.Kid == "" || k.Kid == hdr.Kid) && (k.Use == "" || k.Use == "sig") && k.verify(hdr.Alg, signed, sig) {
				ok = true
				break
			}
		}
	}
	if !ok {
		return Claims{}, errors.New("the ID token signature is not valid")
	}
	var raw map[string]any
	if err := json.Unmarshal(b64(parts[1]), &raw); err != nil {
		return Claims{}, errors.New("the ID token payload is malformed")
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	if strings.TrimRight(str("iss"), "/") != strings.TrimRight(p.Issuer, "/") {
		return Claims{}, errors.New("the ID token comes from a different issuer")
	}
	aud := false
	switch v := raw["aud"].(type) {
	case string:
		aud = v == p.ClientID
	case []any:
		for _, a := range v {
			aud = aud || a == p.ClientID
		}
	}
	if !aud {
		return Claims{}, errors.New("the ID token is meant for a different client")
	}
	exp, _ := raw["exp"].(float64)
	if exp == 0 || time.Now().Unix() > int64(exp)+60 {
		return Claims{}, errors.New("the ID token has expired")
	}
	if nbf, _ := raw["nbf"].(float64); nbf > 0 && time.Now().Unix()+60 < int64(nbf) {
		return Claims{}, errors.New("the ID token is not valid yet")
	}
	if str("nonce") != nonce {
		return Claims{}, errors.New("the ID token does not belong to this sign-in attempt")
	}
	c := Claims{Issuer: str("iss"), Subject: str("sub"), Email: str("email"), Name: str("name"), Username: str("preferred_username"), HostedDomain: str("hd"), Nonce: str("nonce")}
	switch v := raw["email_verified"].(type) {
	case bool:
		c.EmailVerified = v
	case string:
		c.EmailVerified = v == "true"
	}
	if c.Subject == "" {
		return Claims{}, errors.New("the ID token has no subject")
	}
	if p.GroupClaim != "" {
		switch v := raw[p.GroupClaim].(type) {
		case []any:
			for _, g := range v {
				if s, ok := g.(string); ok {
					c.Groups = append(c.Groups, s)
				}
			}
		case string:
			c.Groups = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
		}
	}
	return c, nil
}
