package stripe

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "strings"
    "testing"
    "time"
)

func TestParseEventMetadata(t *testing.T) {
    c := New("sk_test", "whsec_test")
    body := []byte(`{"id":"evt_123","type":"payment_intent.succeeded"}`)
    meta, err := c.ParseEventMetadata(body)
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if meta.EventID != "evt_123" || meta.EventType != "payment_intent.succeeded" {
        t.Fatalf("bad parse: %+v", meta)
    }
}

func TestVerifyWebhook_ValidSignature(t *testing.T) {
    secret := "whsec_unit_test_secret"
    c := New("sk_test", secret)
    body := []byte(`{"id":"evt_42","type":"payment_intent.succeeded","data":{"object":{"id":"pi_x","amount":1050,"currency":"usd"}},"created":` + fmt.Sprint(time.Now().Unix()) + `}`)
    ts := time.Now().Unix()
    sig := signStripe(secret, ts, body)
    header := "t=" + fmt.Sprint(ts) + ",v1=" + sig
    ev, err := c.VerifyWebhook(body, header)
    if err != nil {
        t.Fatalf("verify: %v", err)
    }
    if ev.ProviderTxID != "pi_x" || ev.Amount != 1050 {
        t.Fatalf("bad event: %+v", ev)
    }
}

func TestVerifyWebhook_RejectsStaleTimestamp(t *testing.T) {
    secret := "whsec"
    c := New("sk_test", secret)
    body := []byte(`{}`)
    ts := time.Now().Add(-10 * time.Minute).Unix()
    sig := signStripe(secret, ts, body)
    if _, err := c.VerifyWebhook(body, fmt.Sprintf("t=%d,v1=%s", ts, sig)); err == nil {
        t.Fatal("expected stale timestamp error")
    } else if !strings.Contains(err.Error(), "skew") {
        t.Fatalf("expected skew error, got %v", err)
    }
}

func TestVerifyWebhook_RejectsBadSignature(t *testing.T) {
    c := New("sk_test", "whsec")
    body := []byte(`{}`)
    ts := time.Now().Unix()
    bad := strings.Repeat("0", 64)
    if _, err := c.VerifyWebhook(body, fmt.Sprintf("t=%d,v1=%s", ts, bad)); err == nil {
        t.Fatal("expected signature mismatch")
    }
}

func signStripe(secret string, ts int64, body []byte) string {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(fmt.Sprintf("%d.%s", ts, string(body))))
    return hex.EncodeToString(mac.Sum(nil))
}
