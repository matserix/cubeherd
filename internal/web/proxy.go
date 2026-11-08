package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// Behind a reverse proxy every request arrives from the proxy's address. The proxy middleware believes the
// X-Forwarded-For and X-Forwarded-Proto headers only when the direct peer is one of the trusted proxies of the
// settings, and then replaces RemoteAddr by the real client, so sign-in limits, the audit log and the sessions see it.

type (
	httpsKey struct{}
	peerKey  struct{}
)

// peerOf is the address the connection really came from (the proxy, when there is one).
func peerOf(r *http.Request) string {
	if v, ok := r.Context().Value(peerKey{}).(string); ok {
		return v
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// httpsOf reports whether the client reached the dashboard over HTTPS: directly, or through a trusted proxy that said so.
func httpsOf(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	v, _ := r.Context().Value(httpsKey{}).(bool)
	return v
}

func (a *App) secure(r *http.Request) bool { return httpsOf(r) }

var proxyCache struct {
	mu   sync.Mutex
	key  string
	nets []netip.Prefix
}

// ParseProxies reads IP addresses and CIDR ranges, one per entry; entries it cannot read are returned as bad.
func ParseProxies(list []string) (nets []netip.Prefix, bad []string) {
	for _, e := range list {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			nets = append(nets, p.Masked())
		} else if ip, err := netip.ParseAddr(e); err == nil {
			nets = append(nets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
		} else {
			bad = append(bad, e)
		}
	}
	return
}

func trustedNets(list []string) []netip.Prefix {
	key := strings.Join(list, "\n")
	proxyCache.mu.Lock()
	defer proxyCache.mu.Unlock()
	if proxyCache.key != key || proxyCache.nets == nil && len(list) > 0 {
		proxyCache.nets, _ = ParseProxies(list)
		proxyCache.key = key
	}
	return proxyCache.nets
}

func inNets(ip netip.Addr, nets []netip.Prefix) bool {
	ip = ip.Unmap()
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientFrom works out the real client address and scheme of a request that came from peer.
// With no trusted proxy, or a peer that is not one, the headers are ignored.
func clientFrom(peer string, xff []string, proto []string, nets []netip.Prefix) (client string, https bool) {
	host := peer
	if h, _, err := net.SplitHostPort(peer); err == nil {
		host = h
	}
	pa, err := netip.ParseAddr(host)
	if err != nil || len(nets) == 0 || !inNets(pa, nets) {
		return host, false
	}
	// walk the chain from the right: the first address that is not a trusted proxy is the client
	var chain []string
	for _, v := range xff {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				chain = append(chain, p)
			}
		}
	}
	client = host
	for i := len(chain) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(chain[i])
		if err != nil {
			break // a garbled entry ends the walk; keep what we have
		}
		client = ip.Unmap().String()
		if !inNets(ip, nets) {
			break
		}
	}
	if n := len(proto); n > 0 {
		last := proto[n-1]
		if i := strings.LastIndex(last, ","); i >= 0 {
			last = last[i+1:]
		}
		https = strings.EqualFold(strings.TrimSpace(last), "https")
	}
	return client, https
}

// proxy is the outermost middleware.
func (a *App) proxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.Cfg.Get()
		if len(cfg.TrustedProxies) > 0 {
			nets := trustedNets(cfg.TrustedProxies)
			if pa, err := netip.ParseAddr(peerOf(r)); err == nil && inNets(pa, nets) { // only a trusted proxy is believed
				client, https := clientFrom(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), r.Header.Values("X-Forwarded-Proto"), nets)
				ctx := context.WithValue(context.WithValue(r.Context(), httpsKey{}, https), peerKey{}, peerOf(r))
				r2 := r.Clone(ctx)
				r2.RemoteAddr = client
				r = r2
			}
		}
		if cfg.HSTS && httpsOf(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func prefixStrings(nets []netip.Prefix) []string {
	var out []string
	for _, n := range nets {
		if n.IsSingleIP() {
			out = append(out, n.Addr().String())
		} else {
			out = append(out, n.String())
		}
	}
	return out
}
