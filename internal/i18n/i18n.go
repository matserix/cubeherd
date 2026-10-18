// Package i18n translates the user interface. German is the source language and the
// German text is the key; every other language is a JSON file mapping it to a translation.
// Adding a language means adding <code>.json and an entry in Languages.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed *.json
var files embed.FS

type Lang struct{ Code, Name string }

// Languages offered in the settings, source language first.
var Languages = []Lang{{"de", "Deutsch"}, {"en", "English"}}

const Source = "de"

var catalogs = map[string]map[string]string{}

func init() {
	for _, l := range Languages {
		if l.Code == Source {
			continue
		}
		b, err := files.ReadFile(l.Code + ".json")
		if err != nil {
			panic("i18n: missing catalog " + l.Code)
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			panic("i18n: " + l.Code + ".json: " + err.Error())
		}
		catalogs[l.Code] = m
	}
}

// Valid reports whether code is an offered language.
func Valid(code string) bool {
	for _, l := range Languages {
		if l.Code == code {
			return true
		}
	}
	return false
}

// T translates key (German) into lang and formats it with args. Missing translations fall back to German.
func T(lang, key string, args ...any) string {
	s := key
	if lang != Source {
		if v, ok := catalogs[lang][key]; ok && v != "" {
			s = v
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// Catalog returns the translations of a language (for tests).
func Catalog(lang string) map[string]string { return catalogs[lang] }

// Match picks the best offered language for an Accept-Language header (English if nothing fits).
func Match(accept string) string {
	for _, part := range strings.Split(accept, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		base, _, _ := strings.Cut(tag, "-")
		if Valid(base) {
			return base
		}
	}
	return "en"
}
