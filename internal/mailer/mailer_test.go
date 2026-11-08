package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"mime"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// sink is a minimal SMTP server that records what it receives.
type sink struct {
	ln        net.Listener
	mu        sync.Mutex
	from      string
	rcpts     []string
	data      string
	authUser  string
	sawTLS    bool
	startTLS  bool
	implicit  bool
	cert      tls.Certificate
	refuseTo  string
	delivered chan struct{}
}

func newSink(t *testing.T, startTLS, implicit bool) *sink {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{ln: ln, startTLS: startTLS, implicit: implicit, cert: cert, delivered: make(chan struct{}, 4)}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *sink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *sink) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *sink) handle(c net.Conn) {
	defer c.Close()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{s.cert}}
	if s.implicit {
		c = tls.Server(c, tlsCfg)
		s.mu.Lock()
		s.sawTLS = true
		s.mu.Unlock()
	}
	r, w := bufio.NewReader(c), c
	reply := func(f string, a ...any) { fmt.Fprintf(w, f+"\r\n", a...) }
	reply("220 sink ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if s.startTLS && !s.sawTLS {
				fmt.Fprint(w, "250-sink\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
			} else {
				fmt.Fprint(w, "250-sink\r\n250 AUTH PLAIN\r\n")
			}
		case cmd == "STARTTLS":
			reply("220 go ahead")
			c = tls.Server(c, tlsCfg)
			r, w = bufio.NewReader(c), c
			s.mu.Lock()
			s.sawTLS = true
			s.mu.Unlock()
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			parts := strings.Fields(line)
			raw, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			f := strings.Split(string(raw), "\x00")
			s.mu.Lock()
			s.authUser = f[1]
			s.mu.Unlock()
			reply("235 ok")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			s.mu.Lock()
			s.from = strings.Trim(strings.TrimSpace(line[10:]), "<>")
			s.mu.Unlock()
			reply("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			to := strings.Trim(strings.TrimSpace(line[8:]), "<>")
			if s.refuseTo != "" && to == s.refuseTo {
				reply("550 no such user")
				continue
			}
			s.mu.Lock()
			s.rcpts = append(s.rcpts, to)
			s.mu.Unlock()
			reply("250 ok")
		case cmd == "DATA":
			reply("354 go")
			var b strings.Builder
			for {
				l, _ := r.ReadString('\n')
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			reply("250 queued")
			s.delivered <- struct{}{}
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

func cfg(s *sink, sec string) Config {
	return Config{Host: "127.0.0.1", Port: s.port(), Security: sec, From: "Cubeherd <noreply@example.com>"}
}

func TestSendWithoutEncryption(t *testing.T) {
	s := newSink(t, false, false)
	c := cfg(s, None)
	if err := Send(context.Background(), c, "anna@example.org", "Hello ✓", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	<-s.delivered
	if s.from != "noreply@example.com" || len(s.rcpts) != 1 || s.rcpts[0] != "anna@example.org" {
		t.Fatalf("envelope: %q %v", s.from, s.rcpts)
	}
	m, err := mail.ReadMessage(strings.NewReader(s.data))
	if err != nil {
		t.Fatal(err)
	}
	subj, _ := (&mimeDecoder{}).decode(m.Header.Get("Subject"))
	if subj != "Hello ✓" {
		t.Fatalf("subject %q", subj)
	}
	body := new(strings.Builder)
	buf := make([]byte, 512)
	for {
		n, err := m.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	dec, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(body.String(), "\r\n", ""), "\n", ""))
	if string(dec) != "line one\nline two" {
		t.Fatalf("body %q", dec)
	}
	if m.Header.Get("Message-ID") == "" || m.Header.Get("Date") == "" || m.Header.Get("Auto-Submitted") == "" {
		t.Fatalf("headers: %v", m.Header)
	}
}

func TestHeaderInjectionIsRefused(t *testing.T) {
	c := Config{Host: "127.0.0.1", Port: 25, Security: None, From: "noreply@example.com"}
	for name, args := range map[string][3]string{
		"recipient": {"a@b.c\r\nBcc: evil@x.y", "s", "b"},
		"subject":   {"a@b.c", "hi\r\nBcc: evil@x.y", "b"},
		"nul":       {"a@b.c\x00", "s", "b"},
		"no at":     {"nobody", "s", "b"},
	} {
		if _, _, _, err := Message(c, args[0], args[1], args[2], time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := c
	bad.From = "x@y.z\r\nBcc: q@r.s"
	if _, _, _, err := Message(bad, "a@b.c", "s", "b", time.Now()); err == nil {
		t.Error("a sender with a line break was accepted")
	}
}

func TestRecipientRefusalIsReported(t *testing.T) {
	s := newSink(t, false, false)
	s.refuseTo = "gone@example.org"
	err := Send(context.Background(), cfg(s, None), "gone@example.org", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "recipient refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartTLSIsRequiredWhenAsked(t *testing.T) {
	s := newSink(t, false, false) // offers no STARTTLS
	err := Send(context.Background(), cfg(s, StartTLS), "a@b.c", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("a downgrade to plain text must be refused: %v", err)
	}
	if s.data != "" {
		t.Fatal("nothing may be sent in the clear")
	}
}

func TestUnreachableServerFailsFast(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	start := time.Now()
	err := Send(context.Background(), Config{Host: "127.0.0.1", Port: port, Security: None, From: "a@b.c"}, "x@y.z", "s", "b")
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Host: "mail.example.com", Port: 587, Security: StartTLS, From: "a@b.c"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"no host":    func(c *Config) { c.Host = "" },
		"host path":  func(c *Config) { c.Host = "mail/evil" },
		"port":       func(c *Config) { c.Port = 0 },
		"security":   func(c *Config) { c.Security = "weird" },
		"sender":     func(c *Config) { c.From = "not an address" },
		"user lines": func(c *Config) { c.User = "a\r\nb" },
	} {
		c := ok
		mod(&c)
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if DefaultPort(TLS) != 465 || DefaultPort(StartTLS) != 587 || DefaultPort(None) != 25 {
		t.Fatal("default ports")
	}
}

type mimeDecoder struct{}

func (mimeDecoder) decode(s string) (string, error) {
	var d mime.WordDecoder
	return d.DecodeHeader(s)
}

func trust(t *testing.T, s *sink) {
	t.Helper()
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(s.cert.Certificate[0])
	pool.AddCert(leaf)
	rootCAs = pool
	t.Cleanup(func() { rootCAs = nil })
}

func TestStartTLSAndSignIn(t *testing.T) {
	s := newSink(t, true, false)
	trust(t, s)
	c := cfg(s, StartTLS)
	c.Host = "localhost"
	c.User, c.Password = "mailer", "secret"
	if err := Send(context.Background(), c, "anna@example.org", "s", "b"); err != nil {
		t.Fatal(err)
	}
	<-s.delivered
	if !s.sawTLS || s.authUser != "mailer" {
		t.Fatalf("tls=%v user=%q", s.sawTLS, s.authUser)
	}
}

func TestImplicitTLS(t *testing.T) {
	s := newSink(t, false, true)
	trust(t, s)
	c := cfg(s, TLS)
	c.Host = "localhost"
	if err := Send(context.Background(), c, "anna@example.org", "s", "b"); err != nil {
		t.Fatal(err)
	}
	<-s.delivered
}

func TestAnUntrustedCertificateIsRefused(t *testing.T) {
	s := newSink(t, true, false) // self-signed, and the test does not trust it
	c := cfg(s, StartTLS)
	c.Host = "localhost"
	if err := Send(context.Background(), c, "anna@example.org", "s", "b"); err == nil {
		t.Fatal("a certificate that does not verify must stop the mail")
	}
}
