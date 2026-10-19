package server

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PerfSample is one reading of the server's tick performance.
type PerfSample struct {
	At      time.Time
	TPS     float64 // capped at 20
	MSPT    float64 // mean milliseconds per tick
	P95     float64 // only when the software reports percentiles
	HaveP95 bool
	Dims    []PerfDim // per world, only Forge and NeoForge
}

type PerfDim struct {
	Name string
	TPS  float64
	MSPT float64
}

// ErrPerfUnsupported means the server did not understand the tick command (e.g. Vanilla before 1.20.3).
var ErrPerfUnsupported = errors.New("tick command not supported")

// PerfCommands lists the RCON commands that report tick performance for a software type.
// spark would be the richer source, but it does not answer over RCON.
func PerfCommands(typ string) []string {
	switch typ {
	case "NEOFORGE":
		return []string{"neoforge tps"}
	case "FORGE":
		return []string{"forge tps"}
	case "PAPER", "PURPUR":
		return []string{"tps", "mspt"}
	case "FOLIA":
		return []string{"tps"}
	}
	return []string{"tick query"} // Vanilla, Fabric, Quilt (Minecraft 1.20.3 and newer)
}

// PerfSource is the human readable command shown under the charts.
func PerfSource(typ string) string { return strings.Join(PerfCommands(typ), " + ") }

var (
	unknownCmd = regexp.MustCompile(`(?i)unknown or incomplete command|unknown command|incorrect argument`)
	// "Overworld: 20.000 TPS (0.425 ms/tick)"  (NeoForge, Forge 1.20.x)
	forgeNew = regexp.MustCompile(`^\s*(.+?):\s*([\d.]+)\s*TPS\s*\(\s*([\d.]+)\s*ms/tick\)`)
	// "Dim 0 (minecraft:overworld): Mean tick time: 0.4 ms. Mean TPS: 20.0"  (older Forge)
	forgeOld = regexp.MustCompile(`^\s*(.+?):\s*Mean tick time:\s*([\d.]+)\s*ms\.\s*Mean TPS:\s*([\d.]+)`)
	paperTPS = regexp.MustCompile(`(?i)TPS from last[^:]*:\s*\*?([\d.]+)`)
	paperMS  = regexp.MustCompile(`([\d.]+)/([\d.]+)/([\d.]+)`)
	tickAvg  = regexp.MustCompile(`(?i)Average time per tick:\s*([\d.]+)\s*ms`)
	tickRate = regexp.MustCompile(`(?i)Target tick rate:\s*([\d.]+)`)
	tickP95  = regexp.MustCompile(`(?i)P95:\s*([\d.]+)\s*ms`)
)

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func capTPS(v float64) float64 {
	if v > 20 {
		return 20
	}
	return v
}

// tpsFromMSPT derives TPS from the mean tick time; a tick never runs faster than the 50 ms target.
func tpsFromMSPT(ms, target float64) float64 {
	if target <= 0 {
		target = 20
	}
	budget := 1000 / target
	if ms <= budget {
		return target
	}
	return 1000 / ms
}

// ParsePerf turns the outputs of PerfCommands (same order) into a sample.
func ParsePerf(typ string, outs []string) (PerfSample, error) {
	for _, o := range outs {
		if unknownCmd.MatchString(o) {
			return PerfSample{}, ErrPerfUnsupported
		}
	}
	join := func() string { return strings.Join(outs, "\n") }
	var p PerfSample
	switch typ {
	case "NEOFORGE", "FORGE":
		for _, line := range strings.Split(join(), "\n") {
			name, tps, ms := "", 0.0, 0.0
			if m := forgeNew.FindStringSubmatch(line); m != nil {
				name, tps, ms = m[1], atof(m[2]), atof(m[3])
			} else if m := forgeOld.FindStringSubmatch(line); m != nil {
				name, ms, tps = m[1], atof(m[2]), atof(m[3])
			} else {
				continue
			}
			if strings.EqualFold(name, "overall") {
				p.TPS, p.MSPT = capTPS(tps), ms
				continue
			}
			p.Dims = append(p.Dims, PerfDim{Name: name, TPS: capTPS(tps), MSPT: ms})
		}
		if p.MSPT == 0 && p.TPS == 0 {
			return PerfSample{}, errors.New("no tick data in the reply")
		}
	case "PAPER", "PURPUR", "FOLIA":
		m := paperTPS.FindStringSubmatch(join())
		if m == nil {
			return PerfSample{}, errors.New("no tick data in the reply")
		}
		p.TPS = capTPS(atof(m[1]))
		if len(outs) > 1 {
			if mm := paperMS.FindStringSubmatch(outs[1]); mm != nil {
				p.MSPT = atof(mm[1])
			}
		}
	default:
		o := join()
		m := tickAvg.FindStringSubmatch(o)
		if m == nil {
			return PerfSample{}, errors.New("no tick data in the reply")
		}
		p.MSPT = atof(m[1])
		target := 20.0
		if t := tickRate.FindStringSubmatch(o); t != nil {
			target = atof(t[1])
		}
		p.TPS = tpsFromMSPT(p.MSPT, target)
		if q := tickP95.FindStringSubmatch(o); q != nil {
			p.P95, p.HaveP95 = atof(q[1]), true
		}
	}
	return p, nil
}

// PerfLevel rates a sample: ok, warn (little headroom left) or crit (the server falls behind).
func PerfLevel(p PerfSample) string {
	switch {
	case p.TPS < 15 || p.MSPT >= 60:
		return "crit"
	case p.TPS < 19.5 || p.MSPT >= 40:
		return "warn"
	}
	return "ok"
}
