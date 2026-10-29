package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Natthyx/payment-gateway/internal/provider"
)

// stripeMaxTimestampSkew is the maximum tolerated clock skew between
// Stripe's webhook timestamp and our server clock. Matches Stripe's
// own recommendation in the official docs.
const stripeMaxTimestampSkew = 5 * time.Minute

// Client implements provider.Provider for Stripe.
type Client struct {
	secretKey  string
	webhookKey string // Stripe webhook signing secret (whsec_...)
	httpClient *http.Client
}

func New(secretKey, webhookKey string) *Client {
	return &Client{
		secretKey:  secretKey,
		webhookKey: webhookKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Name() string { return "stripe" }

// CreateCharge calls Stripe's PaymentIntents API.
// We use form-encoded POST — Stripe's API doesn't use JSON.
func (c *Client) CreateCharge(ctx context.Context, req provider.ChargeRequest) (*provider.ChargeResponse, error) {
	params := url.Values{}
	params.Set("amount", fmt.Sprintf("%d", req.Amount))
	params.Set("currency", strings.ToLower(req.Currency))
	params.Set("payment_method_types[]", "card")
	params.Set("description", req.Description)
	params.Set("receipt_email", req.CustomerEmail)
	// Idempotency is handled via header (see below)

	httpReq, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		"https://api.stripe.com/v1/payment_intents",
		strings.NewReader(params.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("stripe: building request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey) // Stripe honours this natively
	httpReq.SetBasicAuth(c.secretKey, "")                     // Stripe uses HTTP Basic Auth with secret key

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stripe: executing request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var stripeErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(body, &stripeErr) //nolint:errcheck
		return nil, fmt.Errorf("stripe: API error %d: %s", resp.StatusCode, stripeErr.Error.Message)
	}

	var result struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("stripe: parsing response: %w", err)
	}

	return &provider.ChargeResponse{
		ProviderTxID: result.ID,
		// In production you'd build a hosted checkout URL with Stripe Checkout Sessions.
		// For now we return the client secret — frontend uses Stripe.js to complete payment.
		CheckoutURL: fmt.Sprintf("https://checkout.stripe.com/pay/%s", result.ClientSecret),
		Status:      result.Status,
	}, nil
}

// VerifyWebhook validates Stripe's webhook signature using HMAC-SHA256.
// Stripe sends: "t=timestamp,v1=signature" in the Stripe-Signature header.
//
// WHY verify signatures:
//
//	Without this, any attacker can POST a fake "payment_intent.succeeded"
//	to your webhook endpoint and get goods/services without paying.
func (c *Client) VerifyWebhook(payload []byte, signature string) (*provider.WebhookEvent, error) {
	return c.VerifyWebhookAt(payload, signature, time.Now())
}

// VerifyWebhookAt is VerifyWebhook with the replay window measured from
// receivedAt instead of now.
func (c *Client) VerifyWebhookAt(payload []byte, signature string, receivedAt time.Time) (*provider.WebhookEvent, error) {
	if c.webhookKey == "" {
		return nil, fmt.Errorf("stripe: webhook secret not configured")
	}
	// Parse "t=1234,v1=abcdef" header
	var timestamp, sig string
	for _, part := range strings.Split(signature, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			sig = kv[1]
		}
	}
	if timestamp == "" || sig == "" {
		return nil, fmt.Errorf("stripe: malformed signature header")
	}

	// Reject stale or far-future webhooks to prevent signed-replay attacks.
	// Without this, a leaked signed body remains valid forever.
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stripe: bad signature timestamp: %w", err)
	}
	skew := receivedAt.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > stripeMaxTimestampSkew {
		return nil, fmt.Errorf("stripe: timestamp outside %s skew window", stripeMaxTimestampSkew)
	}

	mac := hmac.New(sha256.New, []byte(c.webhookKey))
	mac.Write([]byte(timestamp + "." + string(payload)))
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return nil, fmt.Errorf("stripe: webhook signature verification failed")
	}

	// Parse the event
	var event struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID       string `json:"id"`
				Amount   int64  `json:"amount"`
				Currency string `json:"currency"`
			} `json:"object"`
		} `json:"data"`
		Created int64 `json:"created"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("stripe: parsing webhook: %w", err)
	}

	return &provider.WebhookEvent{
		ProviderTxID: event.Data.Object.ID,
		EventType:    event.Type,
		Amount:       event.Data.Object.Amount,
		Currency:     strings.ToUpper(event.Data.Object.Currency),
		RawPayload:   payload,
		OccurredAt:   time.Unix(event.Created, 0),
	}, nil
}

func (c *Client) GetTransaction(ctx context.Context, providerTxID string) (*provider.Transaction, error) {
	httpReq, err := http.NewRequestWithContext(ctx,
		http.MethodGet,
		fmt.Sprintf("https://api.stripe.com/v1/payment_intents/%s", providerTxID),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("stripe: building request: %w", err)
	}
	httpReq.SetBasicAuth(c.secretKey, "")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stripe: executing request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		ID       string `json:"id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
		Created  int64  `json:"created"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("stripe: parsing transaction: %w", err)
	}

	return &provider.Transaction{
		ProviderTxID: result.ID,
		Amount:       result.Amount,
		Currency:     strings.ToUpper(result.Currency),
		Status:       result.Status,
		OccurredAt:   time.Unix(result.Created, 0),
	}, nil
}

// Refund issues a Stripe refund against a PaymentIntent.
// Uses Stripe's native Idempotency-Key header so retries are safe.
func (c *Client) Refund(ctx context.Context, req provider.RefundRequest) (*provider.RefundResponse, error) {
	params := url.Values{}
	params.Set("payment_intent", req.ProviderTxID)
	if req.Amount > 0 {
		params.Set("amount", fmt.Sprintf("%d", req.Amount))
	}
	if req.Reason != "" {
		params.Set("metadata[reason]", req.Reason)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.stripe.com/v1/refunds", strings.NewReader(params.Encode()))
	if err != nil {
		return nil, fmt.Errorf("stripe refund: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	httpReq.SetBasicAuth(c.secretKey, "")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stripe refund: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stripe refund: status %d body=%s", resp.StatusCode, string(body))
	}
	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("stripe refund parse: %w", err)
	}
	return &provider.RefundResponse{ProviderRefundID: result.ID, Status: result.Status}, nil
}

// ParseEventMetadata extracts Stripe's `event.id` + `event.type` from the raw
// JSON body without verifying the signature. We dedupe on event.id which Stripe
// guarantees is stable across redeliveries (format: "evt_...").
func (c *Client) ParseEventMetadata(payload []byte) (*provider.EventMetadata, error) {
	var event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("stripe: parsing event metadata: %w", err)
	}
	return &provider.EventMetadata{EventID: event.ID, EventType: event.Type}, nil
}
