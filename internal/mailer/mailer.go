// Package mailer sends plain-text e-mail through an SMTP server, with the standard library only.
package mailer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Security modes of the connection.
const (
	StartTLS = "starttls" // plain connection, upgraded with STARTTLS (usually port 587)
	TLS      = "tls"      // TLS from the first byte (usually port 465)
	None     = "none"     // no encryption (a local relay)
)

// Config is how to reach the SMTP server.
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	User     string `json:"user,omitempty"`
	Password string `json:"-"`
	From     string `json:"from"`
}

// Configured reports whether enough is set to send mail.
func (c Config) Configured() bool { return c.Host != "" && c.From != "" }

// DefaultPort is the usual port for a security mode.
func DefaultPort(security string) int {
	switch security {
	case TLS:
		return 465
	case None:
		return 25
	}
	return 587
}

// Validate checks the settings without connecting.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" || strings.ContainsAny(c.Host, " \r\n/") {
		return errors.New("the SMTP host is missing or invalid")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("the SMTP port is invalid")
	}
	switch c.Security {
	case StartTLS, TLS, None:
	default:
		return errors.New("unknown connection security")
	}
	if _, err := ParseAddress(c.From); err != nil {
		return errors.New("the sender address is invalid")
	}
	if strings.ContainsAny(c.User, "\r\n\x00") {
		return errors.New("the SMTP user name is invalid")
	}
	return nil
}

// ParseAddress accepts "name <user@host>" or "user@host" and refuses anything with line breaks or control characters.
func ParseAddress(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return nil, errors.New("control characters in an address")
	}
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(a.Address, "\r\n\x00 ") || !strings.Contains(a.Address, "@") {
		return nil, errors.New("invalid address")
	}
	return a, nil
}

// Message builds the RFC 5322 text of a mail. Headers are built from checked values, never from raw input.
func Message(c Config, to, subject, body string, now time.Time) ([]byte, string, string, error) {
	from, err := ParseAddress(c.From)
	if err != nil {
		return nil, "", "", err
	}
	rcpt, err := ParseAddress(to)
	if err != nil {
		return nil, "", "", err
	}
	if strings.ContainsAny(subject, "\r\n\x00") {
		return nil, "", "", errors.New("control characters in the subject")
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	h("To", rcpt.String())
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "base64")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(body, "\r\n", "\n")))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return []byte(b.String()), from.Address, rcpt.Address, nil
}

// rootCAs replaces the system roots; only the tests set it.
var rootCAs *x509.CertPool

// Send delivers one mail. It gives up after the context ends or 20 seconds.
func Send(ctx context.Context, c Config, to, subject, body string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	msg, from, rcpt, err := Message(c, to, subject, body, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12, RootCAs: rootCAs}
	if c.Security == TLS {
		conn = tls.Client(conn, tlsCfg)
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("greeting from %s: %w", addr, err)
	}
	defer cl.Close()
	if c.Security == StartTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS; choose another connection security or fix the server")
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Password, c.Host)); err != nil {
			return fmt.Errorf("sign-in at the mail server: %w", err)
		}
	}
	if err := cl.Mail(from); err != nil {
		return fmt.Errorf("sender refused: %w", err)
	}
	if err := cl.Rcpt(rcpt); err != nil {
		return fmt.Errorf("recipient refused: %w", err)
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the server did not accept the message: %w", err)
	}
	return cl.Quit()
}
