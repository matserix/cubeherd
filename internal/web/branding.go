package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"cubeherd/internal/config"
)

// White label: the name, logo, favicon and every color of the interface can be changed. The built-in look stays the
// default; only what differs from it is stored.

const defaultBrandName = "Cubeherd"

// brandToken is one color of the theme.
type brandToken struct {
	Key   string // CSS variable without the leading dashes
	Label string
	Group string
	Dark  string
	Light string
}

// brandTokens lists the colors in the order of the editor. The defaults must equal app.css (a test checks that).
var brandTokens = []brandToken{
	{"bg", "Hintergrund", "Flächen", "#13161b", "#eef0f3"},
	{"surface", "Fläche", "Flächen", "#1a1e25", "#ffffff"},
	{"surface-2", "Fläche 2", "Flächen", "#222731", "#f3f4f7"},
	{"surface-3", "Fläche 3", "Flächen", "#2a303b", "#e7eaef"},
	{"line", "Linie", "Flächen", "#2d333e", "#dce0e6"},
	{"line-strong", "Linie, kräftig", "Flächen", "#3b4350", "#c6ccd5"},
	{"fg", "Text", "Text", "#e3e6eb", "#1a1e25"},
	{"muted", "Text, gedämpft", "Text", "#8a93a2", "#5a6371"},
	{"faint", "Text, blass", "Text", "#626b79", "#848c99"},
	{"accent", "Akzent", "Akzent", "#d98a4e", "#b5622a"},
	{"accent-2", "Akzent 2", "Akzent", "#a9643a", "#d98a4e"},
	{"accent-3", "Akzent 3", "Akzent", "#7a4429", "#8a4a22"},
	{"accent-fg", "Text auf Akzent", "Akzent", "#1c1209", "#ffffff"},
	{"ok", "Erfolg", "Status", "#4ec98d", "#1d8a57"},
	{"warn", "Warnung", "Status", "#e3b443", "#9c7209"},
	{"crit", "Fehler", "Status", "#ea5a50", "#c23a30"},
	{"info", "Information", "Status", "#5fbcdb", "#1b7a9c"},
	{"console-bg", "Konsole, Hintergrund", "Konsole", "#0e1014", "#1a1e25"},
	{"console-fg", "Konsole, Text", "Konsole", "#cdd3dc", "#dfe3e9"},
}

// soft colors follow their base color.
var brandSoft = map[string]string{"accent": "accent-soft", "ok": "ok-soft", "warn": "warn-soft", "crit": "crit-soft", "info": "info-soft"}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func brandTokenByKey(k string) (brandToken, bool) {
	for _, t := range brandTokens {
		if t.Key == k {
			return t, true
		}
	}
	return brandToken{}, false
}

// Brand is what the templates need.
type Brand struct {
	Name        string
	Logo        string // address of the logo, "" = the built-in one
	Favicon     string
	HideVersion bool
	CSS         template.CSS // color overrides; empty for the built-in look
	Custom      bool         // the name differs from Cubeherd
}

func brandFile(rev int, name, kind string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("/branding/%s?v=%d", kind, rev)
}

// brand builds the template data from the settings. Values that are not valid colors are dropped, so a hand-edited
// cubeherd.json cannot inject CSS.
func (a *App) brand() Brand {
	b := a.Cfg.Get().Branding
	if b == nil {
		return Brand{Name: defaultBrandName}
	}
	out := Brand{Name: strings.TrimSpace(b.Name), Logo: brandFile(b.Rev, b.Logo, "logo"), Favicon: brandFile(b.Rev, b.Favicon, "favicon"), HideVersion: b.HideVersion}
	if out.Name == "" {
		out.Name = defaultBrandName
	}
	out.Custom = out.Name != defaultBrandName
	out.CSS = brandCSS(b)
	return out
}

func brandDecls(over map[string]string, light bool) string {
	var sb strings.Builder
	for _, t := range brandTokens {
		v, ok := over[t.Key]
		if !ok || !hexColor.MatchString(v) {
			continue
		}
		fmt.Fprintf(&sb, "--%s:%s;", t.Key, strings.ToLower(v))
		if soft := brandSoft[t.Key]; soft != "" {
			pct := 14
			if light {
				pct = 10
			}
			fmt.Fprintf(&sb, "--%s:color-mix(in srgb,%s %d%%,transparent);", soft, strings.ToLower(v), pct)
		}
	}
	return sb.String()
}

func brandCSS(b *config.Branding) template.CSS {
	dark, light := brandDecls(b.Dark, false), brandDecls(b.Light, true)
	var sb strings.Builder
	if dark != "" {
		fmt.Fprintf(&sb, ":root{%s}", dark)
	}
	if light != "" {
		fmt.Fprintf(&sb, "@media (prefers-color-scheme:light){:root:not([data-theme=\"dark\"]){%s}}:root[data-theme=\"light\"]{%s}", light, light)
	}
	if dark != "" {
		fmt.Fprintf(&sb, ":root[data-theme=\"dark\"]{%s}", dark)
	}
	return template.CSS(sb.String())
}

// brandPages are the administration pages; they keep the product name in their texts.
var brandKeepName = map[string]bool{"settings": true, "access": true, "nodes": true, "nodestats": true, "branding": true}

// rebrand replaces the product name in the texts of a customer-facing page.
func rebrand(page, body string, b Brand) string {
	if !b.Custom || brandKeepName[page] {
		return body
	}
	return strings.ReplaceAll(body, defaultBrandName, html.EscapeString(b.Name))
}

// brandText replaces the product name in mails and other plain text.
func (a *App) brandText(s string) string {
	if b := a.brand(); b.Custom {
		return strings.ReplaceAll(s, defaultBrandName, b.Name)
	}
	return s
}

// ---------------- Files ----------------

const maxBrandFile = 512 << 10

func (a *App) brandDir() string { return filepath.Join(a.DataDir, "branding") }

// brandImage checks an uploaded image and returns the extension to store it under and its content type.
func brandImage(data []byte) (ext, ctype string, ok bool) {
	switch {
	case len(data) > 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png", "image/png", true
	case len(data) > 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return ".jpg", "image/jpeg", true
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp", "image/webp", true
	case len(data) > 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return ".gif", "image/gif", true
	case len(data) > 4 && data[0] == 0 && data[1] == 0 && data[2] == 1 && data[3] == 0:
		return ".ico", "image/x-icon", true
	}
	head := strings.ToLower(string(data[:min(len(data), 2048)]))
	if strings.Contains(head, "<svg") {
		return ".svg", "image/svg+xml", true
	}
	return "", "", false
}

func (a *App) saveBrandFile(kind string, r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBrandFile+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBrandFile {
		return "", fmt.Errorf("%s", a.t("Das Bild ist größer als 512 KB."))
	}
	ext, _, ok := brandImage(data)
	if !ok {
		return "", fmt.Errorf("%s", a.t("Erlaubt sind PNG, JPEG, WebP, GIF, ICO und SVG."))
	}
	if err := os.MkdirAll(a.brandDir(), 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	name := kind + "-" + hex.EncodeToString(sum[:6]) + ext
	if err := os.WriteFile(filepath.Join(a.brandDir(), name), data, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func (a *App) removeBrandFile(name string) {
	if name != "" && !strings.ContainsAny(name, `/\`) {
		_ = os.Remove(filepath.Join(a.brandDir(), name))
	}
}

// brandAsset serves the logo or the favicon. It is public, because the sign-in page shows it. SVG files get a
// content security policy that forbids scripts, in case someone opens the address directly.
func (a *App) brandAsset(w http.ResponseWriter, r *http.Request) {
	b := a.Cfg.Get().Branding
	name := ""
	if b != nil {
		if r.PathValue("kind") == "logo" {
			name = b.Logo
		} else if r.PathValue("kind") == "favicon" {
			name = b.Favicon
		}
	}
	if name == "" || strings.ContainsAny(name, `/\`) {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.brandDir(), name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, ctype, ok := brandImage(data)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	h.Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

// ---------------- Editor ----------------

type brandRow struct {
	brandToken
	DarkVal, LightVal   string // what is in effect now
	DarkOver, LightOver bool
}

type brandingData struct {
	Name                      string
	Logo                      string
	Favicon                   string
	HideVersion               bool
	Rows                      []brandRow
	Groups                    []string
	PreviewDark, PreviewLight template.CSS
}

func (a *App) brandingPage(w http.ResponseWriter, r *http.Request) {
	b := a.Cfg.Get().Branding
	if b == nil {
		b = &config.Branding{}
	}
	d := brandingData{Name: b.Name, HideVersion: b.HideVersion, Logo: brandFile(b.Rev, b.Logo, "logo"), Favicon: brandFile(b.Rev, b.Favicon, "favicon")}
	seen := map[string]bool{}
	for _, t := range brandTokens {
		row := brandRow{brandToken: t, DarkVal: t.Dark, LightVal: t.Light}
		if v, ok := b.Dark[t.Key]; ok && hexColor.MatchString(v) {
			row.DarkVal, row.DarkOver = strings.ToLower(v), true
		}
		if v, ok := b.Light[t.Key]; ok && hexColor.MatchString(v) {
			row.LightVal, row.LightOver = strings.ToLower(v), true
		}
		d.Rows = append(d.Rows, row)
		if !seen[t.Group] {
			seen[t.Group] = true
			d.Groups = append(d.Groups, t.Group)
		}
	}
	eff := func(f func(brandRow) string) map[string]string {
		m := map[string]string{}
		for _, row := range d.Rows {
			m[row.Key] = f(row)
		}
		return m
	}
	d.PreviewDark = template.CSS(brandDecls(eff(func(r brandRow) string { return r.DarkVal }), false))
	d.PreviewLight = template.CSS(brandDecls(eff(func(r brandRow) string { return r.LightVal }), true))
	a.render(w, r, "branding", Page{Title: a.t("Erscheinungsbild"), Nav: "settings", Data: d})
}

func (a *App) brandingSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(2 << 20); err != nil && err != http.ErrNotMultipart {
		a.brandingErr(w, r, a.t("Die Eingabe ist zu groß."))
		return
	}
	old := a.Cfg.Get().Branding
	nb := config.Branding{}
	if old != nil {
		nb = *old
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if len([]rune(name)) > 40 || strings.ContainsAny(name, "<>&\"'`\r\n") {
		a.brandingErr(w, r, a.t("Der Name darf höchstens 40 Zeichen lang sein und keine Sonderzeichen wie < > & \" ' enthalten."))
		return
	}
	nb.Name = name
	if nb.Name == defaultBrandName {
		nb.Name = ""
	}
	nb.HideVersion = checked(r, "hideVersion")
	var drop []string
	for _, kind := range []string{"logo", "favicon"} {
		cur := &nb.Logo
		if kind == "favicon" {
			cur = &nb.Favicon
		}
		if checked(r, kind+"Remove") && *cur != "" {
			drop = append(drop, *cur)
			*cur = ""
		}
		if f, _, err := r.FormFile(kind); err == nil {
			file, err := a.saveBrandFile(kind, f)
			f.Close()
			if err != nil {
				a.brandingErr(w, r, err.Error())
				return
			}
			if *cur != "" && *cur != file {
				drop = append(drop, *cur)
			}
			*cur = file
		}
	}
	nb.Dark, nb.Light = map[string]string{}, map[string]string{}
	for _, t := range brandTokens {
		for _, side := range []struct {
			prefix string
			def    string
			out    map[string]string
		}{{"dark-", t.Dark, nb.Dark}, {"light-", t.Light, nb.Light}} {
			v := strings.ToLower(strings.TrimSpace(r.FormValue(side.prefix + t.Key)))
			if v != "" && hexColor.MatchString(v) && v != side.def {
				side.out[t.Key] = v
			}
		}
	}
	if len(nb.Dark) == 0 {
		nb.Dark = nil
	}
	if len(nb.Light) == 0 {
		nb.Light = nil
	}
	nb.Rev++
	if empty(nb) {
		nb = config.Branding{}
	}
	if err := a.Cfg.Update(func(s *config.Settings) {
		if empty(nb) {
			s.Branding = nil
		} else {
			s.Branding = &nb
		}
	}); err != nil {
		a.brandingErr(w, r, err.Error())
		return
	}
	for _, f := range drop {
		a.removeBrandFile(f)
	}
	redirect(w, r, "/settings/branding")
}

func empty(b config.Branding) bool {
	return b.Name == "" && b.Logo == "" && b.Favicon == "" && !b.HideVersion && len(b.Dark) == 0 && len(b.Light) == 0
}

func (a *App) brandingReset(w http.ResponseWriter, r *http.Request) {
	old := a.Cfg.Get().Branding
	if err := a.Cfg.Update(func(s *config.Settings) { s.Branding = nil }); err != nil {
		a.brandingErr(w, r, err.Error())
		return
	}
	if old != nil {
		a.removeBrandFile(old.Logo)
		a.removeBrandFile(old.Favicon)
	}
	redirect(w, r, "/settings/branding")
}

func (a *App) brandingErr(w http.ResponseWriter, r *http.Request, msg string) {
	if isHX(r) {
		fail(w, msg)
		return
	}
	http.Error(w, msg, http.StatusBadRequest)
}
