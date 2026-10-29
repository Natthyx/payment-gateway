package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// TrustedProxies resolves the client IP from X-Forwarded-For, but only when
// the request arrives from one of the configured proxy networks.
//
// chi's RealIP trusts those headers from anyone, so any client could pick
// its own address and dodge the per-IP rate limit. Here the header is used
// only if the direct peer is a trusted proxy, and it is read right to left:
// the client IP is the first address that is not itself a trusted proxy.
// With no trusted proxies configured, RemoteAddr is never changed.
type TrustedProxies struct {
	prefixes []netip.Prefix
}

// NewTrustedProxies parses CIDRs or single IPs, e.g. "10.0.0.0/8,192.168.1.5".
func NewTrustedProxies(list []string) (*TrustedProxies, error) {
	t := &TrustedProxies{}
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", s, err)
			}
			t.prefixes = append(t.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", s, err)
		}
		t.prefixes = append(t.prefixes, p.Masked())
	}
	return t, nil
}

func (t *TrustedProxies) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range t.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Handler rewrites r.RemoteAddr to the resolved client IP.
func (t *TrustedProxies) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := t.clientIP(r); ip != "" {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the resolved client IP, or "" to keep RemoteAddr.
func (t *TrustedProxies) clientIP(r *http.Request) string {
	if len(t.prefixes) == 0 {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !t.trusted(peer) {
		return ""
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return "" // malformed chain: do not guess
		}
		if !t.trusted(a) {
			return a.Unmap().String()
		}
	}
	return ""
}
