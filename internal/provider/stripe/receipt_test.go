package stripe

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "strconv"
    "testing"
    "time"
)

func sign(secret string, ts int64, body []byte) string {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + string(body)))
    return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyWebhookAtUsesReceiptTime(t *testing.T) {
    c := New("", "whsec_test")
    body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1"}}}`)
    received := time.Now().Add(-20 * time.Minute) // a webhook received 20 min ago, processed now
    sig := sign("whsec_test", received.Unix(), body)

    if _, err := c.VerifyWebhook(body, sig); err == nil {
        t.Fatal("expected VerifyWebhook to reject a 20-minute-old timestamp")
    }
    if _, err := c.VerifyWebhookAt(body, sig, received); err != nil {
        t.Fatalf("VerifyWebhookAt(receivedAt) rejected a webhook that was fresh on receipt: %v", err)
    }
}

func TestVerifyWebhookRejectsEmptySecret(t *testing.T) {
    c := New("", "")
    body := []byte(`{}`)
    if _, err := c.VerifyWebhook(body, sign("", time.Now().Unix(), body)); err == nil {
        t.Fatal("a webhook signed with an empty secret must not verify")
    }
}
