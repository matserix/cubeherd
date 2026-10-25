// Package update looks up the newest Cubeherd release on GitHub. The lookup is triggered by page views
// (never by a timer) and its result is cached, so an idle dashboard makes no requests at all.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	api       = "https://api.github.com/repos/matserix/cubeherd/releases/latest"
	ttl       = 6 * time.Hour
	retryWait = 30 * time.Minute // after a failed lookup
)

// Release is the newest published version; Notes are the GitHub release notes (Markdown).
type Release struct {
	Tag   string
	Name  string
	Notes string
	URL   string
}

type Checker struct {
	hc      *http.Client
	url     string
	mu      sync.Mutex
	rel     *Release
	next    time.Time // earliest time of the next lookup
	running bool
}

func New() *Checker { return NewWithURL(api) }

// NewWithURL reads the release from another endpoint with the same JSON shape (tests, mirrors).
func NewWithURL(url string) *Checker {
	return &Checker{hc: &http.Client{Timeout: 10 * time.Second}, url: url}
}

// Available returns the newest release if it is newer than current, and starts a refresh in the
// background when the cache is stale. It never blocks on the network.
func (c *Checker) Available(current string) *Release {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running && time.Now().After(c.next) {
		c.running = true
		go c.refresh()
	}
	if c.rel != nil && Newer(c.rel.Tag, current) {
		r := *c.rel
		return &r
	}
	return nil
}

func (c *Checker) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rel, err := c.fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	if err != nil {
		c.next = time.Now().Add(retryWait)
		return
	}
	c.rel, c.next = rel, time.Now().Add(ttl)
}

func (c *Checker) fetch(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cubeherd (update check)")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: %s", resp.Status)
	}
	var v struct {
		Tag        string `json:"tag_name"`
		Name       string `json:"name"`
		Body       string `json:"body"`
		URL        string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return nil, err
	}
	if v.Tag == "" || v.Draft || v.Prerelease {
		return nil, fmt.Errorf("github: no usable release")
	}
	return &Release{Tag: v.Tag, Name: v.Name, Notes: strings.TrimSpace(v.Body), URL: v.URL}, nil
}

// Newer reports whether version a is higher than b ("v" prefixes and pre-release suffixes are ignored).
// Anything that is not a dotted number, such as "dev", is never newer and never older.
func Newer(a, b string) bool {
	pa, ok1 := parse(a)
	pb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) (out [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
