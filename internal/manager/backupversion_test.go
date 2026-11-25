package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cubeherd/internal/server"
)

// A failed installation leaves the old pack on disk while the definition already names the new one.
func TestInstalledSnapshotChecksPackManifest(t *testing.T) {
	// More entries than the file editor would read, like the manifest of a large pack.
	large := `{"files":["` + strings.Repeat(`mods/some-long-file-name.jar","`, 60_000) + `x"],"fileId":111}`
	for _, c := range []struct {
		name, kind, file, content string
		known                     bool
	}{
		{"curseforge match", server.KindCurseForge, ".curseforge-manifest.json", `{"fileId":222}`, true},
		{"curseforge other file", server.KindCurseForge, ".curseforge-manifest.json", `{"fileId":111}`, false},
		{"curseforge large manifest", server.KindCurseForge, ".curseforge-manifest.json", large, false},
		{"curseforge without manifest", server.KindCurseForge, "", "", true},
		{"curseforge without file id", server.KindCurseForge, ".curseforge-manifest.json", `{}`, true},
		{"curseforge broken manifest", server.KindCurseForge, ".curseforge-manifest.json", `{`, true},
		{"modrinth match", server.KindModrinth, ".modrinth-modpack-manifest.json", `{"versionId":"222"}`, true},
		{"modrinth other version", server.KindModrinth, ".modrinth-modpack-manifest.json", `{"versionId":"111"}`, false},
		{"manifest of the other source is ignored", server.KindModrinth, ".curseforge-manifest.json", `{"fileId":111}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, s := healthFixture(t, daily(), time.Now())
			s.Kind, s.Pack = c.kind, &server.Pack{Slug: "p", Name: "P", Version: "222"}
			if err := m.Store.Save(s, m.globals()); err != nil {
				t.Fatal(err)
			}
			if c.file != "" {
				if err := os.WriteFile(filepath.Join(m.Store.DataDir(s), c.file), []byte(c.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := m.installedSnapshot(context.Background(), s, nil); (got != nil) != c.known {
				t.Errorf("snapshot = %v, want known = %v", got.Label(), c.known)
			}
		})
	}
}
