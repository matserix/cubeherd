package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/config"
)

// ssoRuntime holds the identity provider client and the sign-in attempts that are under way.
type ssoRuntime struct {
	oidc *access.OIDC
	mu   sync.Mutex
	pend map[string]access.Pending
}

func newSSO() *ssoRuntime {
	return &ssoRuntime{oidc: access.NewOIDC(), pend: map[string]access.Pending{}}
}

func (s *ssoRuntime) put(state string, p access.Pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.pend {
		if time.Since(v.Created) > 10*time.Minute {
			delete(s.pend, k)
		}
	}
	s.pend[state] = p
}

func (s *ssoRuntime) take(state string) (access.Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pend[state]
	delete(s.pend, state)
	return p, ok && time.Since(p.Created) < 10*time.Minute
}

const (
	ssoCookie   = "cubeherd_sso"
	flashCookie = "cubeherd_flash"
)

// flash carries one message to the next page across a redirect; it is shown once.
func flash(w http.ResponseWriter, r *http.Request, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(msg)), Path: "/", MaxAge: 120, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: httpsOf(r)})
}

func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
	b, _ := base64.RawURLEncoding.DecodeString(c.Value)
	return string(b)
}

type ssoButton struct{ ID, Label string }

// ssoButtons are the providers offered on the login page, and whether password sign-in is off.
func (a *App) ssoButtons() ([]ssoButton, bool) {
	st := a.store()
	if st == nil {
		return nil, false
	}
	ps, _ := st.Providers()
	var out []ssoButton
	for _, p := range ps {
		out = append(out, ssoButton{p.ID, p.Label})
	}
	return out, len(out) > 0 && st.LocalSignInOff()
}

// publicBase is the address users reach the dashboard at: the configured one, else what this request used.
func (a *App) publicBase(r *http.Request) string {
	if u := strings.TrimRight(a.Cfg.Get().PublicURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if httpsOf(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (a *App) ssoRedirectURL(r *http.Request) string { return a.publicBase(r) + "/login/sso/callback" }

func (a *App) provider(id string) (access.Provider, bool) {
	st := a.store()
	if st == nil {
		return access.Provider{}, false
	}
	ps, _ := st.Providers()
	for _, p := range ps {
		if p.ID == id {
			return p, true
		}
	}
	return access.Provider{}, false
}

// ssoStart sends the browser to the identity provider. With test=1 an owner checks the provider without signing in.
func (a *App) ssoStart(w http.ResponseWriter, r *http.Request) {
	if a.store() == nil || a.sso == nil {
		http.NotFound(w, r)
		return
	}
	p, ok := a.provider(r.FormValue("p"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	test := r.FormValue("test") == "1"
	if test {
		if rr, authed := a.authenticate(r); !authed || !principalOf(rr).Global(access.SSOManage) {
			http.NotFound(w, r)
			return
		}
	}
	u, state, pend, err := a.sso.oidc.Begin(r.Context(), p, a.ssoRedirectURL(r), test)
	if err != nil {
		a.ssoFail(w, r, p, err, test)
		return
	}
	a.sso.put(state, pend)
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: state, Path: "/login/sso", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: httpsOf(r)})
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (a *App) ssoFail(w http.ResponseWriter, r *http.Request, p access.Provider, err error, test bool) {
	msg := a.t("Die Anmeldung über %s ist fehlgeschlagen: %s", p.Label, err.Error())
	flash(w, r, msg)
	a.recordAs(r, access.User{}, "", "login.fail", p.Label, access.Fail, "method", "sso", "reason", err.Error())
	if test {
		http.Redirect(w, r, "/access/sso", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ssoCallback finishes the sign-in: it checks the return, the ID token and the account rules, then starts a session.
func (a *App) ssoCallback(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	if st == nil || a.sso == nil {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/login/sso", MaxAge: -1})
	state := r.FormValue("state")
	c, err := r.Cookie(ssoCookie)
	pend, ok := a.sso.take(state)
	if err != nil || state == "" || c.Value != state || !ok {
		flash(w, r, a.t("Die Anmeldung ist abgelaufen oder gehört zu einem anderen Browser. Bitte versuche es noch einmal."))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p, ok := a.provider(pend.Provider)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if e := r.FormValue("error"); e != "" {
		a.ssoFail(w, r, p, errors.New(e+" "+r.FormValue("error_description")), pend.Test)
		return
	}
	claims, err := a.sso.oidc.Exchange(r.Context(), p, pend, r.FormValue("code"), a.ssoRedirectURL(r))
	if err != nil {
		a.ssoFail(w, r, p, err, pend.Test)
		return
	}
	if pend.Test {
		who := claims.Email
		if who == "" {
			who = claims.Subject
		}
		flash(w, r, a.t("Der Test mit %s ist gelungen: Der Anbieter hat %s bestätigt.", p.Label, who))
		a.record(r, "sso.test", p.Label, access.OK)
		http.Redirect(w, r, "/access/sso", http.StatusSeeOther)
		return
	}
	u, err := a.ssoUser(p, claims)
	if err != nil {
		a.ssoFail(w, r, p, err, false)
		return
	}
	if p.GroupClaim != "" {
		_ = st.SetMappedGroups(u.ID, claims.Groups)
	}
	if u.Status == access.StatusPending {
		a.ssoFail(w, r, p, errors.New(a.t("Dein Konto wartet auf die Freigabe durch einen Admin.")), false)
		return
	}
	if err := a.completeLogin(w, r, u, "sso", "provider", p.Label); err != nil {
		a.ssoFail(w, r, p, err, false)
	}
}

// ssoUser maps an identity onto an account: a linked identity, an existing account with the same verified
// e-mail (if the provider allows linking), or a new account (if it allows provisioning).
func (a *App) ssoUser(p access.Provider, c access.Claims) (access.User, error) {
	st := a.store()
	if p.HostedDomain != "" && !strings.EqualFold(c.HostedDomain, p.HostedDomain) {
		return access.User{}, errors.New(a.t("Das Konto gehört nicht zur erlaubten Domain."))
	}
	if c.Email != "" && !p.AllowsDomain(c.Email) || c.Email == "" && strings.TrimSpace(p.Domains) != "" {
		return access.User{}, errors.New(a.t("Die E-Mail-Adresse %s ist nicht erlaubt.", c.Email))
	}
	if u, err := st.UserByIdentity(c.Issuer, c.Subject); err == nil {
		if u.Disabled {
			return u, errors.New(a.t("Das Konto ist gesperrt."))
		}
		return u, nil
	}
	if p.LinkByEmail && c.EmailVerified {
		if u, err := st.UserByEmail(c.Email); err == nil && !u.Disabled {
			if ids, _ := st.Identities(u.ID); !hasIssuer(ids, c.Issuer) {
				if err := st.LinkIdentity(u.ID, c.Issuer, c.Subject, c.Email); err != nil {
					return u, err
				}
				return u, nil
			}
		}
	}
	if !p.AutoProvision {
		return access.User{}, errors.New(a.t("Für diese Anmeldung gibt es kein Konto. Bitte einen Owner, dein Konto anzulegen oder zu verknüpfen."))
	}
	name := firstNonEmptyStr(c.Username, strings.Split(c.Email, "@")[0], c.Name, "user")
	var id int64
	var err error
	for i := 0; i < 50; i++ {
		n := name
		if i > 0 {
			n = name + "-" + string(rune('a'+i%26)) + hexByte()
		}
		if id, err = st.CreateUser(n, c.Email, ""); !errors.Is(err, access.ErrExists) {
			break
		}
	}
	if err != nil {
		return access.User{}, err
	}
	if err := st.LinkIdentity(id, c.Issuer, c.Subject, c.Email); err != nil {
		return access.User{}, err
	}
	_ = st.SetEmailVerified(id, c.EmailVerified) // the provider vouches for the address only when it says so
	if p.DefaultRole != 0 {
		_, _ = st.AddGrant(access.Grant{PrincipalID: id, RoleID: p.DefaultRole, CreatedBy: "sso"})
	}
	_ = st.Record(access.Entry{Actor: "system", Action: "user.provisioned", Target: name, Detail: access.Detail("provider", p.Label, "email", c.Email)})
	return st.UserByID(id)
}

func hasIssuer(ids []access.Identity, issuer string) bool {
	for _, i := range ids {
		if i.Issuer == issuer {
			return true
		}
	}
	return false
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func hexByte() string {
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------- Settings page ----------------

type ssoRow struct {
	access.Provider
	HasSecret bool
}

type ssoData struct {
	Providers   []ssoRow
	PublicURL   string
	Callback    string
	Derived     bool // PublicURL is empty and the callback comes from this request
	NoLocal     bool
	CanNoLocal  bool
	Roles       []access.Role
	Message     string
	GoogleIssue string
}

func (a *App) ssoPage(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	d := &ssoData{PublicURL: a.Cfg.Get().PublicURL, Callback: a.ssoRedirectURL(r), NoLocal: st.LocalSignInOff(), CanNoLocal: st.HasSSOOwner(), GoogleIssue: access.GoogleIssuer}
	d.Derived = d.PublicURL == ""
	d.Roles, _ = st.ListRoles()
	ps, err := st.Providers()
	if err != nil {
		d.Message = err.Error()
	}
	for _, p := range ps {
		d.Providers = append(d.Providers, ssoRow{p, p.ClientSecret != ""})
	}
	d.Message = firstNonEmptyStr(takeFlash(w, r), d.Message)
	a.accessPage(w, r, "sso", accessData{SSO: d})
}

func validIssuer(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func newProviderID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ssoSave handles the three forms of the page: a provider, deleting one, and the global switches.
func (a *App) ssoSave(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	switch r.FormValue("op") {
	case "global":
		pub := strings.TrimRight(strings.TrimSpace(r.FormValue("publicUrl")), "/")
		if pub != "" && !validIssuer(pub) {
			a.fail(w, "Die öffentliche Adresse muss mit https:// beginnen (http:// nur für localhost).")
			return
		}
		off := checked(r, "noLocal")
		if off {
			if ps, _ := st.Providers(); len(ps) == 0 || !st.HasSSOOwner() {
				a.fail(w, "Die Anmeldung mit Passwort lässt sich erst ausschalten, wenn ein Owner über einen Anbieter angemeldet ist.")
				return
			}
		}
		if err := a.Cfg.Update(func(s *config.Settings) { s.PublicURL = pub }); err != nil {
			a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
			return
		}
		if err := st.SetLocalSignInOff(off); err != nil {
			a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
			return
		}
	case "delete":
		ps, _ := st.Providers()
		var keep []access.Provider
		for _, p := range ps {
			if p.ID != r.FormValue("id") {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			_ = st.SetLocalSignInOff(false)
		}
		if err := st.SaveProviders(keep); err != nil {
			a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
			return
		}
	case "provider":
		if !a.saveProvider(w, r) {
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	redirect(w, r, "/access/sso")
}

func (a *App) saveProvider(w http.ResponseWriter, r *http.Request) bool {
	st := a.store()
	ps, err := st.Providers()
	if err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return false
	}
	id := r.FormValue("id")
	p := access.Provider{ID: id, Kind: r.FormValue("kind")}
	idx := -1
	for i, x := range ps {
		if x.ID == id && id != "" {
			p, idx = x, i
		}
	}
	if idx < 0 {
		p.ID, p.Kind = newProviderID(), r.FormValue("kind")
		if p.Kind != "google" {
			p.Kind = "oidc"
		}
	}
	if p.Kind == "google" {
		p.Issuer = access.GoogleIssuer
		p.Label = "Google"
	} else {
		p.Issuer = strings.TrimSpace(r.FormValue("issuer"))
		p.Label = strings.TrimSpace(r.FormValue("label"))
		if p.Label == "" {
			p.Label = "SSO"
		}
	}
	p.ClientID = strings.TrimSpace(r.FormValue("clientId"))
	if s := strings.TrimSpace(r.FormValue("clientSecret")); s != "" {
		p.ClientSecret = s
	}
	p.Scopes = strings.TrimSpace(r.FormValue("scopes"))
	p.Domains = strings.TrimSpace(r.FormValue("domains"))
	p.HostedDomain = strings.TrimSpace(r.FormValue("hd"))
	p.GroupClaim = strings.TrimSpace(r.FormValue("groupClaim"))
	p.LinkByEmail, p.AutoProvision = checked(r, "linkByEmail"), checked(r, "autoProvision")
	p.DefaultRole = 0
	if rid := r.FormValue("defaultRole"); rid != "" && p.AutoProvision {
		var n int64
		for _, ro := range mustRoles(st) {
			if rid == itoa(ro.ID) {
				n = ro.ID
				// a provisioned account must not get more than the person saving this could give
				if !principalOf(r).CanGrant(ro, access.Scope{}) {
					a.fail(w, "Du kannst keine Rolle vergeben, deren Rechte du dort nicht selbst hast.")
					return false
				}
			}
		}
		p.DefaultRole = n
	}
	if p.ClientID == "" || p.ClientSecret == "" || !validIssuer(p.Issuer) {
		a.fail(w, "Aussteller (https://…), Client-ID und Client-Secret werden gebraucht.")
		return false
	}
	if err := a.sso.oidc.Check(r.Context(), p.Issuer); err != nil {
		a.fail(w, "Der Anbieter ist nicht erreichbar oder der Aussteller stimmt nicht: %s", err.Error())
		return false
	}
	if idx >= 0 {
		ps[idx] = p
	} else {
		ps = append(ps, p)
	}
	if err := st.SaveProviders(ps); err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return false
	}
	return true
}

func mustRoles(st *access.Store) []access.Role { r, _ := st.ListRoles(); return r }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
