package payment

import (
    "fmt"
    "strings"

    "github.com/Natthyx/payment-gateway/internal/apierr"
    "github.com/Natthyx/payment-gateway/internal/models"
)

// supportedCurrencyByProvider enforces that callers pick a currency the
// chosen provider can actually settle. Stripe is multi-currency; Chapa and
// Telebirr only do ETB. Rejecting bad combinations at the boundary saves
// a downstream provider error after we've already taken the payment intent.
var supportedCurrencyByProvider = map[models.Provider]map[string]bool{
    models.ProviderStripe:   {"USD": true, "EUR": true, "GBP": true, "ETB": true},
    models.ProviderChapa:    {"ETB": true},
    models.ProviderTelebirr: {"ETB": true},
}

const (
    minAmountInSmallestUnit int64 = 50           // never accept dust below 50¢/santim
    maxAmountInSmallestUnit int64 = 100_000_0000 // 100M units, hard ceiling per payment
)

// ValidateCreatePaymentIntent enforces inviolable invariants. Anything that
// reaches the provider call must be well-formed and currency-compatible.
func ValidateCreatePaymentIntent(req *models.CreatePaymentIntentRequest) error {
    if req.Amount <= 0 {
        return apierr.InvalidRequest("amount must be > 0")
    }
    if req.Amount < minAmountInSmallestUnit {
        return apierr.InvalidRequest(fmt.Sprintf("amount must be at least %d (smallest currency unit)", minAmountInSmallestUnit))
    }
    if req.Amount > maxAmountInSmallestUnit {
        return apierr.InvalidRequest("amount exceeds per-payment ceiling")
    }
    req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
    if req.Currency == "" {
        return apierr.InvalidRequest("currency is required")
    }
    if len(req.Currency) != 3 {
        return apierr.InvalidRequest("currency must be a 3-letter ISO 4217 code")
    }
    if req.Provider == "" {
        return apierr.InvalidRequest("provider is required")
    }
    allowed, ok := supportedCurrencyByProvider[req.Provider]
    if !ok {
        return apierr.InvalidRequest(fmt.Sprintf("unknown provider %q", req.Provider))
    }
    if !allowed[req.Currency] {
        return apierr.InvalidRequest(fmt.Sprintf("provider %s does not support currency %s", req.Provider, req.Currency))
    }
    req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
    if req.IdempotencyKey == "" {
        return apierr.InvalidRequest("idempotency_key is required (header Idempotency-Key or body)")
    }
    if len(req.IdempotencyKey) > 255 {
        return apierr.InvalidRequest("idempotency_key must be <= 255 chars")
    }
    return nil
}
