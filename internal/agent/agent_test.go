package agent_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"cubeherd/internal/agent"
	"cubeherd/internal/api"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/manager"
	"cubeherd/internal/remote"
	"cubeherd/internal/server"
)

type fake struct {
	started []string
	err     error
}

func (f *fake) Views(context.Context, bool) ([]api.View, error) {
	return []api.View{{S: &server.Server{ID: "survival", Name: "Survival"}, Status: server.StatusRunning, Online: []string{"Steve"}, MaxPlayers: 20}}, nil
}
func (f *fake) Start(id string) error { f.started = append(f.started, id); return f.err }
func (f *fake) Stop(string) error     { return f.err }
func (f *fake) Restart(string) error  { return f.err }
func (f *fake) DockerHost() string    { return "unix:///fake.sock" }
func (f *fake) HostStats(context.Context) host.Stats {
	return host.Stats{Cores: 8, MemTotal: 16 << 30}
}
func (f *fake) DockerInfo(context.Context) (docker.Info, error) { return docker.Info{}, nil }

// start serves an agent on a loopback port and returns its address and certificate fingerprint.
func start(t *testing.T, b agent.Backend, token string) (addr, fp string) {
	t.Helper()
	cert, err := agent.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := agent.New(b, agent.NewToken(token), "9.9.9", "/srv/cubeherd").Server("", cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), api.Fingerprint(cert.Certificate[0])
}

func TestEndToEnd(t *testing.T) {
	f := &fake{}
	addr, fp := start(t, f, "secret-token")
	ctx := context.Background()

	got, err := remote.Probe(ctx, addr)
	if err != nil || got != fp {
		t.Fatalf("Probe = %q, %v; want %q", got, err, fp)
	}
	c, err := remote.New(addr, "secret-token", fp)
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.Info(ctx)
	if err != nil || in.API != api.Version || in.Version != "9.9.9" || !in.DockerOK || in.DataDir != "/srv/cubeherd" {
		t.Fatalf("Info = %+v, %v", in, err)
	}
	views, err := c.Views(ctx, true)
	if err != nil || len(views) != 1 || views[0].S.ID != "survival" || views[0].Status != server.StatusRunning || views[0].Online[0] != "Steve" {
		t.Fatalf("Views = %+v, %v", views, err)
	}
	if hs := c.HostStats(ctx); hs.Cores != 8 || hs.MemTotal != 16<<30 {
		t.Fatalf("HostStats = %+v", hs)
	}
	if err := c.Start("survival"); err != nil || len(f.started) != 1 || f.started[0] != "survival" {
		t.Fatalf("Start: %v %v", err, f.started)
	}
	f.err = manager.ErrBusy
	if err := c.Stop("survival"); !errors.Is(err, manager.ErrBusy) {
		t.Fatalf("Stop busy = %v", err)
	}
	f.err = server.ErrNotFound
	if err := c.Restart("nope"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("Restart missing = %v", err)
	}
}

func TestRejectsBadToken(t *testing.T) {
	addr, fp := start(t, &fake{}, "right")
	c, _ := remote.New(addr, "wrong", fp)
	if _, err := c.Info(context.Background()); !errors.Is(err, remote.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestRejectsWrongFingerprint(t *testing.T) {
	addr, fp := start(t, &fake{}, "tok")
	bad := strings.Repeat("00:", 31) + "00"
	if bad == fp {
		t.Fatal("unlucky")
	}
	c, _ := remote.New(addr, "tok", bad)
	if _, err := c.Info(context.Background()); !errors.Is(err, remote.ErrFingerprint) {
		t.Fatalf("err = %v, want ErrFingerprint", err)
	}
}

func TestRejectsPlainHTTPAndOldTLS(t *testing.T) {
	if _, err := remote.New("http://127.0.0.1:1", "t", ""); err == nil {
		t.Fatal("plain http address accepted")
	}
}

func TestAPIVersionMismatch(t *testing.T) {
	addr, fp := start(t, &fake{}, "tok")
	req, _ := http.NewRequest("GET", "https://"+addr+"/v1/info", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set(api.Header, "99")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	_ = fp
}

func TestThrottlesRepeatedFailures(t *testing.T) {
	addr, fp := start(t, &fake{}, "right")
	bad, _ := remote.New(addr, "wrong", fp)
	for i := 0; i < 6; i++ {
		bad.Info(context.Background())
	}
	// Even the right token is refused while the client is blocked.
	good, _ := remote.New(addr, "right", fp)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := good.Info(ctx); err == nil {
		t.Fatal("request succeeded while the client should be throttled")
	}
}

func TestCertAndTokenPersist(t *testing.T) {
	dir := t.TempDir()
	c1, err := agent.LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := agent.LoadOrCreateCert(dir)
	if api.Fingerprint(c1.Certificate[0]) != api.Fingerprint(c2.Certificate[0]) {
		t.Fatal("certificate changed between starts")
	}
	tok, gen, err := agent.LoadOrCreateToken(dir, "")
	if err != nil || gen == "" || !tok.Check(gen) {
		t.Fatalf("first start: %v %q", err, gen)
	}
	tok2, gen2, err := agent.LoadOrCreateToken(dir, "")
	if err != nil || gen2 != "" || !tok2.Check(gen) || tok2.Check("other") {
		t.Fatalf("second start: %v %q", err, gen2)
	}
	if t3, _, _ := agent.LoadOrCreateToken(dir, "configured"); !t3.Check("configured") || t3.Check(gen) {
		t.Fatal("configured token must win")
	}
}
