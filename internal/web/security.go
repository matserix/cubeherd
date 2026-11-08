package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/auth"
	"cubeherd/internal/mailer"
)

// ---------------- Rate windows ----------------

// rateWindow counts events per key over a sliding period; it limits mail-sending and registration, where a
// failed-attempt counter does not fit.
type rateWindow struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func newRateWindow() *rateWindow { return &rateWindow{m: map[string][]time.Time{}} }

// Allow records one event and reports whether the key is still within n events per period.
func (rw *rateWindow) Allow(key string, n int, per time.Duration) bool {
	if rw == nil {
		return true
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()
	now := time.Now()
	cut := now.Add(-per)
	var keep []time.Time
	for _, t := range rw.m[key] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= n {
		rw.m[key] = keep
		return false
	}
	rw.m[key] = append(keep, now)
	if len(rw.m) > 5000 { // forget stale keys
		for k, v := range rw.m {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(rw.m, k)
			}
		}
	}
	return true
}

// ---------------- Cookies for the second step and for known devices ----------------

const (
	twoFACookie  = "cubeherd_2fa"
	deviceCookie = "cubeherd_device"
)

func (a *App) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: httpsOf(r)})
}

// ---------------- Mail ----------------

// mailBase is the address used in links inside e-mails. The configured public address is preferred: without it
// the link follows the Host header of the request, which a caller can choose.
func (a *App) mailBase(r *http.Request) string { return a.publicBase(r) }

// mailAsync sends a mail without making the request wait, and logs a failure. Callers that must not reveal whether
// an address is known use it so the response time does not depend on the outcome.
func (a *App) mailAsync(to, subject, body string) {
	subject, body = a.brandText(subject), a.brandText(body)
	cfg, ok := a.mailConfig()
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := mailer.Send(ctx, cfg, to, subject, body); err != nil {
			log.Printf("mail to %s: %v", maskAddr(to), err)
		}
	}()
}

func maskAddr(a string) string {
	if i := strings.Index(a, "@"); i > 1 {
		return a[:1] + "…" + a[i:]
	}
	return "…"
}

func (a *App) mailReady() bool { _, ok := a.mailConfig(); return ok }

// ---------------- Policy ----------------

var userNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)

// privileged reports whether the user may do something that changes who may do what: the accounts whose second
// factor matters most.
func privileged(p *access.Principal) bool {
	if p == nil {
		return false
	}
	for _, perm := range []access.Perm{access.UsersManage, access.RolesManage, access.SSOManage, access.NodesManage, access.SettingsManage} {
		if p.Global(perm) {
			return true
		}
	}
	return false
}

// twoFADeadline is when the user must have set up a second factor, if the policy asks for one; zero means never.
func (a *App) twoFADeadline(p *access.Principal, method string) time.Time {
	st := a.store()
	if st == nil || p == nil || p.User.TOTP {
		return time.Time{}
	}
	sec := st.Security()
	if sec.TwoFA == "off" || (sec.TwoFA == "privileged" && !privileged(p)) || (method == "sso" && sec.TrustSSO) {
		return time.Time{}
	}
	from := p.User.Created
	if t := time.Unix(sec.TwoFASince, 0); sec.TwoFASince > 0 && t.After(from) {
		from = t
	}
	return from.Add(time.Duration(sec.GraceDays) * 24 * time.Hour)
}

// twoFAGate returns the address to send the user to when their grace period has ended and they have no second factor.
func (a *App) twoFAGate(r *http.Request) string {
	p := principalOf(r)
	if p == nil {
		return ""
	}
	dl := a.twoFADeadline(p, a.store().SessionMethod(tokenOf(r)))
	if dl.IsZero() || time.Now().Before(dl) {
		return ""
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/account/2fa") || path == "/logout" || path == "/account" || strings.HasPrefix(path, "/static/") {
		return ""
	}
	return "/account/2fa/setup"
}

// ---------------- Sign-in steps after the first factor ----------------

// afterFirstFactor decides what happens once a password was accepted: refusals for accounts that may not sign in
// yet, the second factor, or the session itself.
func (a *App) afterFirstFactor(w http.ResponseWriter, r *http.Request, u access.User, fail func(msg, hint string)) {
	st := a.store()
	sec := st.Security()
	switch {
	case u.Status == access.StatusPending:
		a.recordAs(r, u, "", "login.fail", u.Name, access.Fail, "reason", "awaiting approval")
		fail(a.t("Dein Konto wartet auf die Freigabe durch einen Admin."), "")
		return
	case u.Email != "" && !u.EmailVerified && sec.RequireVerified:
		a.recordAs(r, u, "", "login.fail", u.Name, access.Fail, "reason", "e-mail not confirmed")
		fail(a.t("Bitte bestätige zuerst deine E-Mail-Adresse. Den Link haben wir dir geschickt."), "resend")
		return
	case u.ForceReset:
		a.recordAs(r, u, "", "login.fail", u.Name, access.Fail, "reason", "password reset required")
		fail(a.t("Dein Passwort muss zurückgesetzt werden, bevor du dich anmelden kannst."), "forgot")
		return
	}
	if u.TOTP {
		tok, err := st.NewToken(u.ID, access.TokenLogin2, 5*time.Minute, "")
		if err != nil {
			fail(err.Error(), "")
			return
		}
		a.setCookie(w, r, twoFACookie, tok, 300)
		http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
		return
	}
	if err := a.completeLogin(w, r, u, "password"); err != nil {
		fail(err.Error(), "")
	}
}

// completeLogin starts the session, notes the sign-in and may send the notice.
func (a *App) completeLogin(w http.ResponseWriter, r *http.Request, u access.User, method string, detail ...any) error {
	first := u.LastLogin.IsZero()
	if err := a.startSessionVia(w, r, u, method); err != nil {
		return err
	}
	a.recordAs(r, u, "", "login.ok", u.Name, access.OK, append([]any{"method", method}, detail...)...)
	a.signInNotice(w, r, u, method, first)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

func (a *App) startSessionVia(w http.ResponseWriter, r *http.Request, u access.User, method string) error {
	st := a.store()
	tok, err := st.NewSessionVia(u.ID, clientAddr(r), r.UserAgent(), a.sessionTTL(), method)
	if err != nil {
		return err
	}
	a.sess.Set(w, tok, a.secure(r))
	st.TouchLogin(u.ID)
	return nil
}

// signInNotice mails the user about a sign-in, as the admin's policy says: never, from a new device only, or always.
// A first sign-in is not worth a mail.
func (a *App) signInNotice(w http.ResponseWriter, r *http.Request, u access.User, method string, first bool) {
	st := a.store()
	mode := st.Security().Notice
	if mode == "off" || u.Email == "" || !u.EmailVerified || !a.mailReady() {
		return
	}
	device := ""
	if c, err := r.Cookie(deviceCookie); err == nil {
		device = c.Value
	}
	if device == "" {
		device = randomToken()
		a.setCookie(w, r, deviceCookie, device, 2*365*24*3600)
	}
	known := st.SeenDevice(u.ID, device, clientAddr(r), r.UserAgent())
	if first || (mode == "new" && known) {
		return
	}
	notme, err := st.NewToken(u.ID, access.TokenNotMe, 7*24*time.Hour, "")
	if err != nil {
		return
	}
	when := time.Now().In(displayLoc()).Format("02.01.2006 15:04 MST")
	body := a.t("Hallo %s,\n\nin dein Cubeherd-Konto hat sich jemand angemeldet.\n\nZeit: %s\nAdresse: %s\nGerät: %s\nAnmeldung über: %s\n\nWar das nicht du? Öffne diesen Link. Er meldet das Konto überall ab und verlangt ein neues Passwort:\n%s/notme/%s\n\nWar das du, musst du nichts tun.",
		u.Name, when, clientAddr(r), shortUA(r.UserAgent()), a.t(methodLabel(method)), a.mailBase(r), notme)
	a.mailAsync(u.Email, a.t("Neue Anmeldung bei Cubeherd"), body)
}

func methodLabel(m string) string {
	if m == "sso" {
		return "Einmalanmeldung"
	}
	return "Passwort"
}

func shortUA(ua string) string {
	if len(ua) > 120 {
		ua = ua[:120] + "…"
	}
	if ua == "" {
		return "–"
	}
	return ua
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------------- Second step of the sign-in ----------------

type authData struct {
	Kind    string // which page of authpage.html
	Error   string
	Name    string
	Email   string
	Token   string
	Info    string
	Secret  string        // grouped, for typing into an app
	QR      template.HTML // the same secret as a QR code
	Codes   []string      // recovery codes, shown once
	Hint    string
	Domains string
	Mail    bool
}

func (a *App) authPage(w http.ResponseWriter, r *http.Request, d authData, title string) {
	a.render(w, r, "authpage", Page{Title: title, Nav: "login", Data: d})
}

func (a *App) login2Page(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	c, err := r.Cookie(twoFACookie)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if _, err := st.PeekToken(c.Value, access.TokenLogin2); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.authPage(w, r, authData{Kind: "twofa"}, a.t("Zwei-Faktor-Anmeldung"))
}

func (a *App) login2Post(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	c, err := r.Cookie(twoFACookie)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	t, err := st.PeekToken(c.Value, access.TokenLogin2)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	u, err := st.UserByID(t.UserID)
	if err != nil || u.Disabled {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	show := func(msg string) { a.authPage(w, r, authData{Kind: "twofa", Error: msg}, a.t("Zwei-Faktor-Anmeldung")) }
	key := "2fa:" + strings.ToLower(u.Name)
	if d := a.lim.WaitKey(key); d > 0 {
		show(a.t("Zu viele Versuche. Bitte %d Sekunden warten.", int(d.Seconds())+1))
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	ok, recovery := false, false
	switch {
	case st.VerifyTOTP(u.ID, code, time.Now()):
		ok = true
	case len(strings.ReplaceAll(strings.ReplaceAll(code, "-", ""), " ", "")) >= 10 && st.UseRecoveryCode(u.ID, code):
		ok, recovery = true, true
	}
	if !ok {
		a.lim.FailKey(key)
		a.lim.Fail(r)
		time.Sleep(400 * time.Millisecond)
		a.recordAs(r, u, "", "login.fail", u.Name, access.Fail, "reason", "wrong second factor")
		show(a.t("Der Code ist falsch oder wurde schon benutzt."))
		return
	}
	if _, err := st.ConsumeToken(c.Value, access.TokenLogin2); err != nil { // another request used it first
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.lim.ResetKey(key)
	a.setCookie(w, r, twoFACookie, "", -1)
	if recovery {
		a.recordAs(r, u, "", "2fa.recovery-used", u.Name, access.OK, "left", st.RecoveryCodesLeft(u.ID))
	}
	if err := a.completeLogin(w, r, u, "password", "second factor", map[bool]string{true: "recovery code", false: "app code"}[recovery]); err != nil {
		show(err.Error())
	}
}

// ---------------- Password reset ----------------

func (a *App) forgotPage(w http.ResponseWriter, r *http.Request) {
	if !a.mailReady() {
		a.authPage(w, r, authData{Kind: "noreset"}, a.t("Passwort zurücksetzen"))
		return
	}
	a.authPage(w, r, authData{Kind: "forgot"}, a.t("Passwort zurücksetzen"))
}

func (a *App) forgotPost(w http.ResponseWriter, r *http.Request) {
	if !a.mailReady() {
		a.authPage(w, r, authData{Kind: "noreset"}, a.t("Passwort zurücksetzen"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if a.rw.Allow("forgot-ip:"+clientAddr(r), 6, time.Hour) && a.rw.Allow("forgot-mail:"+email, 3, time.Hour) && email != "" {
		base := a.mailBase(r)
		go a.sendReset(base, email, clientAddr(r))
	}
	// the same answer whether or not the address belongs to an account
	a.authPage(w, r, authData{Kind: "forgotsent"}, a.t("Passwort zurücksetzen"))
}

// sendReset looks the address up and mails a link; it runs after the response so the answer's timing says nothing.
func (a *App) sendReset(base, email, ip string) {
	st := a.store()
	if st == nil {
		return
	}
	u, err := st.UserByEmail(email)
	if err != nil || u.Disabled || u.Status != access.StatusActive || (!u.EmailVerified && st.Security().RequireVerified) {
		return
	}
	tok, err := st.NewToken(u.ID, access.TokenReset, time.Hour, "")
	if err != nil {
		return
	}
	body := a.t("Hallo %s,\n\ndu hast ein neues Passwort für Cubeherd angefordert (von der Adresse %s). Öffne diesen Link, um es festzulegen. Er gilt eine Stunde und nur einmal:\n%s/reset/%s\n\nHast du das nicht angefordert, ignoriere diese Nachricht. Dein Passwort bleibt dann unverändert.",
		u.Name, ip, base, tok)
	a.mailAsyncSync(u.Email, a.t("Passwort für Cubeherd zurücksetzen"), body)
}

// mailAsyncSync sends from the current goroutine (callers already run in the background).
func (a *App) mailAsyncSync(to, subject, body string) {
	subject, body = a.brandText(subject), a.brandText(body)
	cfg, ok := a.mailConfig()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := mailer.Send(ctx, cfg, to, subject, body); err != nil {
		log.Printf("mail to %s: %v", maskAddr(to), err)
	}
}

func (a *App) resetPage(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if _, err := a.store().PeekToken(tok, access.TokenReset); err != nil {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	a.authPage(w, r, authData{Kind: "reset", Token: tok}, a.t("Neues Passwort"))
}

func (a *App) resetPost(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	tok := r.PathValue("token")
	if _, err := st.PeekToken(tok, access.TokenReset); err != nil {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 10 || pw != r.FormValue("password2") {
		a.authPage(w, r, authData{Kind: "reset", Token: tok, Error: a.t("Neues Passwort: mindestens 10 Zeichen und beide Felder gleich.")}, a.t("Neues Passwort"))
		return
	}
	hash, err := authHash(pw)
	if err != nil {
		a.authPage(w, r, authData{Kind: "reset", Token: tok, Error: err.Error()}, a.t("Neues Passwort"))
		return
	}
	t, err := st.ConsumeToken(tok, access.TokenReset)
	if err != nil {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	u, err := st.UserByID(t.UserID)
	if err != nil || u.Disabled || u.Status != access.StatusActive {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	if err := st.SetPassword(u.ID, hash, ""); err != nil { // also ends every session
		a.authPage(w, r, authData{Kind: "reset", Token: tok, Error: err.Error()}, a.t("Neues Passwort"))
		return
	}
	_ = st.SetEmailVerified(u.ID, true) // the link arrived by e-mail, so the address is the user's
	a.recordAs(r, u, "", "user.password-reset", u.Name, access.OK)
	if u.Email != "" {
		a.mailAsync(u.Email, a.t("Dein Cubeherd-Passwort wurde geändert"), a.t("Hallo %s,\n\ndein Passwort für Cubeherd wurde gerade geändert, alle Sitzungen wurden beendet. Warst du das nicht, wende dich sofort an einen Admin.", u.Name))
	}
	flash(w, r, a.t("Dein Passwort wurde geändert. Du kannst dich jetzt anmelden."))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------- "This was not me" ----------------

func (a *App) notmePage(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if _, err := a.store().PeekToken(tok, access.TokenNotMe); err != nil {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	a.authPage(w, r, authData{Kind: "notme", Token: tok}, a.t("Das war ich nicht"))
}

func (a *App) notmePost(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	t, err := st.ConsumeToken(r.PathValue("token"), access.TokenNotMe)
	if err != nil {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	u, err := st.UserByID(t.UserID)
	if err == nil {
		_, _ = st.EndUserSessions(u.ID, "")
		_ = st.SetForceReset(u.ID, true)
		a.recordAs(r, u, "", "user.not-me", u.Name, access.OK)
	}
	a.authPage(w, r, authData{Kind: "notmedone", Mail: a.mailReady()}, a.t("Das Konto ist gesichert"))
}

// ---------------- Registration ----------------

func (a *App) registrationOpen() bool {
	st := a.store()
	return st != nil && st.Security().Registration != access.RegClosed
}

func (a *App) registerPage(w http.ResponseWriter, r *http.Request) {
	if !a.registrationOpen() {
		http.NotFound(w, r)
		return
	}
	a.authPage(w, r, authData{Kind: "register", Domains: strings.TrimSpace(a.store().Security().Domains)}, a.t("Registrieren"))
}

func allowedDomain(domains, email string) bool {
	if strings.TrimSpace(domains) == "" {
		return true
	}
	p := access.Provider{Domains: domains}
	return p.AllowsDomain(email)
}

func (a *App) registerPost(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	if !a.registrationOpen() {
		http.NotFound(w, r)
		return
	}
	sec := st.Security()
	name, email := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("email"))
	show := func(msg string) {
		a.authPage(w, r, authData{Kind: "register", Error: msg, Name: name, Email: email, Domains: sec.Domains}, a.t("Registrieren"))
	}
	if r.FormValue("website") != "" { // the field is hidden from people; a bot fills it
		a.authPage(w, r, authData{Kind: "registered", Info: "wait"}, a.t("Registrieren"))
		return
	}
	if !a.rw.Allow("register-ip:"+clientAddr(r), 5, time.Hour) {
		show(a.t("Zu viele Registrierungen von dieser Adresse. Bitte versuche es später noch einmal."))
		return
	}
	if sec.RequireVerified && !a.mailReady() {
		show(a.t("Die Registrierung ist im Moment nicht möglich."))
		return
	}
	addr, err := mailer.ParseAddress(email)
	switch {
	case !userNameRe.MatchString(name):
		show(a.t("Der Name braucht 3 bis 32 Zeichen: Buchstaben, Ziffern, Punkt, Binde- oder Unterstrich."))
		return
	case err != nil || addr.Name != "" || addr.Address != email:
		show(a.t("Bitte gib eine gültige E-Mail-Adresse an."))
		return
	case !allowedDomain(sec.Domains, email):
		show(a.t("Mit dieser E-Mail-Domain ist keine Registrierung möglich."))
		return
	case len(r.FormValue("password")) < 10 || r.FormValue("password") != r.FormValue("password2"):
		show(a.t("Passwort: mindestens 10 Zeichen und beide Felder gleich."))
		return
	}
	state := "login" // what the person sees afterwards
	switch {
	case sec.RequireVerified && sec.Registration == access.RegApproval:
		state = "verify-approval"
	case sec.RequireVerified:
		state = "verify"
	case sec.Registration == access.RegApproval:
		state = "approval"
	}
	if existing, err := st.UserByEmail(email); err == nil {
		// no new account, and no hint to the person at the keyboard that the address is taken
		if a.mailReady() {
			a.mailAsync(existing.Email, a.t("Du hast schon ein Cubeherd-Konto"), a.t("Hallo %s,\n\njemand hat sich mit deiner E-Mail-Adresse bei Cubeherd registriert. Du hast aber schon ein Konto (Name: %s). Hast du dein Passwort vergessen, kannst du es hier zurücksetzen:\n%s/forgot", existing.Name, existing.Name, a.mailBase(r)))
		}
		a.authPage(w, r, authData{Kind: "registered", Info: state}, a.t("Registrieren"))
		return
	}
	hash, err := authHash(r.FormValue("password"))
	if err != nil {
		show(err.Error())
		return
	}
	status := access.StatusActive
	if sec.Registration == access.RegApproval {
		status = access.StatusPending
	}
	id, err := st.CreateUserWith(access.NewUser{Name: name, Email: email, PasswordHash: hash, Unverified: sec.RequireVerified, Status: status})
	if err != nil {
		show(a.t("Diesen Namen gibt es schon."))
		return
	}
	u, _ := st.UserByID(id)
	a.recordAs(r, u, "", "user.registered", name, access.OK, "status", status, "email", maskAddr(email))
	if sec.RequireVerified {
		if tok, err := st.NewToken(id, access.TokenVerify, 72*time.Hour, ""); err == nil {
			a.mailAsync(email, a.t("Bitte bestätige deine E-Mail-Adresse"), a.t("Hallo %s,\n\nschön, dass du dabei bist. Bestätige deine E-Mail-Adresse mit diesem Link (er gilt drei Tage):\n%s/verify/%s", name, a.mailBase(r), tok))
		}
	}
	a.authPage(w, r, authData{Kind: "registered", Info: state}, a.t("Registrieren"))
}

func (a *App) verifyPage(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if _, err := a.store().PeekToken(tok, access.TokenVerify); err != nil {
		a.authPage(w, r, authData{Kind: "linkbad", Hint: "resend"}, a.t("Link ungültig"))
		return
	}
	a.authPage(w, r, authData{Kind: "verify", Token: tok}, a.t("E-Mail bestätigen"))
}

func (a *App) verifyPost(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	t, err := st.ConsumeToken(r.PathValue("token"), access.TokenVerify)
	if err != nil {
		a.authPage(w, r, authData{Kind: "linkbad", Hint: "resend"}, a.t("Link ungültig"))
		return
	}
	u, err := st.UserByID(t.UserID)
	if err != nil {
		a.authPage(w, r, authData{Kind: "linkbad"}, a.t("Link ungültig"))
		return
	}
	_ = st.SetEmailVerified(u.ID, true)
	a.recordAs(r, u, "", "user.verified", u.Name, access.OK)
	info := "login"
	if u.Status == access.StatusPending {
		info = "approval"
	}
	a.authPage(w, r, authData{Kind: "verified", Info: info}, a.t("E-Mail bestätigt"))
}

func (a *App) resendPage(w http.ResponseWriter, r *http.Request) {
	a.authPage(w, r, authData{Kind: "resend"}, a.t("Bestätigungs-Mail erneut senden"))
}

func (a *App) resendPost(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if a.mailReady() && email != "" && a.rw.Allow("resend-ip:"+clientAddr(r), 6, time.Hour) && a.rw.Allow("resend-mail:"+email, 3, time.Hour) {
		base := a.mailBase(r)
		go func() {
			st := a.store()
			if st == nil {
				return
			}
			u, err := st.UserByEmail(email)
			if err != nil || u.EmailVerified || u.Disabled {
				return
			}
			if tok, err := st.NewToken(u.ID, access.TokenVerify, 72*time.Hour, ""); err == nil {
				a.mailAsyncSync(u.Email, a.t("Bitte bestätige deine E-Mail-Adresse"), a.t("Hallo %s,\n\nschön, dass du dabei bist. Bestätige deine E-Mail-Adresse mit diesem Link (er gilt drei Tage):\n%s/verify/%s", u.Name, base, tok))
			}
		}()
	}
	a.authPage(w, r, authData{Kind: "resent"}, a.t("Bestätigungs-Mail erneut senden"))
}

func authHash(pw string) (string, error) { return auth.HashPassword(pw) }

// htmlSafe is markup that was built here and may be placed in a page as it is.
type htmlSafe = template.HTML

func checkPW(hash, pw string) bool { return auth.CheckPassword(hash, pw) }
