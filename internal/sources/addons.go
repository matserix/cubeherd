package sources

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AddonHit is a search result for a mod or plugin.
type AddonHit struct {
	Source    string
	Ref       string // slug (Modrinth/CurseForge) or resource id (Spigot)
	Title     string
	Desc      string
	Downloads int64
	IconURL   string
}

// AddonQuery describes what the server can load.
type AddonQuery struct {
	Text    string
	Plugins bool   // Bukkit/Paper plugins instead of mods
	Loader  string // fabric | forge | neoforge | quilt (mods only)
	MC      string // Minecraft version; empty = any
}

var pluginLoaders = []string{"paper", "purpur", "folia", "spigot", "bukkit"}

func (c *Client) ModrinthAddons(ctx context.Context, q AddonQuery) ([]AddonHit, error) {
	var facets [][]string
	if q.Plugins {
		facets = append(facets, []string{"project_type:plugin"})
		var cats []string
		for _, l := range pluginLoaders {
			cats = append(cats, "categories:"+l)
		}
		facets = append(facets, cats)
	} else {
		facets = append(facets, []string{"project_type:mod"}, []string{"categories:" + q.Loader},
			[]string{"server_side:required", "server_side:optional"})
	}
	if q.MC != "" && IsVersion(q.MC) {
		facets = append(facets, []string{"versions:" + q.MC})
	}
	fb, _ := json.Marshal(facets)
	u := url.Values{"query": {q.Text}, "limit": {"15"}, "facets": {string(fb)}}
	if q.Text == "" {
		u.Set("index", "downloads")
	}
	v, err := c.memo("mr:addons:"+u.Encode(), 10*time.Minute, func() (any, error) {
		var res struct {
			Hits []struct {
				Slug, Title, Description string
				Downloads                int64
				IconURL                  string `json:"icon_url"`
			}
		}
		if err := c.getJSON(ctx, "https://api.modrinth.com/v2/search?"+u.Encode(), nil, &res); err != nil {
			return nil, err
		}
		out := make([]AddonHit, 0, len(res.Hits))
		for _, h := range res.Hits {
			out = append(out, AddonHit{Source: "modrinth", Ref: h.Slug, Title: h.Title, Desc: h.Description, Downloads: h.Downloads, IconURL: h.IconURL})
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]AddonHit), nil
}

var cfLoaderType = map[string]string{"forge": "1", "fabric": "4", "quilt": "5", "neoforge": "6"}

func (c *Client) CurseForgeAddons(ctx context.Context, q AddonQuery) ([]AddonHit, error) {
	hdr, err := c.cfHeader()
	if err != nil {
		return nil, err
	}
	u := url.Values{"gameId": {"432"}, "searchFilter": {q.Text}, "sortField": {"2"}, "sortOrder": {"desc"}, "pageSize": {"15"}}
	if q.Plugins {
		u.Set("classId", "5")
	} else {
		u.Set("classId", "6")
		if t := cfLoaderType[q.Loader]; t != "" {
			u.Set("modLoaderType", t)
		}
	}
	if q.MC != "" && IsVersion(q.MC) {
		u.Set("gameVersion", q.MC)
	}
	v, err := c.memo("cf:addons:"+u.Encode(), 10*time.Minute, func() (any, error) {
		var res struct {
			Data []struct {
				Slug, Name, Summary string
				DownloadCount       float64
				Logo                *struct {
					ThumbnailURL string `json:"thumbnailUrl"`
				}
			}
		}
		if err := c.getJSON(ctx, cfBase+"/mods/search?"+u.Encode(), hdr, &res); err != nil {
			return nil, err
		}
		out := make([]AddonHit, 0, len(res.Data))
		for _, d := range res.Data {
			h := AddonHit{Source: "curseforge", Ref: d.Slug, Title: d.Name, Desc: d.Summary, Downloads: int64(d.DownloadCount)}
			if d.Logo != nil {
				h.IconURL = d.Logo.ThumbnailURL
			}
			out = append(out, h)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]AddonHit), nil
}

// SpigetAddons searches SpigotMC. Premium and externally hosted resources are skipped,
// because itzg cannot download them automatically.
func (c *Client) SpigetAddons(ctx context.Context, q AddonQuery) ([]AddonHit, error) {
	text := strings.TrimSpace(q.Text)
	endpoint := "https://api.spiget.org/v2/resources/free?size=15&sort=-downloads"
	if text != "" {
		endpoint = "https://api.spiget.org/v2/search/resources/" + url.PathEscape(text) + "?field=name&size=25&sort=-downloads"
	}
	// Spiget is often slow; give up early instead of blocking the search box.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	v, err := c.memo("spiget:"+endpoint, 10*time.Minute, func() (any, error) {
		var res []struct {
			ID        int64
			Name, Tag string
			Downloads int64
			Premium   bool
			External  bool
			File      struct{ Type string }
			Icon      struct{ URL string }
		}
		var err error
		for attempt := 0; attempt < 2; attempt++ { // Spiget answers 522 now and then
			if err = c.getJSON(ctx, endpoint, nil, &res); err == nil {
				break
			}
		}
		if err != nil {
			return nil, err
		}
		out := []AddonHit{}
		for _, r := range res {
			if r.Premium || r.External || r.File.Type != ".jar" {
				continue
			}
			h := AddonHit{Source: "spiget", Ref: strconv.FormatInt(r.ID, 10), Title: r.Name, Desc: r.Tag, Downloads: r.Downloads}
			if r.Icon.URL != "" {
				h.IconURL = "https://www.spigotmc.org/" + r.Icon.URL
			}
			out = append(out, h)
			if len(out) == 15 {
				break
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]AddonHit), nil
}
