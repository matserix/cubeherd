package web

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"forgecraft/internal/server"
)

var funcs = template.FuncMap{
	"static":   staticURL,
	"typeName": server.TypeName,
	"types":    func() any { return server.Types },
	"de":       de,
	"gb":       gb,
	"gbOf":     func(n int) string { return strconv.Itoa(n) + " GB" },
	"pct":      pct,
	"level":    level,
	"since":    since,
	"avatar":   avatar,
	"num":      num,
	"add":      func(a, b int) int { return a + b },
	"join":     strings.Join,
	"lower":    strings.ToLower,
	"eq2":      func(a, b, c string) bool { return a == b || a == c },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"list": func(v ...string) []string { return v },
	"addonSrcName": func(s string) string {
		return map[string]string{"modrinth": "Modrinth", "curseforge": "CurseForge", "spiget": "Spigot"}[s]
	},
	// jsonVals builds an hx-vals object from key/value pairs.
	"jsonVals": func(kv ...string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		b, _ := json.Marshal(m)
		return string(b)
	},
	"dt": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		now := time.Now()
		switch {
		case t.YearDay() == now.YearDay() && t.Year() == now.Year():
			return "Heute, " + t.Format("15:04")
		case t.YearDay() == now.AddDate(0, 0, -1).YearDay() && t.Year() == now.AddDate(0, 0, -1).Year():
			return "Gestern, " + t.Format("15:04")
		case t.YearDay() == now.AddDate(0, 0, 1).YearDay() && t.Year() == now.AddDate(0, 0, 1).Year():
			return "Morgen, " + t.Format("15:04")
		}
		return t.Format("02.01.2006, 15:04")
	},
	"mb": func(n int64) string {
		if n >= 1<<30 {
			return de(float64(n)/(1<<30)) + " GB"
		}
		return de(float64(n)/(1<<20)) + " MB"
	},
	"intervalName": func(i string) string {
		return map[string]string{"off": "Aus", "6h": "Alle 6 Stunden", "daily": "Täglich", "weekly": "Wöchentlich"}[i]
	},
	"keepOptions": func() []int { return []int{3, 5, 7, 14, 30} },
	"initial": func(s string) string {
		for _, r := range s {
			return string(r)
		}
		return "?"
	},
	"mulGB": func(n int) uint64 { return uint64(n) << 30 },
	// Rounded half up like Number.toFixed in app.js, so live and rendered values match.
	"heapGB": func(gb int, modded bool) string {
		return de(math.Round(float64(server.HeapMB(gb, modded))/1024*10) / 10)
	},
	"modeName": func(m string) string {
		return map[string]string{"survival": "Überleben", "creative": "Kreativ", "adventure": "Abenteuer", "spectator": "Zuschauer"}[m]
	},
	"diffName": func(d string) string {
		return map[string]string{"peaceful": "friedlich", "easy": "leicht", "normal": "normal", "hard": "schwer"}[d]
	},
	"srcName": func(s string) string {
		return map[string]string{server.KindModrinth: "Modrinth", server.KindCurseForge: "CurseForge"}[s]
	},
}

// de formats a float with one decimal and a German comma.
func de(f float64) string { return strings.Replace(strconv.FormatFloat(f, 'f', 1, 64), ".", ",", 1) }

// gb formats a byte count (any integer type) as gigabytes.
func gb(b any) string { return de(toF(b) / (1 << 30)) }

func pct(a, b any) float64 {
	x, y := toF(a), toF(b)
	if y <= 0 {
		return 0
	}
	p := x / y * 100
	if p > 100 {
		p = 100
	}
	return p
}

func toF(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func level(p float64) string {
	switch {
	case p > 85:
		return "crit"
	case p > 65:
		return "warn"
	}
	return "ok"
}

// num formats large counts like Modrinth downloads ("9,8 Mio").
func num(n int64) string {
	switch {
	case n >= 1_000_000:
		return de(float64(n)/1e6) + " Mio"
	case n >= 10_000:
		return strconv.FormatInt(n/1000, 10) + " Tsd"
	}
	return strconv.FormatInt(n, 10)
}

func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "gerade eben"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d Std %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d T %d Std", int(d.Hours())/24, int(d.Hours())%24)
}

var avatarColors = []string{"#c58b4f", "#5f9bc5", "#7fb069", "#b56576", "#9a7fc4", "#d4a843", "#4fa39a"}

// avatar draws a small pixel face; no external skin service is contacted.
func avatar(name string) template.HTML {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(name)))
	c := avatarColors[h.Sum32()%uint32(len(avatarColors))]
	return template.HTML(`<svg class="avatar" viewBox="0 0 8 8" aria-hidden="true"><rect width="8" height="8" fill="` + c + `"/><rect width="8" height="3" fill="rgba(0,0,0,.35)"/><rect x="2" y="4" width="1" height="1" fill="#fff"/><rect x="5" y="4" width="1" height="1" fill="#fff"/><rect x="3" y="6" width="2" height="1" fill="rgba(0,0,0,.4)"/></svg>`)
}

// spark renders the CPU sparkline as inline SVG.
func spark(vals []float64) template.HTML {
	const w, h = 248.0, 44.0
	if len(vals) < 2 {
		vals = []float64{0, 0}
	}
	max := 10.0
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	var pts strings.Builder
	var lx, ly float64
	for i, v := range vals {
		lx = float64(i) / float64(len(vals)-1) * w
		ly = h - 2 - v/max*(h-6)
		fmt.Fprintf(&pts, "%.1f,%.1f ", lx, ly)
	}
	p := strings.TrimSpace(pts.String())
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 %g %g" preserveAspectRatio="none" aria-hidden="true"><line class="grid" x1="0" y1="%g" x2="%g" y2="%g"/><polygon class="area" points="0,%g %s %g,%g"/><polyline class="ln" points="%s"/><circle class="end" cx="%.1f" cy="%.1f" r="2.5"/></svg>`,
		w, h, h/2, w, h/2, h, p, w, h, p, lx, ly))
}
