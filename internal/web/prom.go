package web

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"cubeherd/internal/access"
	"cubeherd/internal/api"
	"cubeherd/internal/config"
	"cubeherd/internal/nodes"
)

// The Prometheus endpoint: live values in the text exposition format, read with a dedicated bearer token. It is
// independent of the user accounts (it reads everything) and of the recorder (it uses the recorder's latest sample
// when there is one and live readings otherwise).

func scrapeHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

type promWriter struct {
	b      bytes.Buffer
	helped map[string]bool
}

func (p *promWriter) sample(name, help, typ string, v float64, labels ...string) {
	if p.helped == nil {
		p.helped = map[string]bool{}
	}
	if !p.helped[name] {
		p.helped[name] = true
		fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	p.b.WriteString(name)
	if len(labels) > 0 {
		p.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				p.b.WriteByte(',')
			}
			fmt.Fprintf(&p.b, `%s="%s"`, labels[i], promEscape(labels[i+1]))
		}
		p.b.WriteByte('}')
	}
	fmt.Fprintf(&p.b, " %s\n", trimFloat(v))
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.6f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func promEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func val(p *float64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

// metricsEndpoint serves /metrics. 404 while no token is set, 401 for a wrong one.
func (a *App) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	want := a.Cfg.Get().ScrapeTokenHash
	if want == "" {
		http.NotFound(w, r)
		return
	}
	if d := a.lim.Wait(r); d > 0 {
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(scrapeHash(tok)), []byte(want)) != 1 {
		a.lim.Fail(r)
		w.Header().Set("WWW-Authenticate", `Bearer realm="cubeherd"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	a.lim.Reset(r)
	var p promWriter
	p.sample("cubeherd_build_info", "Cubeherd version.", "gauge", 1, "version", Version)
	for _, n := range a.Reg.Refresh(r.Context(), false, 5*time.Second) {
		up := 0.0
		if n.Status.State != nodes.Offline {
			up = 1
		}
		p.sample("cubeherd_node_up", "1 when the dashboard can reach the node.", "gauge", up, "node", n.Name)
		if up == 0 {
			continue
		}
		if n.Host.CPUPercent >= 0 {
			p.sample("cubeherd_node_cpu_ratio", "CPU use of the node host, 0 to 1.", "gauge", n.Host.CPUPercent/100, "node", n.Name)
		}
		p.sample("cubeherd_node_memory_used_bytes", "Memory in use on the node host.", "gauge", float64(n.Host.MemUsed), "node", n.Name)
		p.sample("cubeherd_node_memory_total_bytes", "Memory of the node host.", "gauge", float64(n.Host.MemTotal), "node", n.Name)
		p.sample("cubeherd_node_disk_used_bytes", "Disk space in use in the data directory of the node.", "gauge", float64(n.Host.DiskUsed), "node", n.Name)
		p.sample("cubeherd_node_disk_total_bytes", "Disk space of the file system of the node's data directory.", "gauge", float64(n.Host.DiskTotal), "node", n.Name)
		latest, _ := n.Node.MetricsLatest(r.Context())
		live := map[string]api.View{}
		if len(latest) == 0 { // no recorder: read the figures now, as the overview does
			if vs, err := n.Node.Views(r.Context(), true); err == nil {
				for _, v := range vs {
					live[v.S.ID] = v
				}
			}
		}
		views := append([]api.View(nil), n.Views...)
		sort.Slice(views, func(i, j int) bool { return views[i].S.ID < views[j].S.ID })
		for _, v := range views {
			l := []string{"node", n.Name, "server", v.S.Name}
			upv := 0.0
			if v.Status.Up() {
				upv = 1
			}
			p.sample("cubeherd_server_up", "1 when the server's container is running.", "gauge", upv, l...)
			if lv, ok := live[v.S.ID]; ok && lv.Status.Live() {
				p.sample("cubeherd_server_memory_used_bytes", "Memory used by the server's container.", "gauge", float64(lv.MemUsed), l...)
				p.sample("cubeherd_server_players_online", "Players online.", "gauge", float64(len(lv.Online)), l...)
				p.sample("cubeherd_server_players_max", "Player limit.", "gauge", float64(lv.MaxPlayers), l...)
			}
			if pt, ok := latest[v.S.ID]; ok {
				if x, ok := val(pt.CPU); ok {
					p.sample("cubeherd_server_cpu_ratio", "CPU use of the server's container as a share of the host, 0 to 1.", "gauge", x/100, l...)
				}
				if x, ok := val(pt.Mem); ok {
					p.sample("cubeherd_server_memory_used_bytes", "Memory used by the server's container.", "gauge", x, l...)
				}
				if x, ok := val(pt.Players); ok {
					p.sample("cubeherd_server_players_online", "Players online.", "gauge", x, l...)
				}
				if x, ok := val(pt.TPS); ok {
					p.sample("cubeherd_server_tps", "Ticks per second (from the last recorded sample).", "gauge", x, l...)
				}
				if x, ok := val(pt.MSPT); ok {
					p.sample("cubeherd_server_mspt_milliseconds", "Milliseconds per tick (from the last recorded sample).", "gauge", x, l...)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(p.b.Bytes())
}

// ---------------- Token management (settings) ----------------

// prometheusToken creates or rotates the scrape token and shows it once; "off" removes it.
func (a *App) prometheusToken(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("op") == "off" {
		if err := a.Cfg.Update(func(s *config.Settings) { s.ScrapeTokenHash = "" }); err != nil {
			a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
			return
		}
		redirect(w, r, "/settings#prometheus")
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	tok := "cmp_" + base64.RawURLEncoding.EncodeToString(b)
	if err := a.Cfg.Update(func(s *config.Settings) { s.ScrapeTokenHash = scrapeHash(tok) }); err != nil {
		a.fail(w, "Speichern fehlgeschlagen: %s", err.Error())
		return
	}
	scheme, host, _ := strings.Cut(a.publicBase(r), "://")
	a.partial(w, "settings", "promToken", struct{ Token, Scheme, Host string }{tok, scheme, host})
}

var _ = access.OK
