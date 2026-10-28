package chapa

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "math"
    "net/http"
    "time"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
    "github.com/Natthyx/payment-gateway/internal/provider"
)

const baseURL = "https://api.chapa.co/v1"

type Client struct {
    secretKey  string
    httpClient *http.Client
}

func New(secretKey string) *Client {
    return &Client{
        secretKey:  secretKey,
        httpClient: &http.Client{Timeout: 30 * time.Second},
    }
}

func (c *Client) Name() string { return "chapa" }

func (c *Client) CreateCharge(ctx context.Context, req provider.ChargeRequest) (*provider.ChargeResponse, error) {
    body := map[string]interface{}{
        "amount":           fmt.Sprintf("%.2f", float64(req.Amount)/100.0),
        "currency":         req.Currency,
        "email":            req.CustomerEmail,
        "first_name":       req.CustomerName,
        "tx_ref":           req.IdempotencyKey, // Chapa uses tx_ref as idempotency key
        "callback_url":     req.CallbackURL,
        "customization[title]":       req.Description,
    }

    jsonBody, err := json.Marshal(body)
    if err != nil {
        return nil, fmt.Errorf("chapa: marshalling request: %w", err)
    }

    httpReq, err := http.NewRequestWithContext(ctx,
        http.MethodPost,
        baseURL+"/transaction/initialize",
        bytes.NewReader(jsonBody),
    )
    if err != nil {
        return nil, fmt.Errorf("chapa: building request: %w", err)
    }

    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("Authorization", "Bearer "+c.secretKey)

    resp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("chapa: executing request: %w", err)
    }
    defer resp.Body.Close()

    respBody, _ := io.ReadAll(resp.Body)

    var result struct {
        Status  string `json:"status"`
        Message string `json:"message"`
        Data    struct {
            CheckoutURL string `json:"checkout_url"`
        } `json:"data"`
    }
    if err := json.Unmarshal(respBody, &result); err != nil {
        return nil, fmt.Errorf("chapa: parsing response: %w", err)
    }
    if result.Status != "success" {
        return nil, fmt.Errorf("chapa: API error: %s", result.Message)
    }

    return &provider.ChargeResponse{
        ProviderTxID: req.IdempotencyKey, // Chapa uses tx_ref as the ID
        CheckoutURL:  result.Data.CheckoutURL,
        Status:       "created",
    }, nil
}

// VerifyWebhook for Chapa uses HMAC-SHA256 on the raw payload.
func (c *Client) VerifyWebhook(payload []byte, signature string) (*provider.WebhookEvent, error) {
    if c.secretKey == "" {
        return nil, fmt.Errorf("chapa: webhook secret not configured")
    }
    mac := hmac.New(sha256.New, []byte(c.secretKey))
    mac.Write(payload)
    expected := hex.EncodeToString(mac.Sum(nil))

    if !hmac.Equal([]byte(expected), []byte(signature)) {
        return nil, fmt.Errorf("chapa: webhook signature verification failed")
    }

    var event struct {
        Event string `json:"event"`
        Data  struct {
            TxRef    string  `json:"tx_ref"`
            Amount   float64 `json:"amount"`
            Currency string  `json:"currency"`
            Status   string  `json:"status"`
        } `json:"data"`
    }
    if err := json.Unmarshal(payload, &event); err != nil {
        return nil, fmt.Errorf("chapa: parsing webhook: %w", err)
    }

    return &provider.WebhookEvent{
        ProviderTxID: event.Data.TxRef,
        EventType:    event.Event,
        Amount:       int64(math.Round(event.Data.Amount * 100)), // convert to santim
        Currency:     event.Data.Currency,
        RawPayload:   payload,
        OccurredAt:   time.Now(),
    }, nil
}

func (c *Client) GetTransaction(ctx context.Context, providerTxID string) (*provider.Transaction, error) {
    httpReq, err := http.NewRequestWithContext(ctx,
        http.MethodGet,
        fmt.Sprintf("%s/transaction/verify/%s", baseURL, providerTxID),
        nil,
    )
    if err != nil {
        return nil, fmt.Errorf("chapa: building request: %w", err)
    }
    httpReq.Header.Set("Authorization", "Bearer "+c.secretKey)

    resp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("chapa: request failed: %w", err)
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    var result struct {
        Data struct {
            TxRef    string  `json:"tx_ref"`
            Amount   float64 `json:"amount"`
            Currency string  `json:"currency"`
            Status   string  `json:"status"`
        } `json:"data"`
    }
    if err := json.Unmarshal(body, &result); err != nil {
        return nil, fmt.Errorf("chapa: parsing response: %w", err)
    }

    return &provider.Transaction{
        ProviderTxID: result.Data.TxRef,
        Amount:       int64(result.Data.Amount * 100),
        Currency:     result.Data.Currency,
        Status:       result.Data.Status,
        OccurredAt:   time.Now(),
    }, nil
}

// ParseEventMetadata derives a dedupe key from a Chapa webhook body.
// Chapa does not emit a stable event id, so we use tx_ref + status:
// the same (tx_ref, status) tuple should never legitimately arrive twice.
func (c *Client) ParseEventMetadata(payload []byte) (*provider.EventMetadata, error) {
    var event struct {
        Event string `json:"event"`
        Data  struct {
            TxRef  string `json:"tx_ref"`
            Status string `json:"status"`
        } `json:"data"`
    }
    if err := json.Unmarshal(payload, &event); err != nil {
        return nil, fmt.Errorf("chapa: parsing event metadata: %w", err)
    }
    eventType := event.Event
    if eventType == "" {
        eventType = "chapa." + event.Data.Status
    }
    var eventID string
    if event.Data.TxRef != "" {
        eventID = event.Data.TxRef + ":" + event.Data.Status
    }
    return &provider.EventMetadata{EventID: eventID, EventType: eventType}, nil
}