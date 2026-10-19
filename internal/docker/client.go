// Package docker is a minimal client for the Docker Engine API. It covers only
// what Cubeherd needs and talks to the socket with net/http.
package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiVersion 1.44 corresponds to Docker 25 and is the minimum that Docker 29+ engines accept.
const apiVersion = "v1.44"

type Client struct {
	hc     *http.Client // requests with a deadline
	stream *http.Client // long-lived streams (logs, stats)
	base   string
	Host   string
}

// ErrNotFound is returned when a container does not exist.
var ErrNotFound = errors.New("not found")

// New resolves the Docker socket from DOCKER_HOST or well-known locations.
func New() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		candidates := []string{"/var/run/docker.sock"}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".docker/run/docker.sock"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				host = "unix://" + c
				break
			}
		}
	}
	if host == "" {
		return nil, errors.New("no Docker socket found; set DOCKER_HOST")
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST: %w", err)
	}
	var dial func(ctx context.Context, _, _ string) (net.Conn, error)
	base := "http://docker/" + apiVersion
	switch u.Scheme {
	case "unix":
		path := u.Path
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	case "tcp", "http":
		base = "http://" + u.Host + "/" + apiVersion
	default:
		return nil, fmt.Errorf("DOCKER_HOST scheme %q is not supported", u.Scheme)
	}
	tr := &http.Transport{DialContext: dial, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	return &Client{
		hc:     &http.Client{Transport: tr, Timeout: 30 * time.Second},
		stream: &http.Client{Transport: tr},
		base:   base,
		Host:   host,
	}, nil
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return nil, fmt.Errorf("docker: %s", e.Message)
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, c.hc, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(ctx context.Context, path string) error {
	resp, err := c.do(ctx, c.hc, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// ---- Types (only the fields Cubeherd reads) ----

type Container struct {
	ID     string `json:"Id"`
	Names  []string
	Image  string
	State  string
	Status string
	Labels map[string]string
	Ports  []struct {
		PrivatePort int
		PublicPort  int
		Type        string
	}
}

func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:12]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

type ContainerJSON struct {
	ID           string `json:"Id"`
	Name         string
	RestartCount int
	State        struct {
		Status     string
		Running    bool
		Paused     bool
		Restarting bool
		OOMKilled  bool
		ExitCode   int
		StartedAt  time.Time
		FinishedAt time.Time
		Health     *struct{ Status string }
	}
	Config struct {
		Image  string
		Env    []string
		Tty    bool
		Labels map[string]string
	}
	HostConfig struct {
		PortBindings  map[string][]struct{ HostIP, HostPort string }
		RestartPolicy struct{ Name string }
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
	}
}

// EnvMap returns the container environment as a map.
func (c *ContainerJSON) EnvMap() map[string]string {
	m := make(map[string]string, len(c.Config.Env))
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

type Info struct {
	NCPU            int
	MemTotal        uint64
	ServerVersion   string
	OperatingSystem string
	Architecture    string
}

// ---- Calls ----

func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, c.hc, http.MethodGet, "/_ping", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, c.getJSON(ctx, "/info", &i)
}

// List returns containers; filters follow the Docker filter syntax, e.g. {"label": {"cubeherd.id"}}.
func (c *Client) List(ctx context.Context, all bool, filters map[string][]string) ([]Container, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	if len(filters) > 0 {
		b, _ := json.Marshal(filters)
		q.Set("filters", string(b))
	}
	var out []Container
	return out, c.getJSON(ctx, "/containers/json?"+q.Encode(), &out)
}

func (c *Client) Inspect(ctx context.Context, id string) (*ContainerJSON, error) {
	var out ContainerJSON
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/start")
}

// Stop sends SIGTERM; itzg's runner forwards it as a clean "stop" to the server.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	resp, err := c.do(ctx, c.stream, http.MethodPost, fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(id), int(timeout.Seconds())), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	q := ""
	if force {
		q = "?force=1"
	}
	resp, err := c.do(ctx, c.hc, http.MethodDelete, "/containers/"+url.PathEscape(id)+q, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Logs streams log lines (stdout and stderr merged) until ctx ends or, without follow, the log ends.
// tail < 0 means all lines; since > 0 limits to lines after that Unix time.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail int, tty bool, fn func(line string)) error {
	return c.LogsSince(ctx, id, follow, tail, 0, tty, fn)
}

func (c *Client) LogsSince(ctx context.Context, id string, follow bool, tail int, since int64, tty bool, fn func(line string)) error {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"all"}}
	if tail >= 0 {
		q.Set("tail", fmt.Sprint(tail))
	}
	if since > 0 {
		q.Set("since", fmt.Sprint(since))
	}
	if follow {
		q.Set("follow", "1")
	}
	resp, err := c.do(ctx, c.stream, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r io.Reader = resp.Body
	if !tty {
		r = demux(resp.Body)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fn(sc.Text())
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// demux strips the 8-byte frame headers of non-TTY streams.
func demux(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		hdr := make([]byte, 8)
		for {
			if _, err := io.ReadFull(r, hdr); err != nil {
				pw.CloseWithError(err)
				return
			}
			n := int64(binary.BigEndian.Uint32(hdr[4:]))
			if _, err := io.CopyN(pw, r, n); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}

// Stats is one sample derived from the Docker stats endpoint.
type Stats struct {
	CPUPercent float64 // share of the whole host (0–100)
	MemUsed    uint64
	MemLimit   uint64
}

type rawStats struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  int    `json:"online_cpus"`
}

func (r rawStats) toStats() Stats {
	s := Stats{MemLimit: r.MemoryStats.Limit, MemUsed: r.MemoryStats.Usage}
	// Same calculation as `docker stats`: page cache does not count as used.
	if v, ok := r.MemoryStats.Stats["inactive_file"]; ok && v < s.MemUsed {
		s.MemUsed -= v
	} else if v, ok := r.MemoryStats.Stats["total_inactive_file"]; ok && v < s.MemUsed {
		s.MemUsed -= v
	}
	cpu := float64(r.CPUStats.CPUUsage.TotalUsage) - float64(r.PreCPUStats.CPUUsage.TotalUsage)
	sys := float64(r.CPUStats.SystemUsage) - float64(r.PreCPUStats.SystemUsage)
	if cpu > 0 && sys > 0 {
		s.CPUPercent = cpu / sys * 100
	}
	return s
}

// StatsOnce returns memory figures quickly; CPU is not meaningful in one-shot mode.
func (c *Client) StatsOnce(ctx context.Context, id string) (Stats, error) {
	var r rawStats
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", &r); err != nil {
		return Stats{}, err
	}
	return r.toStats(), nil
}

// StatsStream calls fn about once per second until ctx ends.
func (c *Client) StatsStream(ctx context.Context, id string, fn func(Stats)) error {
	resp, err := c.do(ctx, c.stream, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats?stream=true", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var r rawStats
		if err := dec.Decode(&r); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		fn(r.toStats())
	}
}

// Exec runs a command in the container and returns its combined output and exit code.
func (c *Client) Exec(ctx context.Context, id string, cmd ...string) (string, int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	resp, err := c.do(ctx, c.hc, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": cmd,
	})
	if err != nil {
		return "", -1, err
	}
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil {
		return "", -1, err
	}
	resp, err = c.do(ctx, c.stream, http.MethodPost, "/exec/"+created.ID+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return "", -1, err
	}
	out, err := io.ReadAll(io.LimitReader(demux(resp.Body), 1<<20))
	resp.Body.Close()
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return string(out), -1, err
	}
	var st struct{ ExitCode int }
	if err := c.getJSON(ctx, "/exec/"+created.ID+"/json", &st); err != nil {
		return string(out), -1, err
	}
	return string(out), st.ExitCode, nil
}

// Rename gives a container a new name.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/rename?name="+url.QueryEscape(name))
}

// ImageEnv returns the environment baked into an image.
func (c *Client) ImageEnv(ctx context.Context, image string) ([]string, error) {
	var out struct{ Config struct{ Env []string } }
	if err := c.getJSON(ctx, "/images/"+image+"/json", &out); err != nil {
		return nil, err
	}
	return out.Config.Env, nil
}
