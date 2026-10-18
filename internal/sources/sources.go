// Package sources queries Mojang, Modrinth and CurseForge. Requests happen only
// on user actions and results are cached in memory.
package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const userAgent = "forgecraft/0.1 (itzg dashboard)"

type Client struct {
	hc    *http.Client
	cfKey func() string
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at  time.Time
	val any
}

func New(cfKey func() string) *Client {
	return &Client{hc: &http.Client{Timeout: 12 * time.Second}, cfKey: cfKey, cache: map[string]cached{}}
}

func (c *Client) memo(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	if v, ok := c.cache[key]; ok && time.Since(v.at) < ttl {
		c.mu.Unlock()
		return v.val, nil
	}
	c.mu.Unlock()
	v, err := fn()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.cache) > 500 {
		c.cache = map[string]cached{}
	}
	c.cache[key] = cached{time.Now(), v}
	c.mu.Unlock()
	return v, nil
}

func (c *Client) getJSON(ctx context.Context, u string, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", req.URL.Host, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

var ErrUnauthorized = errors.New("API-Key ungültig oder fehlt")

// ---------------- Mojang ----------------

type mcManifest struct {
	Latest   struct{ Release, Snapshot string }
	Versions []struct {
		ID   string
		Type string
		URL  string
	}
}

func (c *Client) manifest(ctx context.Context) (*mcManifest, error) {
	v, err := c.memo("mojang:manifest", time.Hour, func() (any, error) {
		var m mcManifest
		err := c.getJSON(ctx, "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json", nil, &m)
		return &m, err
	})
	if err != nil {
		return nil, err
	}
	return v.(*mcManifest), nil
}

// Releases returns Minecraft release versions, newest first.
func (c *Client) Releases(ctx context.Context) ([]string, error) {
	m, err := c.manifest(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range m.Versions {
		if v.Type == "release" {
			out = append(out, v.ID)
		}
	}
	return out, nil
}

// JavaTag returns the itzg image tag for a Minecraft version, based on Mojang's metadata.
func (c *Client) JavaTag(ctx context.Context, mc string) string {
	major := 0
	if m, err := c.manifest(ctx); err == nil {
		for _, v := range m.Versions {
			if v.ID != mc {
				continue
			}
			res, err := c.memo("mojang:java:"+mc, 24*time.Hour, func() (any, error) {
				var meta struct {
					JavaVersion struct{ MajorVersion int } `json:"javaVersion"`
				}
				err := c.getJSON(ctx, v.URL, nil, &meta)
				return meta.JavaVersion.MajorVersion, err
			})
			if err == nil {
				major = res.(int)
			}
			break
		}
	}
	if major == 0 {
		major = guessJava(mc)
	}
	return tagFor(major)
}

func tagFor(major int) string {
	switch {
	case major <= 8:
		return "java8"
	case major <= 11:
		return "java11"
	case major <= 17:
		return "java17"
	case major <= 21:
		return "java21"
	}
	return "java" + strconv.Itoa(major)
}

// guessJava is the offline fallback when Mojang's metadata is unavailable.
func guessJava(mc string) int {
	parts := strings.Split(mc, ".")
	n := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		v, _ := strconv.Atoi(parts[i])
		return v
	}
	if n(0) >= 26 {
		return 25
	}
	minor, patch := n(1), n(2)
	switch {
	case minor > 20 || minor == 20 && patch >= 5:
		return 21
	case minor >= 17:
		return 17
	}
	return 8
}

// ---------------- Modpacks ----------------

type Pack struct {
	Source    string
	Slug      string
	ID        string
	Title     string
	Desc      string
	Downloads int64
	IconURL   string
	Loader    string
	MC        string
}

type PackVersion struct {
	ID     string
	Name   string
	MC     string
	Loader string
	Date   time.Time
}

var loaderNames = map[string]string{"fabric": "Fabric", "forge": "Forge", "neoforge": "NeoForge", "quilt": "Quilt"}

// Modrinth --------------------------------------------------------------

func (c *Client) ModrinthSearch(ctx context.Context, q string) ([]Pack, error) {
	v, err := c.memo("mr:search:"+q, 10*time.Minute, func() (any, error) {
		u := url.Values{"query": {q}, "limit": {"12"}, "facets": {`[["project_type:modpack"]]`}}
		if q == "" {
			u.Set("index", "downloads")
		}
		var res struct {
			Hits []struct {
				Slug, Title, Description string
				ProjectID                string `json:"project_id"`
				Downloads                int64
				IconURL                  string `json:"icon_url"`
				Categories               []string
				Versions                 []string
			}
		}
		if err := c.getJSON(ctx, "https://api.modrinth.com/v2/search?"+u.Encode(), nil, &res); err != nil {
			return nil, err
		}
		out := make([]Pack, 0, len(res.Hits))
		for _, h := range res.Hits {
			p := Pack{Source: "modrinth", Slug: h.Slug, ID: h.ProjectID, Title: h.Title, Desc: h.Description, Downloads: h.Downloads, IconURL: h.IconURL}
			for _, cat := range h.Categories {
				if n, ok := loaderNames[cat]; ok {
					p.Loader = n
					break
				}
			}
			if len(h.Versions) > 0 {
				p.MC = latestMC(h.Versions)
			}
			out = append(out, p)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]Pack), nil
}

func (c *Client) ModrinthVersions(ctx context.Context, slug string) ([]PackVersion, error) {
	v, err := c.memo("mr:versions:"+slug, 10*time.Minute, func() (any, error) {
		var res []struct {
			ID            string
			VersionNumber string   `json:"version_number"`
			GameVersions  []string `json:"game_versions"`
			Loaders       []string
			DatePublished time.Time `json:"date_published"`
		}
		if err := c.getJSON(ctx, "https://api.modrinth.com/v2/project/"+url.PathEscape(slug)+"/version", nil, &res); err != nil {
			return nil, err
		}
		out := make([]PackVersion, 0, 25)
		for _, r := range res {
			pv := PackVersion{ID: r.ID, Name: r.VersionNumber, Date: r.DatePublished}
			if len(r.GameVersions) > 0 {
				pv.MC = latestMC(r.GameVersions)
			}
			for _, l := range r.Loaders {
				if n, ok := loaderNames[l]; ok {
					pv.Loader = n
					break
				}
			}
			out = append(out, pv)
			if len(out) == 25 {
				break
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]PackVersion), nil
}

// CurseForge ------------------------------------------------------------

const cfBase = "https://api.curseforge.com/v1"

var cfLoaders = map[int]string{1: "Forge", 4: "Fabric", 5: "Quilt", 6: "NeoForge"}

func (c *Client) cfHeader() (map[string]string, error) {
	k := c.cfKey()
	if k == "" {
		return nil, ErrUnauthorized
	}
	return map[string]string{"x-api-key": k}, nil
}

func (c *Client) CurseForgeCheck(ctx context.Context, key string) error {
	var res any
	return c.getJSON(ctx, cfBase+"/games/432", map[string]string{"x-api-key": key}, &res)
}

func (c *Client) CurseForgeSearch(ctx context.Context, q string) ([]Pack, error) {
	hdr, err := c.cfHeader()
	if err != nil {
		return nil, err
	}
	v, err := c.memo("cf:search:"+q, 10*time.Minute, func() (any, error) {
		u := url.Values{"gameId": {"432"}, "classId": {"4471"}, "searchFilter": {q}, "sortField": {"2"}, "sortOrder": {"desc"}, "pageSize": {"12"}}
		var res struct {
			Data []struct {
				ID            int64
				Slug, Name    string
				Summary       string
				DownloadCount float64
				Logo          *struct {
					ThumbnailURL string `json:"thumbnailUrl"`
				}
				LatestFiles []struct {
					GameVersion string
					ModLoader   int
				} `json:"latestFilesIndexes"`
			}
		}
		if err := c.getJSON(ctx, cfBase+"/mods/search?"+u.Encode(), hdr, &res); err != nil {
			return nil, err
		}
		out := make([]Pack, 0, len(res.Data))
		for _, d := range res.Data {
			p := Pack{Source: "curseforge", Slug: d.Slug, ID: strconv.FormatInt(d.ID, 10), Title: d.Name, Desc: d.Summary, Downloads: int64(d.DownloadCount)}
			if d.Logo != nil {
				p.IconURL = d.Logo.ThumbnailURL
			}
			var mcs []string
			for _, f := range d.LatestFiles {
				mcs = append(mcs, f.GameVersion)
				if p.Loader == "" {
					p.Loader = cfLoaders[f.ModLoader]
				}
			}
			p.MC = latestMC(mcs)
			out = append(out, p)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]Pack), nil
}

func (c *Client) CurseForgeVersions(ctx context.Context, projectID string) ([]PackVersion, error) {
	hdr, err := c.cfHeader()
	if err != nil {
		return nil, err
	}
	v, err := c.memo("cf:files:"+projectID, 10*time.Minute, func() (any, error) {
		var res struct {
			Data []struct {
				ID           int64
				DisplayName  string
				GameVersions []string
				FileDate     time.Time
			}
		}
		if err := c.getJSON(ctx, cfBase+"/mods/"+url.PathEscape(projectID)+"/files?pageSize=25", hdr, &res); err != nil {
			return nil, err
		}
		out := make([]PackVersion, 0, len(res.Data))
		for _, d := range res.Data {
			pv := PackVersion{ID: strconv.FormatInt(d.ID, 10), Name: d.DisplayName, Date: d.FileDate, MC: latestMC(d.GameVersions)}
			for _, g := range d.GameVersions {
				if n, ok := loaderNames[strings.ToLower(g)]; ok {
					pv.Loader = n
				}
			}
			out = append(out, pv)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]PackVersion), nil
}

var mcVersion = regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)

// latestMC picks the highest Minecraft version from a list that may contain other tags.
func latestMC(vs []string) string {
	best := ""
	for _, v := range vs {
		if !mcVersion.MatchString(v) {
			continue
		}
		if best == "" || CompareMC(v, best) > 0 {
			best = v
		}
	}
	return best
}

// IsVersion reports whether v is a numeric Minecraft version (not LATEST/SNAPSHOT).
func IsVersion(v string) bool { return mcVersion.MatchString(v) }

// CompareMC compares dotted Minecraft versions numerically.
func CompareMC(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
