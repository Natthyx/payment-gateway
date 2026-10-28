// Package mock is an in-process stand-in for the real payment providers, used
// for load testing. It is only registered when PROVIDER_MODE=mock (off by
// default) and never opens a network connection.
//
// A mock registers under a real provider name ("stripe", "chapa",
// "telebirr") because payment_intents.provider has a CHECK constraint on
// those three values. Charges, refunds and transaction lookups are faked;
// webhook verification and event parsing are delegated to the real
// provider client, so signed webhooks are checked in exactly the format
// production uses. Those two methods are pure local computation.
package mock

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"

    "github.com/Natthyx/payment-gateway/internal/provider"
)

// Calls counts every mock provider call so load tests can compare provider
// side effects (e.g. refunds issued) with what the database recorded.
var Calls = promauto.NewCounterVec(prometheus.CounterOpts{
    Name: "gateway_mock_provider_calls_total",
    Help: "Calls made to the mock payment provider (PROVIDER_MODE=mock only).",
}, []string{"provider", "op"})

// WebhookVerifier is the subset of provider.Provider that the mock delegates
// to the real client. It deliberately excludes every method that would make
// a network call.
type WebhookVerifier interface {
    VerifyWebhook(payload []byte, signature string) (*provider.WebhookEvent, error)
    ParseEventMetadata(payload []byte) (*provider.EventMetadata, error)
}

// Provider fakes charges, refunds and lookups for one provider name.
type Provider struct {
    name     string
    webhooks WebhookVerifier
    latency  time.Duration
    txStatus string // status GetTransaction reports, e.g. "pending" or "succeeded"
}

// New builds a mock for providerName. latency is added to every call that
// would normally hit the network; txStatus is what GetTransaction reports.
// The mock supports refunds only if the real client does (Stripe today), so
// refund behaviour per provider matches production.
func New(providerName string, real WebhookVerifier, latency time.Duration, txStatus string) provider.Provider {
    p := &Provider{name: providerName, webhooks: real, latency: latency, txStatus: txStatus}
    if _, ok := real.(provider.Refunder); ok {
        return &RefundingProvider{p}
    }
    return p
}

func (p *Provider) Name() string { return p.name }

// CreateCharge returns a provider_tx_id derived from the request, so the same
// intent always maps to the same id, even across gateway instances.
func (p *Provider) CreateCharge(ctx context.Context, req provider.ChargeRequest) (*provider.ChargeResponse, error) {
    Calls.WithLabelValues(p.name, "create_charge").Inc()
    if err := p.wait(ctx); err != nil {
        return nil, err
    }
    txID := "mock_" + digest(p.name, req.Description, req.IdempotencyKey)
    return &provider.ChargeResponse{
        ProviderTxID: txID,
        CheckoutURL:  fmt.Sprintf("https://checkout.mock.invalid/%s/%s", p.name, txID),
        Status:       "requires_payment_method",
    }, nil
}

func (p *Provider) VerifyWebhook(payload []byte, signature string) (*provider.WebhookEvent, error) {
    return p.webhooks.VerifyWebhook(payload, signature)
}

func (p *Provider) VerifyWebhookAt(payload []byte, signature string, receivedAt time.Time) (*provider.WebhookEvent, error) {
    if rv, ok := p.webhooks.(provider.ReceiptVerifier); ok {
        return rv.VerifyWebhookAt(payload, signature, receivedAt)
    }
    return p.webhooks.VerifyWebhook(payload, signature)
}

func (p *Provider) ParseEventMetadata(payload []byte) (*provider.EventMetadata, error) {
    return p.webhooks.ParseEventMetadata(payload)
}

func (p *Provider) GetTransaction(ctx context.Context, providerTxID string) (*provider.Transaction, error) {
    Calls.WithLabelValues(p.name, "get_transaction").Inc()
    if err := p.wait(ctx); err != nil {
        return nil, err
    }
    return &provider.Transaction{
        ProviderTxID: providerTxID,
        Status:       p.txStatus,
        OccurredAt:   time.Now(),
    }, nil
}

// RefundingProvider is a mock for a provider whose real client implements
// provider.Refunder.
type RefundingProvider struct {
    *Provider
}

func (p *RefundingProvider) Refund(ctx context.Context, req provider.RefundRequest) (*provider.RefundResponse, error) {
    Calls.WithLabelValues(p.name, "refund").Inc()
    if err := p.wait(ctx); err != nil {
        return nil, err
    }
    return &provider.RefundResponse{
        ProviderRefundID: "mock_re_" + digest(p.name, req.ProviderTxID, req.IdempotencyKey, fmt.Sprint(req.Amount)),
        Status:           "succeeded",
    }, nil
}

func (p *Provider) wait(ctx context.Context) error {
    if p.latency <= 0 {
        return nil
    }
    t := time.NewTimer(p.latency)
    defer t.Stop()
    select {
    case <-t.C:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func digest(parts ...string) string {
    h := sha256.New()
    for _, s := range parts {
        h.Write([]byte(s))
        h.Write([]byte{0})
    }
    return hex.EncodeToString(h.Sum(nil))[:24]
}
