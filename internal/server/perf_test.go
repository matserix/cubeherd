package server

import "testing"

func TestParsePerfNeoForge(t *testing.T) {
	out := "Overworld: 20.000 TPS (0.425 ms/tick)\nThe Nether: 19.500 TPS (51.200 ms/tick)\nOverall: 20.000 TPS (1.215 ms/tick)\n"
	p, err := ParsePerf("NEOFORGE", []string{out})
	if err != nil || p.TPS != 20 || p.MSPT != 1.215 || len(p.Dims) != 2 || p.Dims[1].Name != "The Nether" || p.Dims[1].MSPT != 51.2 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestParsePerfOldForge(t *testing.T) {
	out := "Dim 0 (minecraft:overworld): Mean tick time: 12.3 ms. Mean TPS: 20.0\nOverall: Mean tick time: 15.0 ms. Mean TPS: 20.0\n"
	p, err := ParsePerf("FORGE", []string{out})
	if err != nil || p.MSPT != 15 || len(p.Dims) != 1 || p.Dims[0].MSPT != 12.3 || p.Dims[0].Name != "Overworld" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestParsePerfPaper(t *testing.T) {
	tps := "TPS from last 1m, 5m, 15m: *20.0, 19.8, 19.5"
	mspt := "Server tick times (avg/min/max) from last 5s, 10s, 1m:\n◴ 7.6/3.5/32.9, 7.9/3.1/40.2, 8.0/2.9/80.1"
	p, err := ParsePerf("PAPER", []string{tps, mspt})
	if err != nil || p.TPS != 20 || p.MSPT != 7.6 || len(p.Dims) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestParsePerfVanilla(t *testing.T) {
	out := "The game is running normally\nTarget tick rate: 20.0 per second.\nAverage time per tick: 61.5ms (Target: 50.0ms)\nPercentiles: P50: 58.1ms P95: 90.8ms P99: 120.7ms, sample: 100\n"
	p, err := ParsePerf("VANILLA", []string{out})
	if err != nil || p.MSPT != 61.5 || !p.HaveP95 || p.P95 != 90.8 {
		t.Fatalf("%+v %v", p, err)
	}
	if want := 1000 / 61.5; p.TPS < want-.01 || p.TPS > want+.01 {
		t.Fatalf("TPS %v want %v", p.TPS, want)
	}
	if p, _ := ParsePerf("FABRIC", []string{"Average time per tick: 3.5ms (Target: 50.0ms)"}); p.TPS != 20 {
		t.Fatalf("fast ticks must read 20 TPS: %+v", p)
	}
}

func TestParsePerfErrors(t *testing.T) {
	if _, err := ParsePerf("VANILLA", []string{"Unknown or incomplete command, see below for error"}); err != ErrPerfUnsupported {
		t.Fatalf("want unsupported, got %v", err)
	}
	if _, err := ParsePerf("PAPER", []string{"garbage"}); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestPerfLevel(t *testing.T) {
	cases := []struct {
		p    PerfSample
		want string
	}{
		{PerfSample{TPS: 20, MSPT: 14}, "ok"},
		{PerfSample{TPS: 20, MSPT: 43}, "warn"},
		{PerfSample{TPS: 18, MSPT: 30}, "warn"},
		{PerfSample{TPS: 9.7, MSPT: 102}, "crit"},
		{PerfSample{TPS: 20, MSPT: 65}, "crit"},
	}
	for _, c := range cases {
		if got := PerfLevel(c.p); got != c.want {
			t.Errorf("%+v: %s want %s", c.p, got, c.want)
		}
	}
}

func TestPerfCommands(t *testing.T) {
	for typ, want := range map[string]string{"NEOFORGE": "neoforge tps", "FORGE": "forge tps", "PAPER": "tps + mspt", "FABRIC": "tick query", "VANILLA": "tick query"} {
		if got := PerfSource(typ); got != want {
			t.Errorf("%s: %q want %q", typ, got, want)
		}
	}
}

// Output captured from real containers (RCON glues the lines together without newlines).
func TestParsePerfRealOutput(t *testing.T) {
	q := "The game is running normallyTarget tick rate: 20.0 per second.\nAverage time per tick: 0.3ms (Target: 50.0ms)Percentiles: P50: 0.3ms P95: 0.5ms P99: 0.6ms, sample: 100"
	if p, err := ParsePerf("FABRIC", []string{q}); err != nil || p.TPS != 20 || p.MSPT != 0.3 || p.P95 != 0.5 {
		t.Fatalf("%+v %v", p, err)
	}
	tps := "TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0"
	mspt := "Server tick times (avg/min/max) from last 5s, 10s, 1m:\n◴ 0.5/3.3/40.2, 0.3/3.3/40.2, 0.0/3.3/40.2"
	if p, err := ParsePerf("PAPER", []string{tps, mspt}); err != nil || p.TPS != 20 || p.MSPT != 0.5 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPrettyDim(t *testing.T) {
	for in, want := range map[string]string{
		"Dim minecraft:the_nether (minecraft:the_nether)":                     "The Nether",
		"Dim 0 (minecraft:overworld)":                                         "Overworld",
		"Dim twilightforest:twilight_forest (twilightforest:twilight_forest)": "Twilight Forest",
		"Dim 7 (undergarden:undergarden)":                                     "Undergarden",
		"Overworld":                                                           "Overworld",
	} {
		if got := prettyDim(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

// Folia's "tps" is a health report; captured from a real container including raw colour codes.
const foliaReport = "§xServer Health Report\n§x - §xOnline Players: §x0\n§x - §xTotal regions: §x3\n§x - §xUtilisation: §x1.0§x% / §x200.0§x%\n§x - §xLoad rate: §x0.00, §xGen rate: §x9.80\n§x - §xLowest Region TPS: §x20.07\n§x - §xMedian Region TPS: §x20.08\n§x - §xHighest Region TPS: §x20.18\n§xHighest §x3§x utilisation regions\n§x - §xRegion around block §x[w:'world',-73,80,-361]§x:\n    §x0.1§x% util at §x2.14§x MSPT at §x20.18§x TPS\n    Chunks: §x49§x Players: §x0§x Entities: §x8\n§x - §xRegion around block §x[w:'world_nether',-537,80,-329]§x:\n    §x0.1§x% util at §x0.71§x MSPT at §x20.08§x TPS\n    Chunks: §x49§x Players: §x0§x Entities: §x0\n"

func TestParsePerfFolia(t *testing.T) {
	p, err := ParsePerf("FOLIA", []string{foliaReport})
	if err != nil || p.TPS != 20 || p.MSPT != 2.14 || len(p.Dims) != 2 || p.Dims[0].Name != "world (-73, -361)" || p.Dims[1].MSPT != 0.71 {
		t.Fatalf("%+v %v", p, err)
	}
	slow := "Lowest Region TPS: 11.5\nRegion around block [w:'world',0,64,0]:\n 99.0% util at 87.30 MSPT at 11.50 TPS"
	if p, err := ParsePerf("FOLIA", []string{slow}); err != nil || p.TPS != 11.5 || p.MSPT != 87.3 || PerfLevel(p) != "crit" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := ParsePerf("FOLIA", []string{"hello"}); err != ErrPerfUnreadable {
		t.Fatalf("want unreadable, got %v", err)
	}
}
