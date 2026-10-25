package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.5.2", "0.5.1", true},
		{"v0.5.1", "0.5.1", false},
		{"v0.5.0", "0.5.1", false},
		{"v0.10.0", "0.9.9", true},
		{"v1.0.0", "0.9.9", true},
		{"v0.6", "0.5.9", true},
		{"v0.6.0", "dev", false},
		{"nonsense", "0.1.0", false},
		{"v0.6.0-rc1", "0.5.0", true},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v9.0.0","name":"Nine","body":"  - things\n","html_url":"https://x/y","draft":false,"prerelease":false}`))
	}))
	defer srv.Close()
	c := NewWithURL(srv.URL)
	rel, err := c.fetch(context.Background())
	if err != nil || rel.Tag != "v9.0.0" || rel.Notes != "- things" || rel.URL != "https://x/y" {
		t.Fatalf("got %+v, %v", rel, err)
	}
}
