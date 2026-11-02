// Package nodes keeps the dashboard's view of every host it manages: the embedded local node (when
// enabled) and the remote agents from the settings. It answers "which nodes are there, are they
// reachable, and what do they run" with a bounded wait, so one dead host never stalls a page.
package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/host"
	"cubeherd/internal/node"
	"cubeherd/internal/remote"
	"cubeherd/internal/server"
)

// LocalID is the id of the embedded node. It is reserved, remote nodes cannot take it.
const LocalID = "local"

// Wait bounds every request to a remote node that a page view depends on.
const Wait = 2 * time.Second

// State is what the dashboard knows about a node right now.
type State string

const (
	Online   State = "online"
	Outdated State = "outdated" // reachable and compatible, but running another version than the dashboard
	Offline  State = "offline"
)

// Reasons an offline node gives; the UI translates them.
const (
	ReasonUnreachable = "unreachable"
	ReasonToken       = "token"
	ReasonFingerprint = "fingerprint"
	ReasonAPI         = "api"
)

// Status is one node's reachability and identity.
type Status struct {
	State     State
	Reason    string // set when State is Offline
	Detail    string // the underlying error, for the details line
	Version   string
	Hostname  string
	Latency   time.Duration
	LastSeen  time.Time
	DockerOK  bool
	DockerErr string
}

// Info describes one node and gives access to it.
type Info struct {
	ID     string
	Name   string
	Local  bool
	Addr   string // "" for the local node
	Node   node.Node
	Client *remote.Client // nil for the local node
	Entry  config.Node    // zero for the local node
}

// View is a node together with its status, resources and servers.
type View struct {
	Info
	Status Status
	Host   host.Stats
	Views  []api.View
	Stale  bool // the servers are the last known ones, not current
}

type remoteState struct {
	entry config.Node
	c     *remote.Client

	status    Status
	host      host.Stats
	views     []api.View
	viewsAt   time.Time
	withStats bool
	pushed    string // hash of the settings last pushed to the agent
}

// Registry is safe for concurrent use.
type Registry struct {
	cfg     *config.Store
	local   node.Node
	version string

	mu      sync.Mutex
	remotes map[string]*remoteState

	nmu          sync.Mutex // guards the cached Docker host name of the local node
	dockerName   string
	dockerNameAt time.Time
}

// New returns a registry. local may be nil when the dashboard has no Docker access of its own.
func New(cfg *config.Store, local node.Node, version string) *Registry {
	return &Registry{cfg: cfg, local: local, version: version, remotes: map[string]*remoteState{}}
}

// sync reconciles the remote clients with the settings and returns them in settings order. Callers hold r.mu.
func (r *Registry) sync() []*remoteState {
	entries := r.cfg.Get().Nodes
	out := make([]*remoteState, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ID] = true
		st := r.remotes[e.ID]
		if st == nil || st.entry.Addr != e.Addr || st.entry.Token != e.Token || st.entry.Fingerprint != e.Fingerprint {
			c, err := remote.New(e.Addr, e.Token, e.Fingerprint)
			if err != nil {
				continue // a malformed address in the file; the node is skipped rather than crashing the page
			}
			st = &remoteState{c: c, status: Status{State: Offline, Reason: ReasonUnreachable}}
			r.remotes[e.ID] = st
		}
		st.entry = e
		out = append(out, st)
	}
	for id := range r.remotes {
		if !seen[id] {
			delete(r.remotes, id)
		}
	}
	return out
}

// localName is the display name of the embedded node: the configured one, else the Docker host's name
// (inside a container the process's own host name is only the container id), else the process's host name.
func (r *Registry) localName() string {
	if n := r.cfg.Get().LocalName; n != "" {
		return n
	}
	r.nmu.Lock()
	defer r.nmu.Unlock()
	if r.dockerName == "" && time.Since(r.dockerNameAt) > time.Minute && r.local != nil {
		r.dockerNameAt = time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if in, err := r.local.DockerInfo(ctx); err == nil {
			r.dockerName = in.Name
		}
		cancel()
	}
	if r.dockerName != "" {
		return r.dockerName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return strings.TrimSuffix(h, ".local")
	}
	return "local"
}

func (r *Registry) info(st *remoteState) Info {
	return Info{ID: st.entry.ID, Name: st.entry.Name, Addr: st.entry.Addr, Node: st.c, Client: st.c, Entry: st.entry}
}

// List returns every node: the local one first, then the remote ones in settings order.
func (r *Registry) List() []Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Info
	if r.local != nil {
		out = append(out, Info{ID: LocalID, Name: r.localName(), Local: true, Node: r.local})
	}
	for _, st := range r.sync() {
		out = append(out, r.info(st))
	}
	return out
}

// Local is the embedded node, or nil when the dashboard has no Docker access of its own.
func (r *Registry) Local() node.Node { return r.local }

// Get finds a node by id.
func (r *Registry) Get(id string) (Info, bool) {
	for _, n := range r.List() {
		if n.ID == id {
			return n, true
		}
	}
	return Info{}, false
}

// Default is the node used when a URL names none: the local one, else the first remote one.
func (r *Registry) Default() (Info, bool) {
	l := r.List()
	if len(l) == 0 {
		return Info{}, false
	}
	return l[0], true
}

// Config builds what an agent needs from the dashboard's global settings.
func (r *Registry) config(e config.Node) api.Config {
	s := r.cfg.Get()
	return api.Config{Timezone: s.Timezone, Language: s.Language, CFAPIKey: s.CFAPIKey, Remote: s.Remote, PortMin: e.PortMin, PortMax: e.PortMax}
}

func hash(c api.Config) string {
	b, _ := json.Marshal(c)
	return string(b)
}

// ForgetPushed makes the next refresh hand every agent the current settings again.
func (r *Registry) ForgetPushed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.remotes {
		st.pushed = ""
	}
}

func classify(err error) (reason string) {
	switch {
	case errors.Is(err, remote.ErrUnauthorized):
		return ReasonToken
	case errors.Is(err, remote.ErrFingerprint):
		return ReasonFingerprint
	case errors.Is(err, remote.ErrAPIVersion):
		return ReasonAPI
	}
	return ReasonUnreachable
}

// Refresh asks every node for its status, resources and servers, in parallel and with a bounded wait.
// Remote results younger than maxAge are reused. A node that does not answer keeps its last known
// servers, marked stale, so the UI can still show them.
func (r *Registry) Refresh(ctx context.Context, withStats bool, maxAge time.Duration) []View {
	r.mu.Lock()
	remotes := r.sync()
	r.mu.Unlock()

	var out []View
	if r.local != nil {
		out = append(out, View{})
	}
	out = append(out, make([]View, len(remotes))...)

	var wg sync.WaitGroup
	idx := 0
	if r.local != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views, _ := r.local.Views(ctx, withStats)
			tag(views, LocalID, r.localName())
			out[0] = View{
				Info:   Info{ID: LocalID, Name: r.localName(), Local: true, Node: r.local},
				Status: Status{State: Online, Version: r.version, LastSeen: time.Now(), DockerOK: true},
				Host:   r.local.HostStats(ctx), Views: views,
			}
		}()
		idx = 1
	}
	for i, st := range remotes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[idx+i] = r.refreshRemote(ctx, st, withStats, maxAge)
		}()
	}
	wg.Wait()
	return out
}

func tag(views []api.View, id, name string) {
	for i := range views {
		if views[i].S != nil {
			views[i].S.Node, views[i].S.NodeName = id, name
		}
	}
}

func (r *Registry) snapshot(st *remoteState, stale bool) View {
	r.mu.Lock()
	defer r.mu.Unlock()
	views := make([]api.View, len(st.views))
	copy(views, st.views)
	if stale {
		for i := range views {
			views[i].Status = server.StatusUnknown // the last known list stays, but not a state that may have changed
		}
	}
	return View{Info: r.info(st), Status: st.status, Host: st.host, Views: views, Stale: stale}
}

func (r *Registry) refreshRemote(ctx context.Context, st *remoteState, withStats bool, maxAge time.Duration) View {
	r.mu.Lock()
	entry := st.entry // sync rewrites st.entry when the settings change; work on a copy
	fresh := st.status.State != Offline && time.Since(st.viewsAt) < maxAge && (st.withStats || !withStats)
	r.mu.Unlock()
	if fresh {
		return r.snapshot(st, false)
	}

	ctx, cancel := context.WithTimeout(ctx, Wait)
	defer cancel()
	t0 := time.Now()
	in, err := st.c.Info(ctx)
	if err == nil && in.API != api.Version {
		err = fmt.Errorf("%w: agent speaks API %d", remote.ErrAPIVersion, in.API)
	}
	var views []api.View
	var hs host.Stats
	if err == nil {
		latency := time.Since(t0)
		if views, err = st.c.Views(ctx, withStats); err == nil {
			hs = st.c.HostStats(ctx)
			tag(views, entry.ID, entry.Name)
			r.mu.Lock()
			state := Online
			if in.Version != r.version && in.Version != "dev" && r.version != "dev" {
				state = Outdated
			}
			st.status = Status{State: state, Version: in.Version, Hostname: in.Hostname, Latency: latency, LastSeen: time.Now(), DockerOK: in.DockerOK, DockerErr: in.DockerErr}
			st.host, st.views, st.viewsAt, st.withStats = hs, views, time.Now(), withStats
			cfg := r.config(entry)
			needPush := st.pushed != hash(cfg)
			r.mu.Unlock()
			if needPush {
				if st.c.PushConfig(ctx, cfg) == nil {
					r.mu.Lock()
					st.pushed = hash(cfg)
					r.mu.Unlock()
				}
			}
			return r.snapshot(st, false)
		}
	}
	r.mu.Lock()
	prev := st.status
	st.status = Status{State: Offline, Reason: classify(err), Detail: err.Error(), Version: prev.Version, Hostname: prev.Hostname, LastSeen: prev.LastSeen}
	r.mu.Unlock()
	return r.snapshot(st, true)
}

// Status is a node's reachability. A remote answer younger than five seconds is reused, so a page that
// makes several requests asks the agent once.
func (r *Registry) Status(ctx context.Context, id string) Status {
	r.mu.Lock()
	st := r.remotes[id]
	if st == nil {
		r.sync()
		st = r.remotes[id]
	}
	r.mu.Unlock()
	if st == nil {
		return Status{State: Offline, Reason: ReasonUnreachable}
	}
	return r.refreshRemote(ctx, st, false, 5*time.Second).Status
}

// Probe fetches the certificate fingerprint an agent presents, for trust on first use.
func (r *Registry) Probe(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return remote.Probe(ctx, addr)
}

// Check connects with the given credentials and returns the agent's identity. It is the add-node connection test.
func (r *Registry) Check(ctx context.Context, addr, token, fingerprint string) (api.Info, error) {
	c, err := remote.New(addr, token, fingerprint)
	if err != nil {
		return api.Info{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in, err := c.Info(ctx)
	if err == nil && in.API != api.Version {
		err = fmt.Errorf("%w: agent speaks API %d", remote.ErrAPIVersion, in.API)
	}
	return in, err
}

var ErrExists = errors.New("a node with this name already exists")

// Add stores a new remote node. The caller has already run Check.
func (r *Registry) Add(name, addr, token, fingerprint string) (config.Node, error) {
	name = strings.TrimSpace(name)
	id := server.Slug(name)
	if id == "" || id == LocalID {
		return config.Node{}, errors.New("invalid node name")
	}
	if _, err := remote.New(addr, token, fingerprint); err != nil {
		return config.Node{}, err
	}
	e := config.Node{ID: id, Name: name, Addr: normalizeAddr(addr), Token: token, Fingerprint: fingerprint}
	exists := false
	err := r.cfg.Update(func(s *config.Settings) {
		for _, n := range s.Nodes {
			if n.ID == id {
				exists = true
				return
			}
		}
		s.Nodes = append(s.Nodes, e)
	})
	if err != nil {
		return config.Node{}, err
	}
	if exists {
		return config.Node{}, ErrExists
	}
	return e, nil
}

// normalizeAddr stores the address as https://host:port.
func normalizeAddr(a string) string {
	a = strings.TrimSpace(a)
	if !strings.Contains(a, "://") {
		a = "https://" + a
	}
	return strings.TrimRight(a, "/")
}

// Edit changes a stored node.
func (r *Registry) Edit(id string, fn func(*config.Node)) error {
	found := false
	err := r.cfg.Update(func(s *config.Settings) {
		for i := range s.Nodes {
			if s.Nodes[i].ID == id {
				fn(&s.Nodes[i])
				found = true
			}
		}
	})
	if err == nil && !found {
		return errors.New("unknown node")
	}
	r.ForgetPushed()
	return err
}

// Remove forgets a remote node. Its servers keep running on their host.
func (r *Registry) Remove(id string) error {
	return r.cfg.Update(func(s *config.Settings) {
		out := s.Nodes[:0:0]
		for _, n := range s.Nodes {
			if n.ID != id {
				out = append(out, n)
			}
		}
		s.Nodes = out
	})
}
