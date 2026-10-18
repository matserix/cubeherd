package sources

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestGuessJava(t *testing.T) {
	for mc, want := range map[string]string{"26.1": "java25", "1.21.10": "java21", "1.20.5": "java21", "1.20.4": "java17", "1.18.2": "java17", "1.16.5": "java8", "1.12.2": "java8"} {
		if got := tagFor(guessJava(mc)); got != want {
			t.Errorf("%s: got %s want %s", mc, got, want)
		}
	}
}

func TestLatestMC(t *testing.T) {
	if got := latestMC([]string{"1.20.1", "Forge", "1.21.1", "1.9", "Server"}); got != "1.21.1" {
		t.Errorf("got %s", got)
	}
}

// FC_NET=1 go test ./internal/sources -run Live -v
func TestLiveSources(t *testing.T) {
	if os.Getenv("FC_NET") == "" {
		t.Skip("FC_NET not set")
	}
	c := New(func() string { return os.Getenv("CF_API_KEY") })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rel, err := c.Releases(ctx)
	if err != nil || len(rel) == 0 {
		t.Fatal(err)
	}
	t.Logf("latest %s; java tag %s; 1.21.1 → %s; 1.16.5 → %s", rel[0], c.JavaTag(ctx, rel[0]), c.JavaTag(ctx, "1.21.1"), c.JavaTag(ctx, "1.16.5"))
	packs, err := c.ModrinthSearch(ctx, "optimized")
	if err != nil || len(packs) == 0 {
		t.Fatal(err)
	}
	t.Logf("modrinth: %+v", packs[0])
	vs, err := c.ModrinthVersions(ctx, packs[0].Slug)
	if err != nil || len(vs) == 0 {
		t.Fatal(err)
	}
	t.Logf("versions: %+v", vs[0])
	if _, err := c.CurseForgeSearch(ctx, "all the mods"); err != nil {
		t.Logf("curseforge: %v", err)
	}
}
