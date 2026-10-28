package payment

import (
	"testing"

	"github.com/Natthyx/payment-gateway/internal/models"
)

func TestValidateCreatePaymentIntent_Accepts(t *testing.T) {
	req := &models.CreatePaymentIntentRequest{
		Amount:         1000,
		Currency:       "usd",
		Provider:       models.ProviderStripe,
		IdempotencyKey: "abc",
	}
	if err := ValidateCreatePaymentIntent(req); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if req.Currency != "USD" {
		t.Fatalf("currency should be uppercased")
	}
}

func TestValidateCreatePaymentIntent_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   models.CreatePaymentIntentRequest
	}{
		{"zero amount", models.CreatePaymentIntentRequest{Amount: 0, Currency: "USD", Provider: models.ProviderStripe, IdempotencyKey: "k"}},
		{"dust amount", models.CreatePaymentIntentRequest{Amount: 10, Currency: "USD", Provider: models.ProviderStripe, IdempotencyKey: "k"}},
		{"missing currency", models.CreatePaymentIntentRequest{Amount: 1000, Currency: "", Provider: models.ProviderStripe, IdempotencyKey: "k"}},
		{"bad currency len", models.CreatePaymentIntentRequest{Amount: 1000, Currency: "USDD", Provider: models.ProviderStripe, IdempotencyKey: "k"}},
		{"missing provider", models.CreatePaymentIntentRequest{Amount: 1000, Currency: "USD", Provider: "", IdempotencyKey: "k"}},
		{"chapa rejects usd", models.CreatePaymentIntentRequest{Amount: 1000, Currency: "USD", Provider: models.ProviderChapa, IdempotencyKey: "k"}},
		{"telebirr rejects usd", models.CreatePaymentIntentRequest{Amount: 1000, Currency: "USD", Provider: models.ProviderTelebirr, IdempotencyKey: "k"}},
		{"missing idempotency", models.CreatePaymentIntentRequest{Amount: 1000, Currency: "USD", Provider: models.ProviderStripe, IdempotencyKey: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.in
			if err := ValidateCreatePaymentIntent(&r); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}
