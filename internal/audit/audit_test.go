package audit

import "testing"

func TestNormalizeIP(t *testing.T) {
	cases := map[string]string{
		"172.18.0.1:60884": "172.18.0.1",
		"10.1.2.3":         "10.1.2.3",
		"[::1]:8080":       "::1",
		"2001:db8::1":      "2001:db8::1",
		"":                 "",
		"not-an-ip":        "",
		"evil:1234":        "",
	}
	for in, want := range cases {
		if got := normalizeIP(in); got != want {
			t.Errorf("normalizeIP(%q) = %q, want %q", in, got, want)
		}
	}
}
