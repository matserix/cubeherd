package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs against a live itzg container: FC_PROBE=<container name> go test ./internal/docker -run Live -v
func TestLiveProbe(t *testing.T) {
	name := os.Getenv("FC_PROBE")
	if name == "" {
		t.Skip("FC_PROBE not set")
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	info, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("engine %s, %d cpus, %d MB", info.ServerVersion, info.NCPU, info.MemTotal>>20)

	ci, err := c.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	health := ""
	if ci.State.Health != nil {
		health = ci.State.Health.Status
	}
	t.Logf("state=%s health=%s tty=%v", ci.State.Status, health, ci.Config.Tty)

	var lines []string
	if err := c.Logs(ctx, name, false, 5, ci.Config.Tty, func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	t.Logf("last log lines:\n%s", strings.Join(lines, "\n"))

	st, err := c.StatsOnce(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mem %d MB / %d MB", st.MemUsed>>20, st.MemLimit>>20)

	n := 0
	sctx, scancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	_ = c.StatsStream(sctx, name, func(s Stats) { n++; t.Logf("cpu %.1f%%", s.CPUPercent) })
	scancel()
	if n == 0 {
		t.Error("no stats samples")
	}

	if ci.State.Running {
		out, code, err := c.Exec(ctx, name, "rcon-cli", "list")
		t.Logf("rcon list: code=%d err=%v out=%q", code, err, out)
	}
}
