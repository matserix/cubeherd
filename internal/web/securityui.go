package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/qr"
)

func (a *App) securityRoutes(rt *router) {
	rt.handle("GET /login/2fa", public(), a.p((*App).login2Page))
	rt.handle("POST /login/2fa", public(), a.p((*App).login2Post))
	rt.handle("GET /forgot", public(), a.p((*App).forgotPage))
	rt.handle("POST /forgot", public(), a.p((*App).forgotPost))
	rt.handle("GET /reset/{token}", public(), a.p((*App).resetPage))
	rt.handle("POST /reset/{token}", public(), a.p((*App).resetPost))
	rt.handle("GET /notme/{token}", public(), a.p((*App).notmePage))
	rt.handle("POST /notme/{token}", public(), a.p((*App).notmePost))
	rt.handle("GET /register", public(), a.p((*App).registerPage))
	rt.handle("POST /register", public(), a.p((*App).registerPost))
	rt.handle("GET /verify/{token}", public(), a.p((*App).verifyPage))
	rt.handle("POST /verify/{token}", public(), a.p((*App).verifyPost))
	rt.handle("GET /resend", public(), a.p((*App).resendPage))
	rt.handle("POST /resend", public(), a.p((*App).resendPost))

	rt.handle("GET /account/2fa/setup", open(), a.p((*App).twoFASetupPage))
	rt.handle("POST /account/2fa/setup", open(), a.p((*App).twoFASetupPost))
	rt.handle("POST /account/2fa/disable", open().log("2fa.disable"), a.p((*App).twoFADisable))
	rt.handle("POST /account/2fa/codes", open().log("2fa.codes"), a.p((*App).twoFACodes))

	users, approve := global(access.UsersManage), global(access.UsersApprove)
	rt.handle("POST /access/users/{id}/2fa/reset", users.log("2fa.reset", pathValue("id")), a.p((*App).userTwoFAReset))
	rt.handle("GET /access/security", users, a.p((*App).securityPage))
	rt.handle("POST /access/security", users.log("security.change"), a.p((*App).securitySave))
	rt.handle("GET /access/pending", approve, a.p((*App).pendingPage))
	rt.handle("POST /access/pending/{id}/approve", approve.log("user.approved", pathValue("id")), a.p((*App).pendingApprove))
	rt.handle("POST /access/pending/{id}/reject", approve.log("user.rejected", pathValue("id")), a.p((*App).pendingReject))
}

// ---------------- Account security settings ----------------

type securityData struct {
	S         access.Security
	MailReady bool
	PublicURL string
}

func (a *App) securityPage(w http.ResponseWriter, r *http.Request) {
	a.accessPage(w, r, "security", accessData{Sec: &securityData{S: a.store().Security(), MailReady: a.mailReady(), PublicURL: a.Cfg.Get().PublicURL}})
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func (a *App) securitySave(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	g, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("graceDays")))
	v := access.Security{Registration: r.FormValue("registration"), RequireVerified: checked(r, "requireVerified"), Domains: strings.TrimSpace(r.FormValue("domains")),
		Notice: r.FormValue("notice"), TwoFA: r.FormValue("twofa"), GraceDays: max(0, min(g, 90)), TrustSSO: checked(r, "trustSso")}
	if !oneOf(v.Registration, access.RegClosed, access.RegOpen, access.RegApproval) || !oneOf(v.Notice, "off", "new", "every") || !oneOf(v.TwoFA, "off", "privileged", "all") {
		a.fail(w, "Ungültige Auswahl.")
		return
	}
	if (v.RequireVerified || v.Notice != "off") && !a.linksReady() {
		a.fail(w, "Dafür sind zuerst die Mail-Einstellungen und die öffentliche Adresse nötig.")
		return
	}
	if err := st.SaveSecurity(v); err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	a.record(r, "security.change", "", access.OK, "registration", v.Registration, "verify", v.RequireVerified, "notice", v.Notice, "twofa", v.TwoFA)
	redirect(w, r, "/access/security")
}

// ---------------- Approving registrations ----------------

type pendingData struct{ Users []access.User }

func (a *App) pendingPage(w http.ResponseWriter, r *http.Request) {
	us, _ := a.store().PendingUsers()
	a.accessPage(w, r, "pending", accessData{Pending: &pendingData{Users: us}})
}

func (a *App) pendingUser(w http.ResponseWriter, r *http.Request) (access.User, bool) {
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return access.User{}, false
	}
	u, err := a.store().UserByID(id)
	if err != nil || u.Status != access.StatusPending {
		a.fail(w, "Dieses Konto wartet nicht auf eine Freigabe.")
		return access.User{}, false
	}
	return u, true
}

func (a *App) pendingApprove(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	u, ok := a.pendingUser(w, r)
	if !ok {
		return
	}
	if st.Security().RequireVerified && !u.EmailVerified {
		a.fail(w, "Die E-Mail-Adresse ist noch nicht bestätigt.")
		return
	}
	if err := st.SetStatus(u.ID, access.StatusActive); err != nil {
		a.storeErr(w, err)
		return
	}
	if u.Email != "" && u.EmailVerified && a.linksReady() {
		a.mailAsync(u.Email, a.t("Dein Cubeherd-Konto ist freigegeben"), a.t("Hallo %s,\n\ndein Konto wurde freigegeben. Du kannst dich jetzt anmelden:\n%s/login\n\nServer und Rechte bekommst du von einem Admin zugewiesen.", u.Name, a.mailBase(r)))
	}
	redirect(w, r, "/access/pending")
}

func (a *App) pendingReject(w http.ResponseWriter, r *http.Request) {
	u, ok := a.pendingUser(w, r)
	if !ok {
		return
	}
	if u.Email != "" && u.EmailVerified {
		a.mailAsync(u.Email, a.t("Deine Registrierung bei Cubeherd"), a.t("Hallo %s,\n\ndeine Registrierung wurde leider nicht angenommen.", u.Name))
	}
	if err := a.store().DeleteUser(u.ID); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/access/pending")
}

// ---------------- Two-factor setup in the account ----------------

func groupSecret(s string) string {
	var parts []string
	for len(s) > 4 {
		parts = append(parts, s[:4])
		s = s[4:]
	}
	return strings.Join(append(parts, s), " ")
}

func (a *App) twoFASetupView(w http.ResponseWriter, r *http.Request, tok, secret, errMsg string) {
	p := principalOf(r)
	acct := p.User.Email
	if acct == "" {
		acct = p.User.Name
	}
	svg, _ := qr.SVG(access.TOTPURI(a.brand().Name, acct, secret), 220)
	a.render(w, r, "twofa", Page{Title: a.t("Zwei-Faktor-Anmeldung"), Nav: "account", Data: twoFAData{Mode: "setup", Token: tok, Secret: groupSecret(secret), QR: htmlSafe(svg), Error: errMsg,
		Required: !a.twoFADeadlineIgnoringSetup(p, a.store().SessionMethod(tokenOf(r))).IsZero()}})
}

func (a *App) twoFASetupPage(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	if p.User.TOTP {
		redirect(w, r, "/account")
		return
	}
	secret := access.NewTOTPSecret()
	sealed, err := st.SealSecret(secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tok, err := st.NewToken(p.User.ID, access.TokenSetup2, 15*time.Minute, sealed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.twoFASetupView(w, r, tok, secret, "")
}

func (a *App) twoFASetupPost(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	tok := r.FormValue("token")
	t, err := st.PeekToken(tok, access.TokenSetup2)
	if err != nil || t.UserID != p.User.ID {
		redirect(w, r, "/account/2fa/setup")
		return
	}
	secret, err := st.OpenSecret(t.Data)
	if err != nil {
		redirect(w, r, "/account/2fa/setup")
		return
	}
	if d := a.lim.WaitKey("2fa-setup:" + p.User.Name); d > 0 {
		a.twoFASetupView(w, r, tok, secret, a.t("Zu viele Versuche. Bitte %d Sekunden warten.", int(d.Seconds())+1))
		return
	}
	codes, err := st.EnableTOTP(p.User.ID, secret, r.FormValue("code"), time.Now())
	if err != nil {
		a.lim.FailKey("2fa-setup:" + p.User.Name)
		a.twoFASetupView(w, r, tok, secret, a.t("Der Code stimmt nicht. Prüfe die Uhrzeit deines Geräts und versuche es mit dem nächsten Code."))
		return
	}
	_, _ = st.ConsumeToken(tok, access.TokenSetup2)
	a.record(r, "2fa.enabled", p.User.Name, access.OK)
	a.render(w, r, "twofa", Page{Title: a.t("Wiederherstellungscodes"), Nav: "account", Data: twoFAData{Mode: "codes", Codes: codes}})
}

type twoFAData struct {
	Mode     string // setup | codes
	Token    string
	Secret   string
	QR       htmlSafe
	Codes    []string
	Error    string
	Required bool
}

// twoFADeadlineIgnoringSetup tells whether the policy asks this user for a second factor at all.
func (a *App) twoFADeadlineIgnoringSetup(p *access.Principal, method string) time.Time {
	probe := *p
	probe.User.TOTP = false
	return a.twoFADeadline(&probe, method)
}

// checkPassword confirms the signed-in user's own password for a sensitive step.
func (a *App) checkPassword(r *http.Request, p *access.Principal) bool {
	_, hash, err := a.store().UserByName(p.User.Name)
	if err != nil {
		return false
	}
	if hash == "" { // an account that signs in only through a provider has no password to ask for
		return true
	}
	if a.lim.WaitKey("pw:"+strings.ToLower(p.User.Name)) > 0 {
		return false
	}
	if !checkPW(hash, r.FormValue("password")) {
		a.lim.FailKey("pw:" + strings.ToLower(p.User.Name))
		return false
	}
	return true
}

func (a *App) twoFADisable(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	if !p.User.TOTP {
		redirect(w, r, "/account")
		return
	}
	if !a.twoFADeadlineIgnoringSetup(p, st.SessionMethod(tokenOf(r))).IsZero() {
		a.fail(w, "Die Richtlinie verlangt die Zwei-Faktor-Anmeldung für dein Konto. Du kannst sie nicht ausschalten.")
		return
	}
	if !a.checkPassword(r, p) || !st.VerifyTOTP(p.User.ID, r.FormValue("code"), time.Now()) {
		a.fail(w, "Passwort oder Code ist falsch.")
		return
	}
	if err := st.DisableTOTP(p.User.ID); err != nil {
		a.storeErr(w, err)
		return
	}
	redirect(w, r, "/account")
}

func (a *App) twoFACodes(w http.ResponseWriter, r *http.Request) {
	st, p := a.store(), principalOf(r)
	if !p.User.TOTP {
		redirect(w, r, "/account")
		return
	}
	if !a.checkPassword(r, p) || !st.VerifyTOTP(p.User.ID, r.FormValue("code"), time.Now()) {
		a.fail(w, "Passwort oder Code ist falsch.")
		return
	}
	codes, err := st.NewRecoveryCodes(p.User.ID)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	a.render(w, r, "twofa", Page{Title: a.t("Wiederherstellungscodes"), Nav: "account", Data: twoFAData{Mode: "codes", Codes: codes}})
}

// userTwoFAReset lets an admin remove a user's second factor, for someone who lost their device and codes.
func (a *App) userTwoFAReset(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	id, ok := id64(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.coversUser(w, r, id) {
		return
	}
	u, err := st.UserByID(id)
	if err != nil {
		a.storeErr(w, err)
		return
	}
	if err := st.DisableTOTP(id); err != nil {
		a.storeErr(w, err)
		return
	}
	_, _ = st.EndUserSessions(id, "")
	if u.Email != "" && u.EmailVerified {
		a.mailAsync(u.Email, a.t("Die Zwei-Faktor-Anmeldung wurde zurückgesetzt"), a.t("Hallo %s,\n\nein Admin hat die Zwei-Faktor-Anmeldung deines Cubeherd-Kontos zurückgesetzt. Richte sie nach der nächsten Anmeldung neu ein. Warst du das nicht gewesen, melde dich bei einem Admin.", u.Name))
	}
	redirect(w, r, "/access/users/"+strconv.FormatInt(id, 10))
}
