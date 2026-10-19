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

	"cubeherd/internal/i18n"
	"cubeherd/internal/server"
	"cubeherd/internal/sources"
)

// displayLoc is the time zone used for dates in the UI (set by New).
var displayLoc = func() *time.Location { return time.Local }

// decimal formats a float with one decimal in the language's style (3,1 vs 3.1).
func decimal(lang string, f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if decimalSep(lang) == "," {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

func decimalSep(lang string) string {
	if lang == "de" {
		return ","
	}
	return "."
}

// newFuncs builds the template functions for one language.
func newFuncs(lang string) template.FuncMap {
	t := func(key string, args ...any) string { return i18n.T(lang, key, args...) }
	dec := func(f float64) string { return decimal(lang, f) }
	gbf := func(b any) string { return dec(toF(b) / (1 << 30)) }
	return template.FuncMap{
		"t":         t,
		"lang":      func() string { return lang },
		"languages": func() []i18n.Lang { return i18n.Languages },
		"decimal":   func() string { return decimalSep(lang) },
		"static":    staticURL,
		"typeName":  server.TypeName,
		"types":     func() any { return server.Types },
		"de":        dec,
		"gb":        gbf,
		"pct":       pct,
		"level":     level,
		"avatar":    avatar,
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"list":     func(v ...string) []string { return v },
		"joinPath": joinRel,
		"since": func(tm time.Time) string {
			if tm.IsZero() {
				return ""
			}
			d := time.Since(tm)
			switch {
			case d < time.Minute:
				return t("gerade eben")
			case d < time.Hour:
				return fmt.Sprintf("%d min", int(d.Minutes()))
			case d < 24*time.Hour:
				return t("%d Std %d min", int(d.Hours()), int(d.Minutes())%60)
			}
			return t("%d T %d Std", int(d.Hours())/24, int(d.Hours())%24)
		},
		// num formats large counts like downloads ("9,8 Mio" / "9.8M").
		"num": func(n int64) string {
			switch {
			case n >= 1_000_000:
				return t("%s Mio", dec(float64(n)/1e6))
			case n >= 10_000:
				return t("%d Tsd", n/1000)
			}
			return strconv.FormatInt(n, 10)
		},
		"size": func(n int64) string {
			switch {
			case n >= 1<<30:
				return dec(float64(n)/(1<<30)) + " GB"
			case n >= 1<<20:
				return dec(float64(n)/(1<<20)) + " MB"
			case n >= 1<<10:
				return dec(float64(n)/(1<<10)) + " KB"
			}
			return strconv.FormatInt(n, 10) + " B"
		},
		"older": func(target, current string) bool {
			return sources.IsVersion(current) && sources.IsVersion(target) && sources.CompareMC(target, current) < 0
		},
		"mb": func(n int64) string {
			if n >= 1<<30 {
				return dec(float64(n)/(1<<30)) + " GB"
			}
			return dec(float64(n)/(1<<20)) + " MB"
		},
		"dt": func(tm time.Time) string {
			if tm.IsZero() {
				return ""
			}
			loc := displayLoc()
			tm = tm.In(loc)
			now := time.Now().In(loc)
			same := func(a, b time.Time) bool { return a.YearDay() == b.YearDay() && a.Year() == b.Year() }
			switch {
			case same(tm, now):
				return t("Heute, %s", tm.Format("15:04"))
			case same(tm, now.AddDate(0, 0, -1)):
				return t("Gestern, %s", tm.Format("15:04"))
			case same(tm, now.AddDate(0, 0, 1)):
				return t("Morgen, %s", tm.Format("15:04"))
			}
			if lang == "de" {
				return tm.Format("02.01.2006, 15:04")
			}
			return tm.Format("2006-01-02 15:04")
		},
		"addonSrcName": func(s string) string {
			return map[string]string{"modrinth": "Modrinth", "curseforge": "CurseForge", "spiget": "Spigot"}[s]
		},
		"srcName": func(s string) string {
			return map[string]string{server.KindModrinth: "Modrinth", server.KindCurseForge: "CurseForge"}[s]
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
			return dec(math.Round(float64(server.HeapMB(gb, modded))/1024*10) / 10)
		},
		"modeName":     func(m string) string { return t(modeNames[m]) },
		"diffName":     func(d string) string { return t(diffNames[d]) },
		"diffTitle":    func(d string) string { return t(diffTitles[d]) },
		"intervalName": func(i string) string { return t(intervalNames[i]) },
		"idleName":     func(m string) string { return t(idleNames[m]) },
	}
}

// Display names; the values are translation keys (see i18n test).
var (
	modeNames     = map[string]string{"survival": "Überleben", "creative": "Kreativ", "adventure": "Abenteuer", "spectator": "Zuschauer"}
	diffNames     = map[string]string{"peaceful": "friedlich", "easy": "leicht", "normal": "normal", "hard": "schwer"}
	diffTitles    = map[string]string{"peaceful": "Friedlich", "easy": "Leicht", "normal": "Normal", "hard": "Schwer"}
	intervalNames = map[string]string{"off": "Aus", "6h": "Alle 6 Stunden", "daily": "Täglich", "weekly": "Wöchentlich"}
	idleNames     = map[string]string{"pause": "Pausieren (Autopause)", "stop": "Stoppen (Autostop)", "none": "Weiterlaufen"}
)

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
