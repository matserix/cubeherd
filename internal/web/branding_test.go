package web

import (
	"bytes"
	"html/template"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"cubeherd/internal/config"
)

// The built-in colors in Go must equal app.css, or "only store what differs" would store the wrong things.
func TestBrandDefaultsMatchTheStylesheet(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	dark := s[strings.Index(s, ":root {"):strings.Index(s, "@media (prefers-color-scheme: light)")]
	light := s[strings.Index(s, ":root[data-theme=\"light\"] {"):]
	light = light[:strings.Index(light, "}")]
	get := func(block, key string) string {
		m := regexp.MustCompile(`--` + regexp.QuoteMeta(key) + `:\s*(#[0-9a-fA-F]{6})`).FindStringSubmatch(block)
		if m == nil {
			t.Fatalf("--%s not found", key)
		}
		return strings.ToLower(m[1])
	}
	for _, tk := range brandTokens {
		if got := get(dark, tk.Key); got != tk.Dark {
			t.Errorf("dark --%s: css %s, Go %s", tk.Key, got, tk.Dark)
		}
		if got := get(light, tk.Key); got != tk.Light {
			t.Errorf("light --%s: css %s, Go %s", tk.Key, got, tk.Light)
		}
	}
}

func (d *dashboard) multipart(c *http.Cookie, path string, fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for k, data := range files {
		fw, _ := w.CreateFormFile(k, k+".bin")
		fw.Write(data)
	}
	w.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("HX-Request", "true")
	req.Host = "example.com"
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)
	return rec
}

var tinyPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 40)...)

func TestDefaultLookIsUntouched(t *testing.T) {
	d, st := newAccounts(t)
	owner := d.cookieFor(t, st, "admin")
	body := d.as(owner, "GET", "/", nil, false).Body.String()
	if !strings.Contains(body, "<b>Cubeherd</b>") || strings.Contains(body, "brand-logo") || strings.Contains(body, "<style>") {
		t.Fatal("the default dashboard changed")
	}
	if !strings.Contains(d.guest("GET", "/login", nil).Body.String(), "<b>Cubeherd</b>") {
		t.Fatal("sign-in page changed")
	}
}

func TestBrandingNameLogoColorsAndReset(t *testing.T) {
	d, st := newAccounts(t)
	owner := d.cookieFor(t, st, "admin")
	rec := d.multipart(owner, "/settings/branding", map[string]string{
		"name": "BlockHost", "hideVersion": "on",
		"dark-accent": "#12ab34", "light-accent": "#ab1234",
		"dark-bg": "#13161b", // equal to the default: not stored
	}, map[string][]byte{"logo": tinyPNG})
	if rec.Code >= 400 || strings.Contains(rec.Header().Get("HX-Trigger"), "crit") {
		t.Fatalf("save: %d %v", rec.Code, rec.Header())
	}
	b := d.app.Cfg.Get().Branding
	if b == nil || b.Name != "BlockHost" || b.Logo == "" || b.Dark["accent"] != "#12ab34" || b.Light["accent"] != "#ab1234" {
		t.Fatalf("stored: %+v", b)
	}
	if _, ok := b.Dark["bg"]; ok {
		t.Error("a color equal to the default was stored")
	}
	// the sign-in page, which nobody is signed in for, carries the name, logo and colors
	login := d.guest("GET", "/login", nil).Body.String()
	for _, want := range []string{"<b>BlockHost</b>", `class="brand-logo"`, "--accent:#12ab34", "--accent:#ab1234", "color-mix"} {
		if !strings.Contains(login, want) {
			t.Errorf("login lacks %q", want)
		}
	}
	if strings.Contains(login, "Cubeherd") {
		t.Error("the product name is still on the sign-in page")
	}
	// customer-facing texts are rebranded, administration pages keep the name in their texts
	if !strings.Contains(d.as(owner, "GET", "/settings", nil, false).Body.String(), "Cubeherd") {
		t.Error("the settings page lost the product name")
	}
	// the logo is public and served safely
	logo := d.guest("GET", "/branding/logo", nil)
	if logo.Code != 200 || logo.Header().Get("Content-Type") != "image/png" || logo.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("logo: %d %v", logo.Code, logo.Header())
	}
	if d.guest("GET", "/branding/favicon", nil).Code != 404 || d.guest("GET", "/branding/other", nil).Code != 404 {
		t.Error("an unset favicon or an unknown kind must be 404")
	}
	// the editor shows what is in effect
	page := d.as(owner, "GET", "/settings/branding", nil, false).Body.String()
	if !strings.Contains(page, `value="#12ab34"`) || !strings.Contains(page, `value="BlockHost"`) {
		t.Error("the editor does not show the stored values")
	}
	// reset brings everything back and removes the files
	file := b.Logo
	d.as(owner, "POST", "/settings/branding/reset", nil, true)
	if d.app.Cfg.Get().Branding != nil {
		t.Fatal("not reset")
	}
	if _, err := os.Stat(d.app.brandDir() + "/" + file); err == nil {
		t.Error("the logo file stayed")
	}
	if strings.Contains(d.guest("GET", "/login", nil).Body.String(), "BlockHost") {
		t.Error("still branded after reset")
	}
}

func TestBrandingRejectsBadInput(t *testing.T) {
	d, st := newAccounts(t)
	owner := d.cookieFor(t, st, "admin")
	crit := func(rec *httptest.ResponseRecorder) bool {
		return strings.Contains(rec.Header().Get("HX-Trigger"), "crit")
	}
	if !crit(d.multipart(owner, "/settings/branding", map[string]string{"name": `<script>alert(1)</script>`}, nil)) {
		t.Error("a name with markup was accepted")
	}
	if !crit(d.multipart(owner, "/settings/branding", map[string]string{"name": strings.Repeat("x", 41)}, nil)) {
		t.Error("a long name was accepted")
	}
	if !crit(d.multipart(owner, "/settings/branding", nil, map[string][]byte{"logo": []byte("<html><script>alert(1)</script>")})) {
		t.Error("a non-image was accepted as logo")
	}
	if !crit(d.multipart(owner, "/settings/branding", nil, map[string][]byte{"logo": append(tinyPNG, make([]byte, maxBrandFile)...)})) {
		t.Error("an oversized logo was accepted")
	}
	// colors that are not #rrggbb never reach the stylesheet
	d.multipart(owner, "/settings/branding", map[string]string{"dark-accent": "red;}</style><script>x</script>", "light-fg": "url(x)"}, nil)
	if b := d.app.Cfg.Get().Branding; b != nil && (len(b.Dark) > 0 || len(b.Light) > 0) {
		t.Errorf("invalid colors stored: %+v", b)
	}
	// and a hand-edited file cannot inject either
	css := string(brandCSSForTest(map[string]string{"accent": "red;}</style><script>"}))
	if strings.Contains(css, "script") || strings.Contains(css, "red") {
		t.Errorf("css injection: %s", css)
	}
}

func TestBrandingNeedsTheSettingsRight(t *testing.T) {
	d, st := newAccounts(t)
	addUser(t, st, "vera")
	c := d.cookieFor(t, st, "vera")
	if d.as(c, "GET", "/settings/branding", nil, false).Code != 403 {
		t.Error("the editor is open to users without the right")
	}
	if rec := d.multipart(c, "/settings/branding", map[string]string{"name": "Evil"}, nil); rec.Code == 204 || rec.Header().Get("HX-Redirect") != "" {
		t.Errorf("save: %d %v", rec.Code, rec.Header())
	}
	if d.app.Cfg.Get().Branding != nil {
		t.Error("branding changed without the right")
	}
	// the asset itself needs no sign-in
	if req := httptest.NewRequest("GET", "/branding/logo", nil); req != nil {
		rec := httptest.NewRecorder()
		d.h.ServeHTTP(rec, req)
		if rec.Code == 303 {
			t.Error("the logo redirects to the sign-in page")
		}
	}
}

func TestBrandedMailsAndSVGHeaders(t *testing.T) {
	d, st := newAccounts(t)
	owner := d.cookieFor(t, st, "admin")
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script><rect width="1" height="1"/></svg>`)
	d.multipart(owner, "/settings/branding", map[string]string{"name": "BlockHost"}, map[string][]byte{"favicon": svg})
	rec := d.guest("GET", "/branding/favicon", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("svg: %d %v", rec.Code, rec.Header())
	}
	if got := d.app.brandText("Dein Cubeherd-Konto"); got != "Dein BlockHost-Konto" {
		t.Errorf("mail text: %q", got)
	}
}

func brandCSSForTest(dark map[string]string) template.CSS {
	return brandCSS(&config.Branding{Dark: dark})
}
