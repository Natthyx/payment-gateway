package mock

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/Natthyx/payment-gateway/internal/provider"
	"github.com/Natthyx/payment-gateway/internal/provider/stripe"
	"github.com/Natthyx/payment-gateway/internal/provider/telebirr"
)

func TestCreateChargeIsDeterministicAndDistinct(t *testing.T) {
	p := New("stripe", stripe.New("", "whsec_test"), 0, "pending")
	req := provider.ChargeRequest{Description: "Payment #1", IdempotencyKey: "k1"}

	a, err := p.CreateCharge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.CreateCharge(context.Background(), req)
	if a.ProviderTxID != b.ProviderTxID {
		t.Fatalf("same request gave different ids: %s vs %s", a.ProviderTxID, b.ProviderTxID)
	}
	req.Description = "Payment #2"
	c, _ := p.CreateCharge(context.Background(), req)
	if c.ProviderTxID == a.ProviderTxID {
		t.Fatal("different intents got the same provider_tx_id")
	}
}

func TestVerifyWebhookDelegatesToRealClient(t *testing.T) {
	secret := "whsec_test"
	p := New("stripe", stripe.New("", secret), 0, "pending")
	body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"mock_x","amount":100,"currency":"usd"}}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	sig := fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))

	ev, err := p.VerifyWebhook(body, sig)
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if ev.ProviderTxID != "mock_x" {
		t.Fatalf("got provider tx %q", ev.ProviderTxID)
	}
	if _, err := p.VerifyWebhook(body, "t="+ts+",v1=00"); err == nil {
		t.Fatal("bad signature accepted")
	}
}

func TestLatencyRespectsContext(t *testing.T) {
	p := New("stripe", stripe.New("", ""), time.Hour, "pending")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.(provider.Refunder).Refund(ctx, provider.RefundRequest{}); err == nil {
		t.Fatal("expected context error")
	}
}

func TestRefundSupportMatchesRealClient(t *testing.T) {
	if _, ok := New("stripe", stripe.New("", ""), 0, "").(provider.Refunder); !ok {
		t.Fatal("stripe mock should support refunds")
	}
	if _, ok := New("telebirr", telebirr.New("", "", "", ""), 0, "").(provider.Refunder); ok {
		t.Fatal("telebirr mock must not support refunds; the real client does not")
	}
}
