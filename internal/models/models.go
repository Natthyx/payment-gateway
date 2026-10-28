package models

import (
	"encoding/json"
	"fmt"
	"time"
)

// ── MERCHANT ──────────────────────────────────────────────────────────────────

type Merchant struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	APIKeyHash   string    `json:"-"` // bcrypt of the SECRET half only
	APIKeyLookup string    `json:"-"` // plaintext lookup; indexed
	IsActive     bool      `json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ── PAYMENT INTENT ────────────────────────────────────────────────────────────

// PaymentStatus is a typed string — prevents passing arbitrary strings as status.
// WHY typed constants: if you use plain strings, a typo like "succes" compiles
// fine but breaks silently at runtime. With typed constants, the compiler catches it.
type PaymentStatus string

const (
	StatusCreated    PaymentStatus = "created"
	StatusProcessing PaymentStatus = "processing"
	StatusSucceeded  PaymentStatus = "succeeded"
	StatusFailed     PaymentStatus = "failed"
	StatusRefunded   PaymentStatus = "refunded"
)

// Provider is the payment provider identifier.
type Provider string

const (
	ProviderStripe   Provider = "stripe"
	ProviderChapa    Provider = "chapa"
	ProviderTelebirr Provider = "telebirr"
)

type PaymentIntent struct {
	ID             int64           `json:"id"`
	MerchantID     int64           `json:"merchant_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Amount         int64           `json:"amount"`   // in smallest unit (cents/santim)
	Currency       string          `json:"currency"` // "USD", "ETB"
	Provider       Provider        `json:"provider"`
	Status         PaymentStatus   `json:"status"`
	ProviderTxID   string          `json:"provider_tx_id,omitempty"`
	FailureReason  string          `json:"failure_reason,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"` // pass-through JSONB
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// AmountInMajorUnits returns a human-readable amount.
// e.g. 1050 cents → "10.50"
// Used in logs and API responses — never for calculations.
func (p *PaymentIntent) AmountInMajorUnits() float64 {
	return float64(p.Amount) / 100.0
}

// ── LEDGER ────────────────────────────────────────────────────────────────────

type EntryType string

const (
	EntryDebit  EntryType = "debit"
	EntryCredit EntryType = "credit"
)

type LedgerEntry struct {
	ID              int64     `json:"id"`
	PaymentIntentID int64     `json:"payment_intent_id"`
	Account         string    `json:"account"` // e.g. "merchant:42", "gateway:fees"
	Amount          int64     `json:"amount"`  // positive = credit, negative = debit
	Currency        string    `json:"currency"`
	EntryType       EntryType `json:"entry_type"`
	Description     string    `json:"description"`
	CreatedAt       time.Time `json:"created_at"`
}

// LedgerPair is always created together — debit + credit.
// WHY: double-entry means you can never create one without the other.
// Enforcing this at the type level makes it impossible to forget.
type LedgerPair struct {
	Debit  LedgerEntry
	Credit LedgerEntry
}

// Validate checks that the pair balances — debit + credit must net to zero.
// If this ever returns an error, it means money was created or destroyed — a critical bug.
func (p *LedgerPair) Validate() error {
	total := p.Debit.Amount + p.Credit.Amount
	if total != 0 {
		return fmt.Errorf(
			"ledger imbalance: debit=%d credit=%d net=%d — this is a critical bug",
			p.Debit.Amount, p.Credit.Amount, total,
		)
	}
	return nil
}

// ── WEBHOOK ───────────────────────────────────────────────────────────────────

type WebhookStatus string

const (
	WebhookPending   WebhookStatus = "pending"
	WebhookProcessed WebhookStatus = "processed"
	WebhookFailed    WebhookStatus = "failed"
)

type Webhook struct {
	ID              int64           `json:"id"`
	Provider        Provider        `json:"provider"`
	ProviderEventID string          `json:"provider_event_id,omitempty"`
	EventType       string          `json:"event_type"`
	Payload         json.RawMessage `json:"payload"`
	Signature       string          `json:"signature"`
	Status          WebhookStatus   `json:"status"`
	Attempts        int             `json:"attempts"`
	LastError       string          `json:"last_error,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	ProcessedAt     *time.Time      `json:"processed_at,omitempty"` // pointer — can be NULL
}

// ── REQUEST / RESPONSE DTOs ───────────────────────────────────────────────────

type CreateMerchantRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type CreateMerchantResponse struct {
	Merchant *Merchant `json:"merchant"`
	APIKey   string    `json:"api_key"` // returned ONCE at creation, never again
}

type CreatePaymentIntentRequest struct {
	Amount         int64           `json:"amount"` // in cents/santim
	Currency       string          `json:"currency"`
	Provider       Provider        `json:"provider"`
	IdempotencyKey string          `json:"idempotency_key"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

type CreatePaymentIntentResponse struct {
	PaymentIntent *PaymentIntent `json:"payment_intent"`
	CheckoutURL   string         `json:"checkout_url"` // redirect user here to pay
}

// ── REFUND ────────────────────────────────────────────────────────────────────

type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

type Refund struct {
	ID               int64        `json:"id"`
	PaymentIntentID  int64        `json:"payment_intent_id"`
	MerchantID       int64        `json:"merchant_id"`
	IdempotencyKey   string       `json:"idempotency_key"`
	Amount           int64        `json:"amount"`
	Currency         string       `json:"currency"`
	Reason           string       `json:"reason,omitempty"`
	Status           RefundStatus `json:"status"`
	ProviderRefundID string       `json:"provider_refund_id,omitempty"`
	FailureReason    string       `json:"failure_reason,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

type CreateRefundRequest struct {
	Amount         int64  `json:"amount"` // optional; defaults to full
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

// ── API ENVELOPE ──────────────────────────────────────────────────────────────

// APIResponse is the only response shape returned by handlers.
// ErrorObj carries structured errors with stable codes (preferred for new code);
// Error is retained as a free-form string for backwards-compatibility with
// any older clients that may still be coupled to it.
type APIResponse struct {
	Success  bool        `json:"success"`
	Data     interface{} `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	ErrorObj interface{} `json:"error_obj,omitempty"`
	Meta     *Meta       `json:"meta,omitempty"`
}

type Meta struct {
	Total int `json:"total"`
	Page  int `json:"page"`
	Limit int `json:"limit"`
}
