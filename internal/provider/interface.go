package provider

import (
    "context"
    "fmt"
    "time"
)

// EventMetadata is extracted from a raw webhook body BEFORE signature
// verification so we can dedupe re-deliveries via INSERT ... ON CONFLICT.
// EventID is the provider's stable identifier for the event; if a provider
// does not supply one, callers should fall back to a content hash.
type EventMetadata struct {
    EventID   string
    EventType string
}

// ── CORE TYPES ────────────────────────────────────────────────────────────────

// ChargeRequest is the unified input for all providers.
// WHY unified: Stripe, Chapa, and Telebirr all have different APIs.
// We translate THIS struct into each provider's format internally.
// Business logic never knows which provider it's talking to.
type ChargeRequest struct {
    Amount         int64             // in smallest unit (cents/santim)
    Currency       string            // "USD", "ETB"
    IdempotencyKey string            // passed to provider where supported
    CustomerEmail  string
    CustomerName   string
    Description    string
    CallbackURL    string            // where provider redirects after payment
    Metadata       map[string]string // arbitrary pass-through
}

// ChargeResponse is the unified output from all providers.
type ChargeResponse struct {
    ProviderTxID string // provider's own transaction ID
    CheckoutURL  string // redirect user here to complete payment
    Status       string // initial status from provider
}

// WebhookEvent is the normalised event after parsing a provider's webhook.
// Raw webhook bodies differ wildly between providers — we normalise them here.
type WebhookEvent struct {
    ProviderTxID string
    EventType    string        // "payment.succeeded", "payment.failed"
    Amount       int64
    Currency     string
    RawPayload   []byte        // original body preserved for audit
    OccurredAt   time.Time
}

// Transaction is a normalised view of a provider transaction for reconciliation.
type Transaction struct {
    ProviderTxID string
    Amount       int64
    Currency     string
    Status       string
    OccurredAt   time.Time
}

// RefundRequest is the unified refund input.
type RefundRequest struct {
    ProviderTxID   string
    Amount         int64  // smallest unit; if 0 the provider should refund full
    Reason         string
    IdempotencyKey string
}

// RefundResponse is the unified refund output.
type RefundResponse struct {
    ProviderRefundID string
    Status           string // "succeeded" | "pending" | "failed"
}

// Refunder is implemented by providers that support refunds.
// Not all third-party APIs do (Telebirr historically does not), so we keep
// this as an optional capability rather than baking it into Provider.
type Refunder interface {
    Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error)
}

// ReceiptVerifier is implemented by providers whose signatures carry a
// timestamp. The inbox worker verifies stored webhooks against the time they
// were RECEIVED, so a backlog longer than the replay window does not turn
// genuine events into verification failures.
type ReceiptVerifier interface {
    VerifyWebhookAt(payload []byte, signature string, receivedAt time.Time) (*WebhookEvent, error)
}

// ── THE INTERFACE ─────────────────────────────────────────────────────────────

// Provider is the Strategy Pattern interface.
//
// WHY this matters (Bloomberg explanation):
//   This is Dependency Inversion — high-level business logic (payment service)
//   depends on this abstraction, not on Stripe or Chapa directly.
//   To add a new provider (e.g. M-Pesa), you implement this interface.
//   To switch providers, you change one config value.
//   Zero changes to business logic. Zero risk of regression.
//
// This is identical in principle to how Bloomberg abstracts
// market data feeds — one interface, many underlying sources.
type Provider interface {
    // Name returns the provider identifier e.g. "stripe"
    Name() string

    // CreateCharge initiates a payment and returns a checkout URL.
    CreateCharge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error)

    // VerifyWebhook validates the webhook signature and returns a normalised event.
    // CRITICAL: always verify signatures — without this, anyone can fake a
    // "payment succeeded" webhook and get goods for free.
    VerifyWebhook(payload []byte, signature string) (*WebhookEvent, error)

    // ParseEventMetadata extracts a stable event id + event type from the raw
    // body WITHOUT verifying the signature. Used for inbox-pattern dedupe of
    // re-deliveries. Returns (nil, nil) if the body is unparseable — callers
    // fall back to a content hash for dedupe.
    ParseEventMetadata(payload []byte) (*EventMetadata, error)

    // GetTransaction fetches the current state of a transaction from the provider.
    // Used by the reconciliation engine to detect mismatches.
    GetTransaction(ctx context.Context, providerTxID string) (*Transaction, error)
}

// ── REGISTRY ──────────────────────────────────────────────────────────────────

// Registry holds all registered providers, looked up by name.
// WHY a registry instead of a switch statement:
//   A switch statement requires changing core code to add a provider.
//   A registry is open for extension, closed for modification — the Open/Closed Principle.
type Registry struct {
    providers map[string]Provider
}

func NewRegistry() *Registry {
    return &Registry{providers: make(map[string]Provider)}
}

// Register adds a provider to the registry.
func (r *Registry) Register(p Provider) {
    r.providers[p.Name()] = p
}

// Get retrieves a provider by name. Returns error if not found or not registered.
func (r *Registry) Get(name string) (Provider, error) {
    p, ok := r.providers[name]
    if !ok {
        return nil, fmt.Errorf("provider %q not registered", name)
    }
    return p, nil
}