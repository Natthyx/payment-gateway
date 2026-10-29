package merchant

import (
	"strings"
	"testing"
)

func TestParseKey_Valid(t *testing.T) {
	// 16 hex chars lookup + 48 hex chars secret
	lookup := strings.Repeat("a", apiKeyLookupLen)
	secret := strings.Repeat("b", apiKeySecretLen)
	key := "gw_live_" + lookup + "_" + secret

	env, gotLookup, gotSecret, err := ParseKey(key)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if env != "live" {
		t.Fatalf("env=%q", env)
	}
	if gotLookup != lookup {
		t.Fatalf("lookup mismatch")
	}
	if gotSecret != secret {
		t.Fatalf("secret mismatch")
	}
}

func TestParseKey_Rejects(t *testing.T) {
	cases := []string{
		"", // empty
		"live_aaaaaaaaaaaaaaaa_" + strings.Repeat("b", 48),                   // missing gw prefix
		"gw_prod_" + strings.Repeat("a", 16) + "_" + strings.Repeat("b", 48), // bad env
		"gw_live_short_" + strings.Repeat("b", 48),                           // bad lookup length
		"gw_live_" + strings.Repeat("a", 16) + "_short",                      // bad secret length
		"gw_live_zzzzzzzzzzzzzzzz_" + strings.Repeat("b", 48),                // non-hex lookup
	}
	for _, c := range cases {
		if _, _, _, err := ParseKey(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}
