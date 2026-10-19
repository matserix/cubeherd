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
	if err != nil || p.MSPT != 15 || len(p.Dims) != 1 || p.Dims[0].MSPT != 12.3 {
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
	// Folia only runs tps.
	if p, err := ParsePerf("FOLIA", []string{tps}); err != nil || p.TPS != 20 || p.MSPT != 0 {
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
