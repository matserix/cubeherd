package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"forgecraft/internal/i18n"
	"forgecraft/internal/manager"
	"forgecraft/internal/server"
	"forgecraft/internal/sources"
)

var (
	tmplKey = regexp.MustCompile(`\bt "((?:[^"\\]|\\.)*)"`)
	goKey   = regexp.MustCompile(`(?:\b[am]\.t\(|\bt\(|i18n\.T\([^,()\n]+, )"((?:[^"\\\n]|\\.)*)"|\ba\.fail\(w, "((?:[^"\\\n]|\\.)*)"|\ba\.toast\(w, "[^"]*", "((?:[^"\\\n]|\\.)*)"`)
	dictKey = regexp.MustCompile(`"(?:Title|Hint)" "((?:[^"\\]|\\.)*)"`)
)

// usedKeys collects every German source text that is translated somewhere.
func usedKeys(t *testing.T) map[string]bool {
	keys := map[string]bool{}
	add := func(s string) {
		if s != "" {
			keys[s] = true
		}
	}
	unq := func(s string) string {
		u, err := strconv.Unquote(`"` + s + `"`)
		if err != nil {
			t.Fatalf("unquote %q: %v", s, err)
		}
		return u
	}
	_ = fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := templateFS.ReadFile(p)
		for _, m := range tmplKey.FindAllStringSubmatch(string(b), -1) {
			add(unq(m[1]))
		}
		for _, m := range dictKey.FindAllStringSubmatch(string(b), -1) {
			add(unq(m[1]))
		}
		return nil
	})
	// Go sources of the whole module (translations are requested in web and manager).
	root, _ := filepath.Abs("../..")
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, m := range goKey.FindAllStringSubmatch(string(b), -1) {
			for _, g := range m[1:] {
				if g != "" {
					add(unq(g))
				}
			}
		}
		return nil
	})
	// Data tables whose values are shown through {{t ...}} or translated as error texts.
	for _, v := range server.StatusLabels() {
		add(v)
	}
	for _, v := range server.CrashHints() {
		add(v)
	}
	for _, ty := range server.Types {
		add(ty.Desc)
	}
	for _, v := range manager.ReasonLabels() {
		add(v)
	}
	for _, m := range []map[string]string{modeNames, diffNames, diffTitles, intervalNames, idleNames, fileHints} {
		for _, v := range m {
			add(v)
		}
	}
	for _, tb := range tabs {
		add(tb.Label)
	}
	for _, v := range []string{"Modpack-Version", "Server-Image", "Minecraft-Version", "Mods", "Plugins",
		manager.ErrBusy.Error(), manager.ErrOutside.Error(), manager.ErrNoWorld.Error(), server.ErrNotFound.Error(),
		sources.ErrUnauthorized.Error()} {
		add(v)
	}
	return keys
}

// TestCatalogsComplete checks both directions: every used text is translated, and no
// translation is left over (that catches German texts edited without updating the catalog).
func TestCatalogsComplete(t *testing.T) {
	used := usedKeys(t)
	if os.Getenv("FC_DUMP_KEYS") != "" {
		var list []string
		for k := range used {
			list = append(list, k)
		}
		sort.Strings(list)
		b, _ := json.MarshalIndent(list, "", "  ")
		os.WriteFile(os.Getenv("FC_DUMP_KEYS"), b, 0o644)
	}
	for _, l := range i18n.Languages {
		if l.Code == i18n.Source {
			continue
		}
		cat := i18n.Catalog(l.Code)
		var missing, unused []string
		for k := range used {
			if cat[k] == "" {
				missing = append(missing, k)
			}
		}
		for k := range cat {
			if !used[k] {
				unused = append(unused, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(unused)
		for _, k := range missing {
			t.Errorf("%s: missing translation for %q", l.Code, k)
		}
		for _, k := range unused {
			t.Errorf("%s: translation no longer used: %q", l.Code, k)
		}
		// Format verbs must match, otherwise Sprintf output breaks.
		verbs := regexp.MustCompile(`%[-+# 0]*[0-9.]*[a-zA-Z%]`)
		for k, v := range cat {
			if strings.Join(verbs.FindAllString(k, -1), "") != strings.Join(verbs.FindAllString(v, -1), "") {
				t.Errorf("%s: format verbs differ for %q → %q", l.Code, k, v)
			}
		}
	}
}
