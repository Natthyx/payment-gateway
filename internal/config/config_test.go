package config

import "testing"

func TestCallbackURL_TrimsTrailingSlash(t *testing.T) {
    c := &Config{BaseCallbackURL: "https://example.com/"}
    got := c.CallbackURL("stripe")
    if got != "https://example.com/webhooks/stripe" {
        t.Fatalf("got %q", got)
    }
}

func TestCallbackURL_NoSlash(t *testing.T) {
    c := &Config{BaseCallbackURL: "https://example.com"}
    if got := c.CallbackURL("chapa"); got != "https://example.com/webhooks/chapa" {
        t.Fatalf("got %q", got)
    }
}
