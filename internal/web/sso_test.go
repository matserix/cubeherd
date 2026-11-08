package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/auth"
)

// idp is a minimal OpenID Connect provider: discovery, keys and a token endpoint that signs whatever claims the
// test puts in next.
type idp struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	claims map[string]any // overrides merged into the next ID token
	alg    string
	nonce  string // set from the authorization request by the test
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key, alg: "RS256", claims: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/auth", "token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "s3cret" || r.Form.Get("grant_type") != "authorization_code" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": p.token()})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) token() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := map[string]any{"iss": p.srv.URL, "aud": "cubeherd-client", "sub": "subject-1", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": p.nonce, "email": "nina@example.com", "email_verified": true, "preferred_username": "nina"}
	for k, v := range p.claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	signed := enc(map[string]string{"alg": p.alg, "typ": "JWT", "kid": "k1"}) + "." + enc(c)
	if p.alg == "none" {
		return signed + "."
	}
	h := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, h[:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// signIn runs the whole round trip through the dashboard and returns the callback's response.
func signIn(t *testing.T, d *dashboard, p *idp, provider string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/login/sso?p="+provider, nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	if rec.Code != 303 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("the authorization request lacks PKCE, state or nonce: %v", q)
	}
	if q.Get("redirect_uri") != "http://example.com/login/sso/callback" {
		t.Fatalf("redirect_uri = %s", q.Get("redirect_uri"))
	}
	p.mu.Lock()
	p.nonce = q.Get("nonce")
	p.mu.Unlock()
	cb := httptest.NewRequest("GET", "/login/sso/callback?code=abc&state="+url.QueryEscape(q.Get("state")), nil)
	cb.Host = "example.com"
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	d.h.ServeHTTP(out, cb)
	return out
}

func sessionOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.Cookie && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func flashOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			b, _ := base64.RawURLEncoding.DecodeString(c.Value)
			return string(b)
		}
	}
	return ""
}

func setupSSO(t *testing.T, mod func(*access.Provider)) (*dashboard, *access.Store, *idp) {
	t.Helper()
	d, st := newAccounts(t)
	p := newIdP(t)
	pr := access.Provider{ID: "idp1", Kind: "oidc", Label: "TestIdP", Issuer: p.srv.URL, ClientID: "cubeherd-client", ClientSecret: "s3cret"}
	if mod != nil {
		mod(&pr)
	}
	if err := st.SaveProviders([]access.Provider{pr}); err != nil {
		t.Fatal(err)
	}
	return d, st, p
}

func TestSSORefusesUnknownIdentitiesByDefault(t *testing.T) {
	d, st, p := setupSSO(t, nil)
	rec := signIn(t, d, p, "idp1")
	if sessionOf(rec) != "" || st.UserCount() != 1 {
		t.Fatal("an identity without an account must not get in or create one")
	}
	if !strings.Contains(flashOf(rec), "no account") && !strings.Contains(flashOf(rec), "kein Konto") {
		t.Fatalf("the user should be told why: %q", flashOf(rec))
	}
}

func TestSSOProvisionsAccountsWhenAllowed(t *testing.T) {
	d, st, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true; p.Domains = "example.com" })
	rec := signIn(t, d, p, "idp1")
	tok := sessionOf(rec)
	if tok == "" || rec.Header().Get("Location") != "/" {
		t.Fatalf("not signed in: %d %s", rec.Code, flashOf(rec))
	}
	u, err := st.SessionUser(tok)
	if err != nil || u.Name != "nina" || u.Email != "nina@example.com" || u.HasPassword {
		t.Fatalf("provisioned user: %+v %v", u, err)
	}
	if pr := principal(t, st, u); pr.Anywhere(access.ServerView) {
		t.Fatal("a provisioned account starts without rights")
	}
	// the next sign-in finds the linked identity instead of creating another account
	rec = signIn(t, d, p, "idp1")
	if sessionOf(rec) == "" || st.UserCount() != 2 {
		t.Fatalf("second sign-in: users=%d", st.UserCount())
	}
	// a locked account stays locked
	_ = st.UpdateUser(u.ID, u.Name, u.Email, true)
	if sessionOf(signIn(t, d, p, "idp1")) != "" {
		t.Fatal("a disabled account signed in through SSO")
	}
}

func TestSSOLinksByVerifiedEmailOnly(t *testing.T) {
	d, st, p := setupSSO(t, func(p *access.Provider) { p.LinkByEmail = true })
	id, _ := st.CreateUser("nina-local", "nina@example.com", "")
	p.claims["email_verified"] = false
	if sessionOf(signIn(t, d, p, "idp1")) != "" {
		t.Fatal("an unverified e-mail address must not link to an account")
	}
	p.claims["email_verified"] = true
	rec := signIn(t, d, p, "idp1")
	u, err := st.SessionUser(sessionOf(rec))
	if err != nil || u.ID != id {
		t.Fatalf("link by verified e-mail: %v", err)
	}
	if ids, _ := st.Identities(id); len(ids) != 1 {
		t.Fatal("the identity was not stored")
	}
}

func TestSSORejectsBadTokens(t *testing.T) {
	for name, claims := range map[string]map[string]any{
		"wrong audience": {"aud": "someone-else"},
		"expired":        {"exp": time.Now().Add(-time.Hour).Unix()},
		"wrong issuer":   {"iss": "https://evil.example.com"},
		"wrong nonce":    {"nonce": "replayed"},
		"no subject":     {"sub": nil},
	} {
		t.Run(name, func(t *testing.T) {
			d, st, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true })
			for k, v := range claims {
				p.claims[k] = v
			}
			rec := signIn(t, d, p, "idp1")
			if sessionOf(rec) != "" || st.UserCount() != 1 {
				t.Fatalf("a token with %s was accepted", name)
			}
		})
	}
	t.Run("unsigned", func(t *testing.T) {
		d, st, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true })
		p.alg = "none"
		if sessionOf(signIn(t, d, p, "idp1")) != "" || st.UserCount() != 1 {
			t.Fatal("an unsigned token was accepted")
		}
	})
	t.Run("signed by another key", func(t *testing.T) {
		d, st, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true })
		p.key, _ = rsa.GenerateKey(rand.Reader, 2048)
		if sessionOf(signIn(t, d, p, "idp1")) != "" || st.UserCount() != 1 {
			t.Fatal("a token signed by an unknown key was accepted")
		}
	})
}

func TestSSODomainRules(t *testing.T) {
	d, st, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true; p.Domains = "corp.example" })
	if sessionOf(signIn(t, d, p, "idp1")) != "" || st.UserCount() != 1 {
		t.Fatal("an e-mail outside the allowed domains signed in")
	}
	d, st, p = setupSSO(t, func(p *access.Provider) { p.Kind = "google"; p.AutoProvision = true; p.HostedDomain = "example.com" })
	if sessionOf(signIn(t, d, p, "idp1")) != "" || st.UserCount() != 1 {
		t.Fatal("a Google account without the hosted domain signed in")
	}
	p.claims["hd"] = "example.com"
	if sessionOf(signIn(t, d, p, "idp1")) == "" {
		t.Fatal("the right hosted domain was refused")
	}
}

func TestSSOMapsGroupsFromTheClaim(t *testing.T) {
	d, st, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true; p.GroupClaim = "groups" })
	gid, _ := st.CreateGroup("Mods", "mc-mods", "")
	_, _ = st.CreateGroup("Other", "other", "")
	p.claims["groups"] = []string{"mc-mods", "unrelated"}
	rec := signIn(t, d, p, "idp1")
	u, _ := st.SessionUser(sessionOf(rec))
	gs, _ := st.UserGroups(u.ID)
	if len(gs) != 1 || gs[0].ID != gid {
		t.Fatalf("groups = %+v", gs)
	}
	p.claims["groups"] = []string{}
	signIn(t, d, p, "idp1")
	if gs, _ := st.UserGroups(u.ID); len(gs) != 0 {
		t.Fatal("membership should follow the provider and be removed when the claim no longer has it")
	}
}

// TestSSOGroupMappingIsScopedToItsProvider covers two providers that happen to send the same group claim
// value: a group mapped from one of them must not pick up a sign-in through the other.
func TestSSOGroupMappingIsScopedToItsProvider(t *testing.T) {
	d, st, p1 := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true; p.GroupClaim = "groups" })
	p2 := newIdP(t)
	pr2 := access.Provider{ID: "idp2", Kind: "oidc", Label: "Second", Issuer: p2.srv.URL, ClientID: "cubeherd-client", ClientSecret: "s3cret",
		AutoProvision: true, GroupClaim: "groups"}
	ps, _ := st.Providers()
	if err := st.SaveProviders(append(ps, pr2)); err != nil {
		t.Fatal(err)
	}
	scoped, _ := st.CreateGroup("Mods1", "mc-mods", "idp1")
	p1.claims["groups"] = []string{"mc-mods"}
	p2.claims["groups"] = []string{"mc-mods"}
	p2.claims["email"], p2.claims["preferred_username"] = "other@example.com", "other"

	u1 := signIn(t, d, p1, "idp1")
	user1, _ := st.SessionUser(sessionOf(u1))
	if gs, _ := st.UserGroups(user1.ID); len(gs) != 1 || gs[0].ID != scoped {
		t.Fatalf("the matching provider should still map the group: %+v", gs)
	}

	u2 := signIn(t, d, p2, "idp2")
	user2, _ := st.SessionUser(sessionOf(u2))
	if user2.ID == user1.ID {
		t.Fatal("the second sign-in should be a different account")
	}
	if gs, _ := st.UserGroups(user2.ID); len(gs) != 0 {
		t.Fatalf("a group scoped to idp1 must not map a sign-in through idp2: %+v", gs)
	}
}

func TestSSOStateMustMatchTheBrowser(t *testing.T) {
	d, _, p := setupSSO(t, func(p *access.Provider) { p.AutoProvision = true })
	req := httptest.NewRequest("GET", "/login/sso?p=idp1", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	p.nonce = loc.Query().Get("nonce")
	// the callback arrives without the cookie that belongs to the attempt (another browser)
	cb := httptest.NewRequest("GET", "/login/sso/callback?code=abc&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	cb.Host = "example.com"
	out := httptest.NewRecorder()
	d.h.ServeHTTP(out, cb)
	if sessionOf(out) != "" {
		t.Fatal("a callback without the matching cookie signed in")
	}
	cb = httptest.NewRequest("GET", "/login/sso/callback?code=abc&state=forged", nil)
	out = httptest.NewRecorder()
	d.h.ServeHTTP(out, cb)
	if sessionOf(out) != "" {
		t.Fatal("a forged state signed in")
	}
}

func TestPasswordSignInCanBeSwitchedOffOnlyWithAnSSOOwner(t *testing.T) {
	d, st, p := setupSSO(t, func(p *access.Provider) { p.LinkByEmail = true })
	owner := d.cookieFor(t, st, "admin")
	off := url.Values{"op": {"global"}, "noLocal": {"on"}}
	if rec := d.as(owner, "POST", "/access/sso", off, true); !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") || st.LocalSignInOff() {
		t.Fatal("password sign-in was switched off although no owner can use SSO")
	}
	adm, _, _ := st.UserByName("admin")
	_ = st.UpdateUser(adm.ID, adm.Name, "nina@example.com", false)
	if sessionOf(signIn(t, d, p, "idp1")) == "" {
		t.Fatal("owner could not link by e-mail")
	}
	if rec := d.as(owner, "POST", "/access/sso", off, true); strings.Contains(rec.Header().Get("HX-Trigger"), "crit") || !st.LocalSignInOff() {
		t.Fatalf("switching off should work with a linked owner: %v", rec.Header())
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"user": {"admin"}, "password": {"correct horse battery"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	if sessionOf(rec) != "" {
		t.Fatal("password sign-in still works after it was switched off")
	}
}

func TestClientSecretIsEncryptedAtRest(t *testing.T) {
	_, st, _ := setupSSO(t, nil)
	if raw := st.KV("sso.providers"); strings.Contains(raw, "s3cret") || !strings.Contains(raw, `"secret":"`) {
		t.Fatalf("the secret is stored in plain text: %s", raw)
	}
	ps, err := st.Providers()
	if err != nil || len(ps) != 1 || ps[0].ClientSecret != "s3cret" {
		t.Fatalf("round trip: %v %+v", err, ps)
	}
}
