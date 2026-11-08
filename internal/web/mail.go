package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"cubeherd/internal/access"
	"cubeherd/internal/mailer"
)

// mailConfig is the SMTP setting; ok is false while none is configured.
func (a *App) mailConfig() (mailer.Config, bool) {
	st := a.store()
	if st == nil {
		return mailer.Config{}, false
	}
	c, err := st.MailConfig()
	return c, err == nil && c.Configured()
}

// sendMail sends one mail with the stored settings.
func (a *App) sendMail(ctx context.Context, to, subject, body string) error {
	c, ok := a.mailConfig()
	if !ok {
		return mailer.Config{}.Validate()
	}
	return mailer.Send(ctx, c, to, subject, body)
}

type mailData struct {
	Cfg        mailer.Config
	HasPass    bool
	TestTo     string
	Configured bool
}

func (a *App) mailPage(w http.ResponseWriter, r *http.Request) {
	c, _ := a.store().MailConfig()
	d := &mailData{Cfg: c, HasPass: c.Password != "", Configured: c.Configured()}
	if d.Cfg.Port == 0 {
		d.Cfg.Security, d.Cfg.Port = mailer.StartTLS, mailer.DefaultPort(mailer.StartTLS)
	}
	d.Cfg.Password = ""
	if p := principalOf(r); p != nil {
		d.TestTo = p.User.Email
	}
	a.accessPage(w, r, "mail", accessData{Mail: d})
}

func (a *App) mailSave(w http.ResponseWriter, r *http.Request) {
	st := a.store()
	old, _ := st.MailConfig()
	c := mailer.Config{Host: strings.TrimSpace(r.FormValue("host")), Security: r.FormValue("security"), User: strings.TrimSpace(r.FormValue("user")),
		Password: r.FormValue("password"), From: strings.TrimSpace(r.FormValue("from"))}
	c.Port, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if c.Port == 0 {
		c.Port = mailer.DefaultPort(c.Security)
	}
	if c.Host == "" && c.From == "" { // both empty switches the mail features off
		if err := st.SaveMailConfig(mailer.Config{}); err != nil {
			a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
			return
		}
		redirect(w, r, "/access/mail")
		return
	}
	if err := c.Validate(); err != nil {
		a.fail(w, "Die Mail-Einstellungen sind ungültig: %s", err.Error())
		return
	}
	if c.Password == "" && c.User != "" && c.User == old.User {
		c.Password = old.Password // blank keeps the stored password
	}
	if c.User == "" {
		c.Password = ""
	}
	if err := st.SaveMailConfig(c); err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	redirect(w, r, "/access/mail")
}

func (a *App) mailTest(w http.ResponseWriter, r *http.Request) {
	to := strings.TrimSpace(r.FormValue("to"))
	if _, err := mailer.ParseAddress(to); err != nil {
		a.fail(w, "Bitte gib eine gültige Empfängeradresse an.")
		return
	}
	if _, ok := a.mailConfig(); !ok {
		a.fail(w, "Speichere zuerst die Mail-Einstellungen.")
		return
	}
	if err := a.sendMail(r.Context(), to, a.t("Testmail von Cubeherd"), a.t("Diese Nachricht bestätigt, dass Cubeherd E-Mails über diesen Server versenden kann.")); err != nil {
		a.record(r, "mail.test", to, access.Fail, "error", err.Error())
		a.fail(w, "Die Testmail konnte nicht gesendet werden: %s", err.Error())
		return
	}
	a.toast(w, "ok", "Testmail gesendet an %s.", to)
	w.WriteHeader(http.StatusNoContent)
}
