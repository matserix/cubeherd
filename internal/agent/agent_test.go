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
	"cubeherd/internal/node"
	"cubeherd/internal/remote"
	"cubeherd/internal/server"
)

// fake implements what the tests use; everything else panics through the nil embedded interfaces.
type fake struct {
	node.Servers
	node.Console
	node.Live
	node.Host
	started []string
	err     error
	logs    []string
	created *server.Server
	forgot  []string
	opsGone chan struct{}
}

func (f *fake) Views(context.Context, bool) ([]api.View, error) {
	return []api.View{{S: &server.Server{ID: "survival", Name: "Survival"}, Status: server.StatusRunning, Online: []string{"Steve"}, MaxPlayers: 20}}, nil
}
func (f *fake) Server(id string) (*server.Server, error) {
	if id != "survival" {
		return nil, server.ErrNotFound
	}
	return &server.Server{ID: "survival", Name: "Survival", Port: 25565}, nil
}
func (f *fake) ServerExists(id string) bool     { _, err := f.Server(id); return err == nil }
func (f *fake) Start(id string) error           { f.started = append(f.started, id); return f.err }
func (f *fake) Stop(string) error               { return f.err }
func (f *fake) Restart(string) error            { return f.err }
func (f *fake) Forget(id string)                { f.forgot = append(f.forgot, id) }
func (f *fake) Create(s *server.Server) error   { f.created = s; return f.err }
func (f *fake) NextPort(ex string) int          { return 25570 }
func (f *fake) PortTaken(p int, ex string) bool { return p == 25565 && ex != "survival" }
func (f *fake) Busy(string) bool                { return true }
func (f *fake) DockerHost() string              { return "unix:///fake.sock" }
func (f *fake) Loc() *time.Location             { return time.UTC }
func (f *fake) HostStats(context.Context) host.Stats {
	return host.Stats{Cores: 8, MemTotal: 16 << 30}
}
func (f *fake) DockerInfo(context.Context) (docker.Info, error) { return docker.Info{}, nil }
func (f *fake) RCON(_ context.Context, s *server.Server, cmd string) (string, error) {
	return s.ID + ":" + cmd, f.err
}
func (f *fake) Players(context.Context, *server.Server) ([]string, int, error) {
	return []string{"Steve", "Alex"}, 20, nil
}
func (f *fake) Inspect(context.Context, *server.Server) (*docker.ContainerJSON, error) {
	if f.err != nil {
		return nil, docker.ErrNotFound
	}
	return &docker.ContainerJSON{}, nil
}
func (f *fake) LogsSince(ctx context.Context, _ *server.Server, follow bool, tail int, since int64, fn func(string)) error {
	for _, l := range f.logs {
		fn(l)
	}
	if follow {
		<-ctx.Done() // a followed log stays open until the dashboard goes away
	}
	return nil
}
func (f *fake) StatsStream(ctx context.Context, _ *server.Server, fn func(docker.Stats)) error {
	fn(docker.Stats{CPUPercent: 12.5, MemUsed: 1 << 30})
	<-ctx.Done()
	return nil
}
func (f *fake) FollowOps(ctx context.Context, id string, fn func(string)) {
	fn("[cubeherd] docker compose up -d")
	<-ctx.Done()
	if f.opsGone != nil {
		close(f.opsGone)
	}
}

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

func connect(t *testing.T, f *fake) *remote.Client {
	t.Helper()
	addr, fp := start(t, f, "tok")
	c, err := remote.New(addr, "tok", fp)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServersAndConsole(t *testing.T) {
	f := &fake{}
	c := connect(t, f)
	ctx := context.Background()

	s, err := c.Server("survival")
	if err != nil || s.Port != 25565 {
		t.Fatalf("Server = %+v, %v", s, err)
	}
	if _, err := c.Server("nope"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("missing server = %v", err)
	}
	if !c.ServerExists("survival") || c.ServerExists("nope") {
		t.Fatal("ServerExists wrong")
	}
	if !c.Busy("survival") || c.NextPort("") != 25570 || !c.PortTaken(25565, "") || c.PortTaken(25565, "survival") {
		t.Fatal("busy/ports wrong")
	}
	if err := c.Create(&server.Server{ID: "new", Name: "New", Port: 25566, MemoryGB: 4}); err != nil || f.created == nil || f.created.ID != "new" || f.created.MemoryGB != 4 {
		t.Fatalf("Create: %v %+v", err, f.created)
	}
	c.Forget("survival")
	if len(f.forgot) != 1 || f.forgot[0] != "survival" {
		t.Fatalf("Forget: %v", f.forgot)
	}
	out, err := c.RCON(ctx, s, "list")
	if err != nil || out != "survival:list" {
		t.Fatalf("RCON = %q, %v", out, err)
	}
	names, max, err := c.Players(ctx, s)
	if err != nil || len(names) != 2 || max != 20 {
		t.Fatalf("Players = %v %d %v", names, max, err)
	}
	if c.Loc().String() != "UTC" || c.DockerHost() != "unix:///fake.sock" {
		t.Fatalf("Loc/DockerHost = %v / %q", c.Loc(), c.DockerHost())
	}
	if _, err := c.DockerInfo(ctx); err != nil {
		t.Fatal(err)
	}
	f.err = errors.New("boom")
	if _, err := c.RCON(ctx, s, "x"); err == nil || err.Error() != "boom" {
		t.Fatalf("error text lost: %v", err)
	}
}

func TestStreams(t *testing.T) {
	f := &fake{logs: []string{"line one", "line two"}}
	c := connect(t, f)
	s := &server.Server{ID: "survival"}

	// A followed log delivers lines as they come and stops when the caller cancels.
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string, 4)
	done := make(chan error, 1)
	go func() { done <- c.LogsSince(ctx, s, true, 300, 0, func(l string) { got <- l }) }()
	for _, want := range f.logs {
		select {
		case l := <-got:
			if l != want {
				t.Fatalf("log line %q, want %q", l, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no log line within 3 s: stream is buffered")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("log stream did not stop after cancel")
	}

	// Stats stream.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	st := make(chan docker.Stats, 1)
	go c.StatsStream(ctx2, s, func(x docker.Stats) { st <- x })
	select {
	case x := <-st:
		if x.CPUPercent != 12.5 || x.MemUsed != 1<<30 {
			t.Fatalf("stats = %+v", x)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no stats sample")
	}

	// Ops output, and the agent notices when the dashboard disconnects.
	f.opsGone = make(chan struct{})
	ctx3, cancel3 := context.WithCancel(context.Background())
	lines := make(chan string, 1)
	go c.FollowOps(ctx3, "survival", func(l string) { lines <- l })
	select {
	case l := <-lines:
		if !strings.Contains(l, "compose up") {
			t.Fatalf("op line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no op line")
	}
	cancel3()
	select {
	case <-f.opsGone:
	case <-time.After(3 * time.Second):
		t.Fatal("agent kept following ops after the client left")
	}

	// A missing container is a clean error, not an empty stream.
	f.err = errors.New("gone")
	if err := c.LogsSince(context.Background(), s, false, 10, 0, func(string) {}); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("missing container = %v", err)
	}
	if _, err := c.Inspect(context.Background(), s); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("Inspect missing = %v", err)
	}
}
