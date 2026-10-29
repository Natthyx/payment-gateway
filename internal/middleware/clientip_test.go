package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestTrustedProxiesClientIP(t *testing.T) {
	tp, err := NewTrustedProxies([]string{"10.0.0.0/8", "192.168.1.5"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"untrusted peer: header ignored", "203.0.113.9:4000", "1.2.3.4", ""},
		{"trusted peer: client from header", "10.1.1.1:4000", "198.51.100.7", "198.51.100.7"},
		{"spoofed leftmost entry is skipped", "10.1.1.1:4000", "6.6.6.6, 198.51.100.7", "198.51.100.7"},
		{"chain of trusted proxies", "10.1.1.1:4000", "198.51.100.7, 192.168.1.5, 10.2.2.2", "198.51.100.7"},
		{"trusted peer without header", "10.1.1.1:4000", "", ""},
		{"malformed entry", "10.1.1.1:4000", "198.51.100.7, garbage", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		r.Header.Set("X-Real-IP", "9.9.9.9") // never trusted
		if got := tp.clientIP(r); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTrustedProxiesEmptyNeverTrusts(t *testing.T) {
	tp, _ := NewTrustedProxies(nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.1.1:4000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := tp.clientIP(r); got != "" {
		t.Fatalf("got %q with no trusted proxies", got)
	}
}

func TestNewTrustedProxiesRejectsGarbage(t *testing.T) {
	if _, err := NewTrustedProxies([]string{"not-an-ip"}); err == nil {
		t.Fatal("expected error")
	}
}
