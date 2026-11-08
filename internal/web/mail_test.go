package web

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cubeherd/internal/access"
)

// smtpSink accepts mail on a loopback port without encryption and records recipients and data.
type smtpSink struct {
	ln    net.Listener
	mu    sync.Mutex
	rcpt  []string
	data  []string
	auths []string
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *smtpSink) port() string { return fmt.Sprint(s.ln.Addr().(*net.TCPAddr).Port) }

func (s *smtpSink) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	fmt.Fprint(c, "220 ok\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		up := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(up, "EHLO"):
			fmt.Fprint(c, "250 ok\r\n")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.mu.Lock()
			s.rcpt = append(s.rcpt, strings.Trim(strings.TrimSpace(line[8:]), "<>"))
			s.mu.Unlock()
			fmt.Fprint(c, "250 ok\r\n")
		case up == "DATA":
			fmt.Fprint(c, "354 go\r\n")
			var b strings.Builder
			for {
				l, _ := r.ReadString('\n')
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = append(s.data, b.String())
			s.mu.Unlock()
			fmt.Fprint(c, "250 queued\r\n")
		case up == "QUIT":
			fmt.Fprint(c, "221 bye\r\n")
			return
		default:
			fmt.Fprint(c, "250 ok\r\n")
		}
	}
}

func (s *smtpSink) received() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.data) }

func TestMailSettingsAreStoredEncryptedAndTestMailIsSent(t *testing.T) {
	d, st := newAccounts(t)
	sink := newSMTPSink(t)
	owner := d.cookieFor(t, st, "admin")
	save := func(f url.Values) *map[string][]string {
		rec := d.as(owner, "POST", "/access/mail", f, true)
		h := map[string][]string(rec.Header())
		return &h
	}
	// refused: no usable settings
	if h := *save(url.Values{"host": {"mail.example.com"}, "port": {"587"}, "security": {"bogus"}, "from": {"noreply@example.com"}}); !strings.Contains(strings.Join(h["Hx-Trigger"], ""), "crit") {
		t.Fatal("an unknown security mode must be refused")
	}
	if c, _ := st.MailConfig(); c.Configured() {
		t.Fatal("nothing should have been stored")
	}
	good := url.Values{"host": {"127.0.0.1"}, "port": {sink.port()}, "security": {"none"}, "from": {"Cubeherd <noreply@example.com>"}}
	save(good)
	c, err := st.MailConfig()
	if err != nil || c.Host != "127.0.0.1" || !c.Configured() {
		t.Fatalf("stored: %+v %v", c, err)
	}
	// a password is encrypted at rest and a blank field keeps it
	withPw := url.Values{"host": {"127.0.0.1"}, "port": {sink.port()}, "security": {"none"}, "from": {"noreply@example.com"}, "user": {"mailer"}, "password": {"s3cret-smtp"}}
	save(withPw)
	if raw := st.KV("mail.smtp"); strings.Contains(raw, "s3cret-smtp") || !strings.Contains(raw, `"password":"`) {
		t.Fatalf("the SMTP password is stored in plain text: %s", raw)
	}
	withPw.Set("password", "")
	save(withPw)
	if c, _ := st.MailConfig(); c.Password != "s3cret-smtp" {
		t.Fatalf("a blank password field must keep the stored one, got %q", c.Password)
	}
	// the page never shows the password
	if body := d.as(owner, "GET", "/access/mail", nil, false).Body.String(); strings.Contains(body, "s3cret-smtp") {
		t.Fatal("the password is shown on the page")
	}
	// test mail (the sink offers no AUTH, so sign in must not be attempted with a user: clear it)
	save(good)
	rec := d.as(owner, "POST", "/access/mail/test", url.Values{"to": {"anna@example.org"}}, true)
	if strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatalf("test mail failed: %v", rec.Header())
	}
	deadline := time.Now().Add(3 * time.Second)
	for sink.received() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sink.received() != 1 || len(sink.rcpt) != 1 || sink.rcpt[0] != "anna@example.org" {
		t.Fatalf("delivered %d mail(s) to %v", sink.received(), sink.rcpt)
	}
	// both address fields empty switch the feature off
	save(url.Values{})
	if c, _ := st.MailConfig(); c.Configured() {
		t.Fatal("emptying the settings should switch mail off")
	}
	if rec := d.as(owner, "POST", "/access/mail/test", url.Values{"to": {"anna@example.org"}}, true); !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatal("a test mail without settings must be refused")
	}
	// audited
	es, _, _ := st.Audit(access.Filter{Action: "mail."})
	if len(es) < 3 {
		t.Fatalf("mail changes should be audited: %d entries", len(es))
	}
}

func TestOnlySettingsManagersSeeTheMailTab(t *testing.T) {
	d, st := newAccounts(t)
	u := addUser(t, st, "marta")
	giveRole(t, st, u, access.RoleViewer, access.Scope{Level: access.LevelServer, Node: "local", Server: "loc-one"})
	c := d.cookieFor(t, st, "marta")
	for _, p := range []string{"/access/mail"} {
		if got := d.as(c, "GET", p, nil, false).Code; got != 403 {
			t.Errorf("%s: %d", p, got)
		}
	}
	if got := d.as(c, "POST", "/access/mail/test", url.Values{"to": {"a@b.c"}}, false).Code; got != 403 {
		t.Errorf("test mail: %d", got)
	}
}
