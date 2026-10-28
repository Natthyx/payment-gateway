package webhook

import (
    "testing"
    "time"
)

func TestWebhookBackoff(t *testing.T) {
    want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second}
    for i, w := range want {
        if got := webhookBackoff(i + 1); got != w {
            t.Errorf("webhookBackoff(%d) = %s, want %s", i+1, got, w)
        }
    }
}
