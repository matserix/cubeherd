package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/docker"
	"cubeherd/internal/node"
	"cubeherd/internal/server"
)

var (
	_ node.Servers = (*Client)(nil)
	_ node.Console = (*Client)(nil)
	_ node.Live    = (*Client)(nil)
	_ node.Host    = (*Client)(nil)
)

// long bounds requests that wait for the agent to do real work (create, update, uninstall).
const long = 10 * time.Minute

func esc(id string) string { return url.PathEscape(id) }

func flag(name string, on bool) string {
	if on {
		return name + "=1"
	}
	return name + "=0"
}

// send performs a request with an optional JSON body and decodes an optional JSON reply.
func (c *Client) send(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return c.do(ctx, method, path, body, out)
}

func withTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (c *Client) quick(method, path string, in, out any) error {
	ctx, cancel := withTimeout(c.Timeout)
	defer cancel()
	return c.send(ctx, method, path, in, out)
}

func (c *Client) slow(method, path string, in, out any) error {
	ctx, cancel := withTimeout(long)
	defer cancel()
	return c.send(ctx, method, path, in, out)
}

// ---- Servers ----

func (c *Client) Server(id string) (*server.Server, error) {
	var s server.Server
	if err := c.quick(http.MethodGet, "/v1/servers/"+esc(id), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) ServerExists(id string) bool { _, err := c.Server(id); return err == nil }

// Views lists every server on the agent.
func (c *Client) Views(ctx context.Context, withStats bool) ([]api.View, error) {
	var v []api.View
	return v, c.send(ctx, http.MethodGet, "/v1/servers?"+flag("stats", withStats), nil, &v)
}

// View returns one server's view; when the agent cannot answer, the view carries the error.
func (c *Client) View(ctx context.Context, s *server.Server, withStats bool) api.View {
	var v api.View
	if err := c.send(ctx, http.MethodGet, "/v1/servers/"+esc(s.ID)+"/view?"+flag("stats", withStats), nil, &v); err != nil {
		return api.View{S: s, OpError: err.Error()}
	}
	return v
}

func (c *Client) Busy(id string) bool {
	var r struct{ Busy bool }
	return c.quick(http.MethodGet, "/v1/servers/"+esc(id)+"/busy", nil, &r) == nil && r.Busy
}

func (c *Client) Forget(id string) {
	_ = c.quick(http.MethodPost, "/v1/servers/"+esc(id)+"/forget", nil, nil)
}

func (c *Client) DiskUsage(s *server.Server) int64 {
	var r struct{ Bytes int64 }
	if c.quick(http.MethodGet, "/v1/servers/"+esc(s.ID)+"/disk", nil, &r) != nil {
		return 0
	}
	return r.Bytes
}

func (c *Client) NextPort(excludeID string) int {
	var r struct{ Port int }
	_ = c.quick(http.MethodGet, "/v1/ports/next?exclude="+url.QueryEscape(excludeID), nil, &r)
	return r.Port
}

func (c *Client) PortTaken(port int, excludeID string) bool {
	var r struct{ Taken bool }
	q := "port=" + strconv.Itoa(port) + "&exclude=" + url.QueryEscape(excludeID)
	return c.quick(http.MethodGet, "/v1/ports/taken?"+q, nil, &r) == nil && r.Taken
}

// store sends a definition and replaces *s with what the agent saved, as the local manager does in place.
func (c *Client) store(method, path string, s *server.Server) error {
	var got server.Server
	if err := c.slow(method, path, s, &got); err != nil {
		return err
	}
	*s = got
	return nil
}

func (c *Client) Create(s *server.Server) error { return c.store(http.MethodPost, "/v1/servers", s) }

func (c *Client) Start(id string) error {
	return c.quick(http.MethodPost, "/v1/servers/"+esc(id)+"/start", nil, nil)
}
func (c *Client) Stop(id string) error {
	return c.quick(http.MethodPost, "/v1/servers/"+esc(id)+"/stop", nil, nil)
}
func (c *Client) Restart(id string) error {
	return c.quick(http.MethodPost, "/v1/servers/"+esc(id)+"/restart", nil, nil)
}

func (c *Client) Apply(s *server.Server, pull, restart bool) error {
	return c.store(http.MethodPut, "/v1/servers/"+esc(s.ID)+"?"+flag("pull", pull)+"&"+flag("restart", restart), s)
}

func (c *Client) ApplyUpdate(s *server.Server, pull, restart, backupFirst bool) error {
	q := flag("pull", pull) + "&" + flag("restart", restart) + "&" + flag("backup", backupFirst)
	return c.store(http.MethodPost, "/v1/servers/"+esc(s.ID)+"/update?"+q, s)
}

func (c *Client) Uninstall(id, mode string) error {
	return c.slow(http.MethodDelete, "/v1/servers/"+esc(id)+"?mode="+url.QueryEscape(mode), nil, nil)
}

func (c *Client) Discover(ctx context.Context) []api.Unmanaged {
	var l []api.Unmanaged
	if c.send(ctx, http.MethodGet, "/v1/discover", nil, &l) != nil {
		return nil
	}
	return l
}

func (c *Client) Import(ctx context.Context, containerID, name string) (*server.Server, error) {
	var s server.Server
	if err := c.send(ctx, http.MethodPost, "/v1/import", api.ImportRequest{ContainerID: containerID, Name: name}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ---- Console ----

func (c *Client) RCON(ctx context.Context, s *server.Server, cmd string) (string, error) {
	var r api.RCONReply
	err := c.send(ctx, http.MethodPost, "/v1/servers/"+esc(s.ID)+"/rcon", api.RCONRequest{Cmd: cmd}, &r)
	return r.Out, err
}

func (c *Client) Players(ctx context.Context, s *server.Server) ([]string, int, error) {
	var p api.Players
	err := c.send(ctx, http.MethodGet, "/v1/servers/"+esc(s.ID)+"/players", nil, &p)
	return p.Names, p.Max, err
}

func (c *Client) Perf(ctx context.Context, s *server.Server) (server.PerfSample, error) {
	var p server.PerfSample
	return p, c.send(ctx, http.MethodGet, "/v1/servers/"+esc(s.ID)+"/perf", nil, &p)
}

// ---- Live ----

func (c *Client) Inspect(ctx context.Context, s *server.Server) (*docker.ContainerJSON, error) {
	var j docker.ContainerJSON
	if err := c.send(ctx, http.MethodGet, "/v1/servers/"+esc(s.ID)+"/container", nil, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// stream follows a newline-delimited JSON response until it ends or ctx is cancelled. It has no
// overall timeout: only ctx ends it. Values that fail to decode end the stream with an error.
func (c *Client) stream(ctx context.Context, path string, each func(dec *json.Decoder) error) error {
	resp, err := c.raw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for dec.More() {
		if err := each(dec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) StatsStream(ctx context.Context, s *server.Server, fn func(docker.Stats)) error {
	return c.stream(ctx, "/v1/servers/"+esc(s.ID)+"/stats", func(d *json.Decoder) error {
		var st docker.Stats
		if err := d.Decode(&st); err != nil {
			return err
		}
		fn(st)
		return nil
	})
}

func (c *Client) LogsSince(ctx context.Context, s *server.Server, follow bool, tail int, since int64, fn func(line string)) error {
	q := fmt.Sprintf("?%s&tail=%d&since=%d", flag("follow", follow), tail, since)
	return c.stream(ctx, "/v1/servers/"+esc(s.ID)+"/logs"+q, lineReader(fn))
}

// FollowOps keeps (re)connecting to the action output stream until ctx ends, like the local loop does.
func (c *Client) FollowOps(ctx context.Context, id string, fn func(line string)) {
	for ctx.Err() == nil {
		_ = c.stream(ctx, "/v1/servers/"+esc(id)+"/ops", lineReader(fn))
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func lineReader(fn func(string)) func(*json.Decoder) error {
	return func(d *json.Decoder) error {
		var l string
		if err := d.Decode(&l); err != nil {
			return err
		}
		fn(l)
		return nil
	}
}

// ---- Host ----

func (c *Client) DockerInfo(ctx context.Context) (docker.Info, error) {
	var in docker.Info
	return in, c.send(ctx, http.MethodGet, "/v1/docker", nil, &in)
}

// cachedInfo returns the agent's identity, refreshed at most once a minute. Loc is called for every
// rendered date, so it must not cost a request each time. A stale or empty value is returned when the
// agent cannot be reached.
func (c *Client) cachedInfo() api.Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.infoAt.IsZero() && time.Since(c.infoAt) < time.Minute {
		return c.info
	}
	ctx, cancel := withTimeout(c.Timeout)
	defer cancel()
	if in, err := c.Info(ctx); err == nil {
		c.info = in
	}
	c.infoAt = time.Now()
	return c.info
}

func (c *Client) DockerHost() string { return c.cachedInfo().DockerHost }

// Loc is the agent's display time zone; the local zone when it is unknown.
func (c *Client) Loc() *time.Location {
	if tz := c.cachedInfo().Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}
