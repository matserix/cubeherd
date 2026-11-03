package web

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
	"time"

	"cubeherd/internal/api"
)

// Charts are plain SVG built on the server, like the sparkline: no script library, and every point carries a
// <title> with its time and values for the hover.

type chartSeries struct {
	Name  string
	Class string // s1..s6, see app.css
	Vals  []*float64
	Area  bool // fill below the line
	Thin  bool // a faint helper line, such as the maximum behind an average
}

type chartSpec struct {
	Title    string
	Unit     func(float64) string
	T        []int64 // time of each point
	Series   []chartSeries
	YMax     float64 // 0 = fit to the data
	Fixed    bool    // YMax is not raised to fit
	Count    bool    // whole numbers (players, servers): the axis runs 0-4, 0-8, 0-16 … so every step is a whole number
	Div      float64 // axis steps are round numbers of this unit (1 << 30 for GB); 0 = 1
	From, To int64
	Step     int
	Events   []api.MetricEvent
	Loc      *time.Location
	Label    func(t int64, vals []*float64) string // text of the hover for one point
}

const (
	chartW  = 860.0
	chartH  = 190.0
	chartL  = 56.0
	chartR  = 12.0
	chartT  = 12.0
	chartB  = 26.0
	gapMult = 2.5 // a pause longer than this many steps breaks the line
)

// niceMax rounds up to 1, 2, 5 or 10 times a power of ten so the axis ends on a round number.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if v <= m*p {
			return m * p
		}
	}
	return 10 * p
}

// niceBinary rounds up to a power of two (0.5, 1, 2, 4, 8, …), whose quarters are round numbers.
func niceBinary(v float64) float64 {
	m := 0.5
	for m < v {
		m *= 2
	}
	return m
}

func (sp chartSpec) xOf(t int64) float64 {
	span := float64(sp.To - sp.From)
	if span <= 0 {
		return chartL
	}
	return chartL + (float64(t-sp.From)/span)*(chartW-chartL-chartR)
}

func renderChart(sp chartSpec) template.HTML {
	max := sp.YMax
	if !sp.Fixed || max <= 0 {
		for _, s := range sp.Series {
			for _, v := range s.Vals {
				if v != nil && *v > max {
					max = *v
				}
			}
		}
		if sp.YMax > max {
			max = sp.YMax
		}
		div := sp.Div
		if div <= 0 {
			div = 1
		}
		if sp.Count {
			max = math.Max(niceBinary(max*1.05), 4)
		} else if div >= 1<<20 { // bytes: whole gigabytes with quarters that are round numbers (4, 8, 16 …)
			max = niceBinary(max*1.05/div) * div
		} else {
			max = niceMax(max*1.05/div) * div
		}
	}
	y := func(v float64) float64 {
		if v > max {
			v = max
		}
		if v < 0 {
			v = 0
		}
		return chartT + (1-v/max)*(chartH-chartT-chartB)
	}
	base := chartH - chartB
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s" preserveAspectRatio="xMidYMid meet">`, chartW, chartH, html.EscapeString(sp.Title))
	// grid and y axis
	for i := 0; i <= 4; i++ {
		v := max * float64(i) / 4
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/><text class="ylab" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
			chartL, chartW-chartR, y(v), y(v), chartL-6, y(v)+4, html.EscapeString(sp.Unit(v)))
	}
	// x axis
	span := sp.To - sp.From
	layout := "15:04"
	switch {
	case span > 20*86400:
		layout = "02.01.06"
	case span > 36*3600:
		layout = "02.01. 15:04"
	}
	loc := sp.Loc
	if loc == nil {
		loc = time.UTC
	}
	for i := 0; i <= 4; i++ {
		t := sp.From + span*int64(i)/4
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == 4 {
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text class="xlab" x="%.1f" y="%.1f" text-anchor="%s">%s</text>`, sp.xOf(t), chartH-8, anchor, time.Unix(t, 0).In(loc).Format(layout))
	}
	// events
	for _, e := range sp.Events {
		if e.T < sp.From || e.T > sp.To {
			continue
		}
		title := e.Kind
		if e.Detail != "" {
			title += ": " + e.Detail
		}
		fmt.Fprintf(&b, `<g class="ev ev-%s"><line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/><title>%s · %s</title></g>`,
			html.EscapeString(e.Kind), sp.xOf(e.T), sp.xOf(e.T), chartT, base, time.Unix(e.T, 0).In(loc).Format("02.01. 15:04"), html.EscapeString(title))
	}
	// lines: a gap or a missing value starts a new piece
	limit := int64(float64(sp.Step) * gapMult)
	for _, s := range sp.Series {
		var d, area strings.Builder
		var piece []string
		flush := func() {
			if len(piece) == 0 {
				return
			}
			if len(piece) == 1 {
				fmt.Fprintf(&b, `<circle class="pt %s" cx="%s" r="1.8"/>`, s.Class, strings.Replace(piece[0], ",", `" cy="`, 1))
			} else {
				d.WriteString("M" + strings.Join(piece, " L"))
				if s.Area {
					first, lastp := piece[0], piece[len(piece)-1]
					fx, lx := strings.Split(first, ",")[0], strings.Split(lastp, ",")[0]
					fmt.Fprintf(&area, "M%s,%.1f L%s L%s,%.1f Z ", fx, base, strings.Join(piece, " L"), lx, base)
				}
			}
			piece = piece[:0]
		}
		var prevT int64 = -1
		for i, v := range s.Vals {
			if v == nil || math.IsNaN(*v) {
				flush()
				prevT = -1
				continue
			}
			if prevT >= 0 && sp.T[i]-prevT > limit {
				flush()
			}
			piece = append(piece, fmt.Sprintf("%.1f,%.1f", sp.xOf(sp.T[i]), y(*v)))
			prevT = sp.T[i]
		}
		flush()
		if area.Len() > 0 {
			fmt.Fprintf(&b, `<path class="area %s" d="%s"/>`, s.Class, area.String())
		}
		if d.Len() > 0 {
			cls := "line " + s.Class
			if s.Thin {
				cls += " thin"
			}
			fmt.Fprintf(&b, `<path class="%s" d="%s"/>`, cls, d.String())
		}
	}
	// hover columns
	n := len(sp.T)
	colW := math.Max((chartW-chartL-chartR)*float64(sp.Step)/math.Max(float64(span), 1), 2)
	for i := 0; i < n; i++ {
		vals := make([]*float64, len(sp.Series))
		for j, s := range sp.Series {
			vals[j] = s.Vals[i]
		}
		txt := time.Unix(sp.T[i], 0).In(loc).Format("02.01. 15:04")
		if sp.Label != nil {
			txt += " · " + sp.Label(sp.T[i], vals)
		} else {
			var parts []string
			for j, s := range sp.Series {
				if vals[j] != nil && !s.Thin {
					parts = append(parts, s.Name+" "+sp.Unit(*vals[j]))
				}
			}
			txt += " · " + strings.Join(parts, " · ")
		}
		fmt.Fprintf(&b, `<rect class="hov" x="%.1f" y="%.1f" width="%.1f" height="%.1f"><title>%s</title></rect>`, sp.xOf(sp.T[i])-colW/2, chartT, colW, base-chartT, html.EscapeString(txt))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// ---------------- Units ----------------

func pctUnit(v float64) string { return fmt.Sprintf("%.0f %%", v) }

func gbUnit(lang string) func(float64) string {
	return func(v float64) string {
		g := v / (1 << 30)
		if g >= 10 {
			return decimal(lang, math.Round(g)) + " GB"
		}
		return decimal(lang, math.Round(g*10)/10) + " GB"
	}
}

func intUnit(v float64) string { return fmt.Sprintf("%.0f", v) }

func msUnit(lang string) func(float64) string {
	return func(v float64) string {
		if v < 1 && v != 0 {
			return strings.Replace(fmt.Sprintf("%.2f", v), ".", decimalSep(lang), 1) + " ms"
		}
		return decimal(lang, math.Round(v*10)/10) + " ms"
	}
}

func tpsUnit(lang string) func(float64) string {
	return func(v float64) string { return decimal(lang, math.Round(v*10)/10) }
}
