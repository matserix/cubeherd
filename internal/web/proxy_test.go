package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"cubeherd/internal/auth"
	"cubeherd/internal/config"
)

func nets(t *testing.T, list ...string) []netip.Prefix {
	t.Helper()
	n, bad := ParseProxies(list)
	if len(bad) > 0 {
		t.Fatalf("bad entries %v", bad)
	}
	return n
}

func TestClientFromTrustsHeadersOnlyFromTrustedProxies(t *testing.T) {
	proxy := nets(t, "10.0.0.5", "172.18.0.0/16")
	for _, c := range []struct {
		name, peer string
		xff, proto []string
		want       string
		https      bool
	}{
		{"direct connection, no proxy configured", "203.0.113.9:5000", []string{"1.2.3.4"}, []string{"https"}, "203.0.113.9", false},
		{"header from a stranger is ignored", "203.0.113.9:5000", []string{"1.2.3.4"}, []string{"https"}, "203.0.113.9", false},
		{"one trusted proxy", "10.0.0.5:4000", []string{"198.51.100.7"}, []string{"https"}, "198.51.100.7", true},
		{"chain through two proxies", "10.0.0.5:4000", []string{"198.51.100.7, 172.18.0.3"}, []string{"https"}, "198.51.100.7", true},
		{"a forged leftmost entry cannot win", "10.0.0.5:4000", []string{"6.6.6.6, 198.51.100.7"}, nil, "198.51.100.7", false},
		{"several header lines", "10.0.0.5:4000", []string{"6.6.6.6", "198.51.100.7"}, nil, "198.51.100.7", false},
		{"no header from the proxy", "10.0.0.5:4000", nil, nil, "10.0.0.5", false},
		{"garbled entry falls back to the proxy", "10.0.0.5:4000", []string{"not-an-ip"}, nil, "10.0.0.5", false},
		{"ipv6 client", "10.0.0.5:4000", []string{"2001:db8::7"}, []string{"http"}, "2001:db8::7", false},
		{"ipv4-mapped peer", "[::ffff:10.0.0.5]:4000", []string{"198.51.100.7"}, nil, "198.51.100.7", false},
		{"scheme list takes the last", "10.0.0.5:4000", []string{"198.51.100.7"}, []string{"http, https"}, "198.51.100.7", true},
	} {
		got, https := clientFrom(c.peer, c.xff, c.proto, proxy)
		if c.name == "direct connection, no proxy configured" {
			got, https = clientFrom(c.peer, c.xff, c.proto, nil)
		}
		if got != c.want || https != c.https {
			t.Errorf("%s: client=%q https=%v, want %q %v", c.name, got, https, c.want, c.https)
		}
	}
}

func TestParseProxies(t *testing.T) {
	n, bad := ParseProxies([]string{"127.0.0.1", " 10.0.0.0/8 ", "", "::1", "nonsense", "300.1.1.1"})
	if len(n) != 3 || len(bad) != 2 || bad[0] != "nonsense" {
		t.Fatalf("%v %v", n, bad)
	}
	if got := prefixStrings(n); got[0] != "127.0.0.1" || got[1] != "10.0.0.0/8" || got[2] != "::1" {
		t.Fatalf("normalised: %v", got)
	}
}

// The middleware must turn a request through a trusted proxy into one from the real client, and leave other requests alone.
func TestProxyMiddlewareRewritesTheClient(t *testing.T) {
	d := newDashboard(t, false)
	d.cfg.Update(func(s *config.Settings) { s.TrustedProxies = []string{"192.0.2.10"}; s.HSTS = true })
	var gotAddr string
	var gotHTTPS bool
	h := d.app.proxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotAddr, gotHTTPS = r.RemoteAddr, httpsOf(r) }))
	do := func(peer string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = peer
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do("192.0.2.10:1234", map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"})
	if gotAddr != "198.51.100.7" || !gotHTTPS || rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatalf("through the proxy: %q https=%v hsts=%q", gotAddr, gotHTTPS, rec.Header().Get("Strict-Transport-Security"))
	}
	rec = do("203.0.113.9:1234", map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"})
	if gotAddr != "203.0.113.9:1234" || gotHTTPS || rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("from a stranger: %q https=%v", gotAddr, gotHTTPS)
	}
}

func TestSessionCookieIsSecureOnlyOverHTTPS(t *testing.T) {
	d := newDashboard(t, false)
	d.cfg.Update(func(s *config.Settings) { s.TrustedProxies = []string{"192.0.2.10"} })
	login := func(hdr map[string]string, tls bool) *http.Cookie {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"password": {"correct horse battery"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.com")
		req.Host = "example.com"
		req.RemoteAddr = "192.0.2.10:5555"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		d.h.ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == auth.Cookie {
				return c
			}
		}
		t.Fatalf("no session cookie: %d", rec.Code)
		return nil
	}
	if c := login(map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"}, false); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("over HTTPS the cookie must be Secure, HttpOnly and Strict: %+v", c)
	}
	if c := login(map[string]string{"X-Forwarded-For": "198.51.100.7"}, false); c.Secure {
		t.Error("over plain HTTP the cookie must not be Secure, or the browser would drop it")
	}
}

func TestSignInLimitUsesTheRealClient(t *testing.T) {
	d := newDashboard(t, false)
	d.cfg.Update(func(s *config.Settings) { s.TrustedProxies = []string{"192.0.2.10"} })
	try := func(client, pw string) int {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"password": {pw}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", client)
		req.Host = "example.com"
		req.RemoteAddr = "192.0.2.10:5555"
		rec := httptest.NewRecorder()
		d.h.ServeHTTP(rec, req)
		return len(rec.Result().Cookies())
	}
	for i := 0; i < 6; i++ {
		try("198.51.100.1", "wrong password") // one attacker fails six times
	}
	if try("198.51.100.1", "correct horse battery") != 0 {
		t.Fatal("the attacker should be throttled even with the right password")
	}
	if try("198.51.100.2", "correct horse battery") == 0 {
		t.Fatal("another client behind the same proxy must not be locked out")
	}
}

func TestProxySettingsAreValidatedAndShown(t *testing.T) {
	d := newDashboard(t, false)
	form := func(proxies string) url.Values {
		return url.Values{"trustedProxies": {proxies}, "hsts": {"on"}, "portMin": {"25565"}, "portMax": {"25600"}, "defaultMemoryGb": {"4"}, "sessionDays": {"7"}, "language": {"en"}, "updateCheck": {"on"}}
	}
	rec := d.do("POST", "/settings", form("10.0.0.1\nbanana"), true)
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "crit") || len(d.cfg.Get().TrustedProxies) != 0 {
		t.Fatalf("an invalid entry must be refused: %v", rec.Header())
	}
	d.do("POST", "/settings", form("10.0.0.1\n172.18.0.0/16"), true)
	got := d.cfg.Get()
	if len(got.TrustedProxies) != 2 || got.TrustedProxies[1] != "172.18.0.0/16" || !got.HSTS {
		t.Fatalf("stored: %+v", got.TrustedProxies)
	}
	if body := d.do("GET", "/settings", nil, false).Body.String(); !strings.Contains(body, "172.18.0.0/16") || !strings.Contains(body, "HTTP") {
		t.Fatal("the settings page should list the proxies and the request's address")
	}
}
