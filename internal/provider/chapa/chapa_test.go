package chapa

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "testing"
)

func TestParseEventMetadata(t *testing.T) {
    c := New("CHAPUBK_TEST")
    body := []byte(`{"event":"charge.success","data":{"tx_ref":"tx_1","status":"success"}}`)
    meta, err := c.ParseEventMetadata(body)
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    if meta.EventID != "tx_1:success" || meta.EventType != "charge.success" {
        t.Fatalf("bad parse: %+v", meta)
    }
}

func TestVerifyWebhook(t *testing.T) {
    secret := "CHAPUBK_TEST"
    body := []byte(`{"event":"charge.success","data":{"tx_ref":"tx_1","amount":10.50,"currency":"ETB","status":"success"}}`)
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    sig := hex.EncodeToString(mac.Sum(nil))

    c := New(secret)
    ev, err := c.VerifyWebhook(body, sig)
    if err != nil {
        t.Fatalf("verify: %v", err)
    }
    if ev.Amount != 1050 || ev.Currency != "ETB" {
        t.Fatalf("bad event: %+v", ev)
    }
}
