// Package remote talks to a Cubeherd agent over HTTPS with a pinned certificate fingerprint.
package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cubeherd/internal/api"
	"cubeherd/internal/docker"
	"cubeherd/internal/host"
	"cubeherd/internal/manager"
	"cubeherd/internal/server"
)

var (
	ErrUnauthorized = errors.New("the agent rejected the token")
	ErrAPIVersion   = errors.New("the agent speaks a different API version")
	ErrFingerprint  = errors.New("the agent's certificate does not match the pinned fingerprint")
)

// Client is a connection to one agent.
type Client struct {
	base    string
	token   string
	hc      *http.Client
	Timeout time.Duration // per request unless the context is shorter; 5 s by default

	mu     sync.Mutex
	info   api.Info
	infoAt time.Time
}

// New returns a client for the agent at addr ("host:port" or "https://host:port").
// fingerprint is the pinned SHA-256 of the agent's certificate (see agent.Fingerprint).
func New(addr, token, fingerprint string) (*Client, error) {
	base, err := baseURL(addr)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, token: token, hc: pinned(fingerprint), Timeout: 5 * time.Second}, nil
}

func baseURL(addr string) (string, error) {
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return "", fmt.Errorf("invalid agent address %q (expected https://host:port)", addr)
	}
	return "https://" + u.Host, nil
}

// pinned builds an HTTP client that skips certificate-authority checks and instead requires the
// server's leaf certificate to match the fingerprint. An empty fingerprint accepts any certificate
// and is only meant for Probe.
func pinned(fingerprint string) *http.Client {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by the fingerprint check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if fingerprint == "" {
				return nil
			}
			if len(raw) == 0 || !strings.EqualFold(api.Fingerprint(raw[0]), fingerprint) {
				return ErrFingerprint
			}
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:     cfg,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     60 * time.Second,
	}}
}

// Probe connects to addr without sending any credentials and returns the certificate fingerprint the
// agent presents. The dashboard shows it when a node is added; the operator confirms it (trust on first use).
func Probe(ctx context.Context, addr string) (string, error) {
	base, err := baseURL(addr)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(base)
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("the agent presented no certificate")
	}
	return api.Fingerprint(certs[0].Raw), nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	if _, ok := ctx.Deadline(); !ok && c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	c.sign(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, ErrFingerprint) {
			return ErrFingerprint
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeErr(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sign adds the bearer token and the API major.
func (c *Client) sign(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(api.Header, strconv.Itoa(api.Version))
}

func decodeErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e api.Error
	_ = json.NewDecoder(bytes.NewReader(b)).Decode(&e)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case e.Code == "apiversion":
		return fmt.Errorf("%w: %s", ErrAPIVersion, e.Error)
	case e.Code == "busy":
		return manager.ErrBusy
	case e.Code == "notfound":
		return server.ErrNotFound
	case e.Code == "dockernotfound":
		return docker.ErrNotFound
	case e.Error != "":
		return errors.New(e.Error)
	}
	return fmt.Errorf("agent answered %s", resp.Status)
}

// Info identifies the agent. It doubles as the connection test.
func (c *Client) Info(ctx context.Context) (api.Info, error) {
	var in api.Info
	return in, c.do(ctx, http.MethodGet, "/v1/info", nil, &in)
}

// HostStats reports the agent host's resources; it returns zero values when the agent cannot be reached.
func (c *Client) HostStats(ctx context.Context) host.Stats {
	var s host.Stats
	if err := c.do(ctx, http.MethodGet, "/v1/host", nil, &s); err != nil {
		return host.Stats{CPUPercent: -1}
	}
	return s
}
