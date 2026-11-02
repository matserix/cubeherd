package nodes_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"cubeherd/internal/agent"
	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/node"
	"cubeherd/internal/nodes"
	"cubeherd/internal/server"
)

// fakeNode is both the dashboard's "local" node and the backend of the test agents.
type fakeNode struct {
	node.Servers
	node.Console
	node.Live
	node.Files
	node.Addons
	node.Backups
	node.Host
	name   string
	pushed atomic.Int32
	last   atomic.Pointer[api.Config]
}

func (f *fakeNode) Views(context.Context, bool) ([]api.View, error) {
	return []api.View{{S: &server.Server{ID: f.name + "-srv", Name: f.name}, Status: server.StatusRunning}}, nil
}
func (f *fakeNode) HostStats(context.Context) host.Stats {
	return host.Stats{Cores: 4, MemTotal: 8 << 30}
}
func (f *fakeNode) DockerInfo(context.Context) (docker.Info, error) { return docker.Info{}, nil }
func (f *fakeNode) DockerHost() string                              { return "unix:///x" }
func (f *fakeNode) Loc() *time.Location                             { return time.UTC }
func (f *fakeNode) ArchiveDir() string                              { return "/a" }
func (f *fakeNode) ApplyConfig(c api.Config) error {
	f.pushed.Add(1)
	f.last.Store(&c)
	return nil
}

type agentUnderTest struct {
	addr, fp string
	srv      *http.Server
	node     *fakeNode
}

func startAgent(t *testing.T, name, token, version string) *agentUnderTest {
	t.Helper()
	cert, err := agent.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNode{name: name}
	srv := agent.New(f, agent.NewToken(token), version, "/srv").Server("", cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	t.Cleanup(func() { srv.Close() })
	return &agentUnderTest{addr: ln.Addr().String(), fp: api.Fingerprint(cert.Certificate[0]), srv: srv, node: f}
}

func newRegistry(t *testing.T, local node.Node, version string) (*nodes.Registry, *config.Store) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return nodes.New(cfg, local, version), cfg
}

func TestRefreshCombinesLocalAndRemote(t *testing.T) {
	a := startAgent(t, "vps", "tok", "1.0.0")
	reg, cfg := newRegistry(t, &fakeNode{name: "home"}, "1.0.0")
	cfg.Update(func(s *config.Settings) {
		s.Timezone, s.Language = "Europe/Berlin", "en"
		s.Nodes = []config.Node{{ID: "vps", Name: "VPS", Addr: a.addr, Token: "tok", Fingerprint: a.fp, PortMin: 25600, PortMax: 25700}}
	})

	got := reg.Refresh(context.Background(), false, 0)
	if len(got) != 2 || got[0].ID != nodes.LocalID || !got[0].Local || got[1].ID != "vps" {
		t.Fatalf("nodes = %+v", got)
	}
	if got[0].Views[0].S.Node != "local" || got[1].Views[0].S.Node != "vps" {
		t.Fatalf("servers are not tagged with their node: %q %q", got[0].Views[0].S.Node, got[1].Views[0].S.Node)
	}
	if got[1].Status.State != nodes.Online || got[1].Status.Version != "1.0.0" || got[1].Host.Cores != 4 || got[1].Status.Latency <= 0 {
		t.Fatalf("remote status = %+v host %+v", got[1].Status, got[1].Host)
	}
	if n := a.node.pushed.Load(); n != 1 {
		t.Fatalf("settings pushed %d times, want 1", n)
	}
	if c := a.node.last.Load(); c.Timezone != "Europe/Berlin" || c.Language != "en" || c.PortMin != 25600 || c.PortMax != 25700 {
		t.Fatalf("pushed settings = %+v", c)
	}

	// A second refresh does not push again, and a fresh enough result is served from the cache.
	reg.Refresh(context.Background(), false, time.Minute)
	reg.Refresh(context.Background(), false, 0)
	if n := a.node.pushed.Load(); n != 1 {
		t.Fatalf("pushed %d times after unchanged refreshes", n)
	}
	// Changed settings are pushed on the next refresh.
	cfg.Update(func(s *config.Settings) { s.Language = "de" })
	reg.ForgetPushed()
	reg.Refresh(context.Background(), false, 0)
	if n := a.node.pushed.Load(); n != 2 || a.node.last.Load().Language != "de" {
		t.Fatalf("after a settings change: pushed %d, %+v", n, a.node.last.Load())
	}
}

func TestUnreachableNodeKeepsLastKnownServers(t *testing.T) {
	a := startAgent(t, "vps", "tok", "1.0.0")
	reg, cfg := newRegistry(t, nil, "1.0.0")
	cfg.Update(func(s *config.Settings) {
		s.Nodes = []config.Node{{ID: "vps", Name: "VPS", Addr: a.addr, Token: "tok", Fingerprint: a.fp}}
	})
	if got := reg.Refresh(context.Background(), false, 0); got[0].Status.State != nodes.Online || len(got[0].Views) != 1 {
		t.Fatalf("online refresh = %+v", got[0].Status)
	}
	a.srv.Close()

	start := time.Now()
	got := reg.Refresh(context.Background(), false, 0)
	if time.Since(start) > nodes.Wait+time.Second {
		t.Fatalf("an unreachable node stalled the refresh for %v", time.Since(start))
	}
	n := got[0]
	if n.Status.State != nodes.Offline || n.Status.Reason != nodes.ReasonUnreachable || !n.Stale {
		t.Fatalf("status = %+v stale=%v", n.Status, n.Stale)
	}
	if len(n.Views) != 1 || n.Views[0].S.Node != "vps" {
		t.Fatalf("last known servers lost: %+v", n.Views)
	}
	if n.Status.LastSeen.IsZero() {
		t.Fatal("LastSeen not kept")
	}
}

func TestFailureReasons(t *testing.T) {
	a := startAgent(t, "vps", "tok", "1.0.0")
	reg, cfg := newRegistry(t, nil, "1.0.0")
	set := func(token, fp string) {
		cfg.Update(func(s *config.Settings) {
			s.Nodes = []config.Node{{ID: "vps", Name: "VPS", Addr: a.addr, Token: token, Fingerprint: fp}}
		})
	}
	set("wrong", a.fp)
	if st := reg.Refresh(context.Background(), false, 0)[0].Status; st.State != nodes.Offline || st.Reason != nodes.ReasonToken {
		t.Fatalf("wrong token: %+v", st)
	}
	set("tok", "00:11")
	if st := reg.Refresh(context.Background(), false, 0)[0].Status; st.State != nodes.Offline || st.Reason != nodes.ReasonFingerprint {
		t.Fatalf("wrong fingerprint: %+v", st)
	}
	set("tok", a.fp)
	if st := reg.Refresh(context.Background(), false, 0)[0].Status; st.State != nodes.Online {
		t.Fatalf("recovered: %+v", st)
	}
}

func TestOutdatedAgent(t *testing.T) {
	a := startAgent(t, "vps", "tok", "0.9.0")
	reg, cfg := newRegistry(t, nil, "1.0.0")
	cfg.Update(func(s *config.Settings) {
		s.Nodes = []config.Node{{ID: "vps", Name: "VPS", Addr: a.addr, Token: "tok", Fingerprint: a.fp}}
	})
	if st := reg.Refresh(context.Background(), false, 0)[0].Status; st.State != nodes.Outdated || st.Version != "0.9.0" {
		t.Fatalf("status = %+v", st)
	}
	// Development builds never count as outdated.
	reg2, cfg2 := newRegistry(t, nil, "dev")
	cfg2.Update(func(s *config.Settings) {
		s.Nodes = []config.Node{{ID: "vps", Name: "VPS", Addr: a.addr, Token: "tok", Fingerprint: a.fp}}
	})
	if st := reg2.Refresh(context.Background(), false, 0)[0].Status; st.State != nodes.Online {
		t.Fatalf("dev dashboard: %+v", st)
	}
}

func TestAddEditRemove(t *testing.T) {
	a := startAgent(t, "vps", "tok", "1.0.0")
	reg, cfg := newRegistry(t, &fakeNode{name: "home"}, "1.0.0")
	ctx := context.Background()

	fp, err := reg.Probe(ctx, a.addr)
	if err != nil || fp != a.fp {
		t.Fatalf("Probe = %q %v", fp, err)
	}
	in, err := reg.Check(ctx, a.addr, "tok", fp)
	if err != nil || in.Version != "1.0.0" {
		t.Fatalf("Check = %+v %v", in, err)
	}
	if _, err := reg.Check(ctx, a.addr, "nope", fp); err == nil {
		t.Fatal("Check accepted a wrong token")
	}

	e, err := reg.Add("EU West 2", a.addr, "tok", fp)
	if err != nil || e.ID != "eu-west-2" || e.Addr != "https://"+a.addr {
		t.Fatalf("Add = %+v %v", e, err)
	}
	if _, err := reg.Add("eu west 2", a.addr, "tok", fp); !errors.Is(err, nodes.ErrExists) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := reg.Add("Local", a.addr, "tok", fp); err == nil {
		t.Fatal("the reserved id was accepted")
	}
	if _, err := reg.Add("!!!", a.addr, "tok", fp); err == nil {
		t.Fatal("an empty id was accepted")
	}
	if l := reg.List(); len(l) != 2 || l[0].ID != "local" || l[1].ID != "eu-west-2" {
		t.Fatalf("List = %+v", l)
	}
	if d, ok := reg.Default(); !ok || d.ID != "local" {
		t.Fatalf("Default = %+v", d)
	}

	if err := reg.Edit("eu-west-2", func(n *config.Node) { n.PortMin, n.PortMax = 26000, 26100 }); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Get().Nodes[0]; got.PortMin != 26000 {
		t.Fatalf("edit not stored: %+v", got)
	}
	if err := reg.Edit("nope", func(*config.Node) {}); err == nil {
		t.Fatal("editing an unknown node succeeded")
	}
	if err := reg.Remove("eu-west-2"); err != nil || len(reg.List()) != 1 {
		t.Fatalf("Remove: %v %v", err, reg.List())
	}
}

func TestWithoutLocalNodeTheFirstRemoteIsTheDefault(t *testing.T) {
	a := startAgent(t, "vps", "tok", "1.0.0")
	reg, cfg := newRegistry(t, nil, "1.0.0")
	if _, ok := reg.Default(); ok {
		t.Fatal("a registry without nodes has a default")
	}
	cfg.Update(func(s *config.Settings) {
		s.Nodes = []config.Node{{ID: "vps", Name: "VPS", Addr: a.addr, Token: "tok", Fingerprint: a.fp}}
	})
	if d, ok := reg.Default(); !ok || d.ID != "vps" || d.Local {
		t.Fatalf("Default = %+v", d)
	}
}
