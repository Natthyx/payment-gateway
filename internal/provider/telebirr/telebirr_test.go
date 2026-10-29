package telebirr

import (
	"context"
	"testing"
	"time"

	"github.com/Natthyx/payment-gateway/internal/provider"
)

func TestParseEventMetadata(t *testing.T) {
	c := New("appid", "appkey", "pubkey", "shortcode")
	body := []byte(`{"out_trade_no":"abc","trade_status":"SUCCESS","timestamp":"1000"}`)
	meta, err := c.ParseEventMetadata(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if meta.EventID != "abc:SUCCESS:1000" || meta.EventType != "payment.succeeded" {
		t.Fatalf("bad parse: %+v", meta)
	}
}

func TestCreateCharge_RejectsEmptyIdempotency(t *testing.T) {
	c := New("appid", "appkey", "pubkey", "shortcode")
	_, err := c.CreateCharge(context.Background(), provider.ChargeRequest{IdempotencyKey: "", Amount: 1000})
	if err == nil {
		t.Fatal("expected error on empty idempotency key")
	}
}

func TestCreateCharge_NoPanicOnShortIdempotency(t *testing.T) {
	// Fail fast via a 1ns context — we only care that request construction
	// does not panic on idempotency keys shorter than 8 chars.
	c := New("appid", "appkey", "pubkey", "shortcode")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on short key: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	_, _ = c.CreateCharge(ctx, provider.ChargeRequest{
		IdempotencyKey: "x",
		Amount:         1000,
		Currency:       "ETB",
		Description:    "test",
		CallbackURL:    "https://example.com",
	})
}
