package telebirr

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "crypto/sha256"
	"sort"
    "strings"
    "time"
    "github.com/Natthyx/payment-gateway/internal/provider"
)

const baseURL = "https://app.ethiomobilemoney.et:2121/ammapi/payment/service-openup"

type Client struct {
    appID      string
    appKey     string
    publicKey  string // Telebirr's RSA public key for encrypting requests
    shortCode  string
    httpClient *http.Client
}

func New(appID, appKey, publicKey, shortCode string) *Client {
    return &Client{
        appID:      appID,
        appKey:     appKey,
        publicKey:  publicKey,
        shortCode:  shortCode,
        httpClient: &http.Client{Timeout: 30 * time.Second},
    }
}

func (c *Client) Name() string { return "telebirr" }

// CreateCharge initiates a Telebirr H5 payment.
// Telebirr uses a signature scheme: sort all params alphabetically,
// concatenate as key=value&... then SHA256 hash + appKey appended.
func (c *Client) CreateCharge(ctx context.Context, req provider.ChargeRequest) (*provider.ChargeResponse, error) {
    if req.IdempotencyKey == "" {
        return nil, fmt.Errorf("telebirr: idempotency key is required")
    }
    timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
    // Telebirr nonce is capped at 8 chars. Take the leading 8 of the merchant's
    // idempotency key when long enough; otherwise pad with the timestamp so
    // short keys do not panic.
    nonce := req.IdempotencyKey
    if len(nonce) > 8 {
        nonce = nonce[:8]
    } else if len(nonce) < 8 {
        nonce = (nonce + timestamp)[:8]
    }

    params := map[string]string{
        "appId":       c.appID,
        "merch_code":  c.shortCode,
        "nonce":       nonce,
        "notifyUrl":   req.CallbackURL,
        "outTradeNo":  req.IdempotencyKey,
        "subject":     req.Description,
        "timeoutExpress": "30",
        "timestamp":   timestamp,
        "totalAmount": fmt.Sprintf("%.2f", float64(req.Amount)/100.0),
        "returnApp":   "0",
    }

    // Build signature: sort keys, join as k=v&k=v, append appKey, SHA256
    signature := c.buildSignature(params)

    body := map[string]interface{}{
        "appid":   c.appID,
        "sign":    signature,
        "ussd": params, // Telebirr wraps params in "ussd" key
    }

    jsonBody, err := json.Marshal(body)
    if err != nil {
        return nil, fmt.Errorf("telebirr: marshalling request: %w", err)
    }

    httpReq, err := http.NewRequestWithContext(ctx,
        http.MethodPost, baseURL, bytes.NewReader(jsonBody),
    )
    if err != nil {
        return nil, fmt.Errorf("telebirr: building request: %w", err)
    }
    httpReq.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("telebirr: request failed: %w", err)
    }
    defer resp.Body.Close()

    respBody, _ := io.ReadAll(resp.Body)
    var result struct {
        Code string `json:"code"`
        Msg  string `json:"msg"`
        Data struct {
            ToPayURL string `json:"toPayUrl"`
        } `json:"data"`
    }
    if err := json.Unmarshal(respBody, &result); err != nil {
        return nil, fmt.Errorf("telebirr: parsing response: %w", err)
    }
    if result.Code != "200" {
        return nil, fmt.Errorf("telebirr: API error %s: %s", result.Code, result.Msg)
    }

    return &provider.ChargeResponse{
        ProviderTxID: params["outTradeNo"],
        CheckoutURL:  result.Data.ToPayURL,
        Status:       "created",
    }, nil
}

// VerifyWebhook for Telebirr verifies the SHA256 signature on the callback.
func (c *Client) VerifyWebhook(payload []byte, signature string) (*provider.WebhookEvent, error) {
    var event struct {
        TradeNo    string `json:"trade_no"`
        OutTradeNo string `json:"out_trade_no"`
        TotalAmount string `json:"total_amount"`
        TradeStatus string `json:"trade_status"`
        Timestamp   string `json:"timestamp"`
    }
    if err := json.Unmarshal(payload, &event); err != nil {
        return nil, fmt.Errorf("telebirr: parsing webhook: %w", err)
    }

    // Telebirr signature: SHA256(sorted_params + appKey)
    params := map[string]string{
        "trade_no":     event.TradeNo,
        "out_trade_no": event.OutTradeNo,
        "total_amount": event.TotalAmount,
        "trade_status": event.TradeStatus,
        "timestamp":    event.Timestamp,
    }
    expected := c.buildSignature(params)
    if !strings.EqualFold(expected, signature) {
        return nil, fmt.Errorf("telebirr: webhook signature verification failed")
    }

    // Parse amount from string to int64 santim
    var amountFloat float64
    fmt.Sscanf(event.TotalAmount, "%f", &amountFloat)

    status := "payment.failed"
    if event.TradeStatus == "SUCCESS" {
        status = "payment.succeeded"
    }

    return &provider.WebhookEvent{
        ProviderTxID: event.OutTradeNo,
        EventType:    status,
        Amount:       int64(amountFloat * 100),
        Currency:     "ETB",
        RawPayload:   payload,
        OccurredAt:   time.Now(),
    }, nil
}

func (c *Client) GetTransaction(ctx context.Context, providerTxID string) (*provider.Transaction, error) {
    // Telebirr query uses same signature scheme
    timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
    params := map[string]string{
        "appId":      c.appID,
        "merch_code": c.shortCode,
        "outTradeNo": providerTxID,
        "timestamp":  timestamp,
    }
    signature := c.buildSignature(params)
    params["sign"] = signature

    jsonBody, _ := json.Marshal(params)
    httpReq, err := http.NewRequestWithContext(ctx,
        http.MethodPost,
        baseURL+"/queryOrder",
        bytes.NewReader(jsonBody),
    )
    if err != nil {
        return nil, fmt.Errorf("telebirr: building request: %w", err)
    }
    httpReq.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("telebirr: request failed: %w", err)
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    var result struct {
        Data struct {
            OutTradeNo  string `json:"outTradeNo"`
            TotalAmount string `json:"totalAmount"`
            TradeStatus string `json:"tradeStatus"`
        } `json:"data"`
    }
    json.Unmarshal(body, &result) //nolint:errcheck

    var amount float64
    fmt.Sscanf(result.Data.TotalAmount, "%f", &amount)

    return &provider.Transaction{
        ProviderTxID: result.Data.OutTradeNo,
        Amount:       int64(amount * 100),
        Currency:     "ETB",
        Status:       result.Data.TradeStatus,
        OccurredAt:   time.Now(),
    }, nil
}

// ParseEventMetadata derives a dedupe key from a Telebirr notify body.
// Telebirr does not emit a stable event id, so we use (out_trade_no, trade_status, timestamp).
func (c *Client) ParseEventMetadata(payload []byte) (*provider.EventMetadata, error) {
    var event struct {
        OutTradeNo  string `json:"out_trade_no"`
        TradeStatus string `json:"trade_status"`
        Timestamp   string `json:"timestamp"`
    }
    if err := json.Unmarshal(payload, &event); err != nil {
        return nil, fmt.Errorf("telebirr: parsing event metadata: %w", err)
    }
    eventType := "payment.unknown"
    switch event.TradeStatus {
    case "SUCCESS":
        eventType = "payment.succeeded"
    case "FAILED":
        eventType = "payment.failed"
    }
    var eventID string
    if event.OutTradeNo != "" {
        eventID = event.OutTradeNo + ":" + event.TradeStatus + ":" + event.Timestamp
    }
    return &provider.EventMetadata{EventID: eventID, EventType: eventType}, nil
}

// buildSignature sorts params alphabetically, joins as k=v&..., appends appKey, SHA256s.
// This is Telebirr's specific signing algorithm.
func (c *Client) buildSignature(params map[string]string) string {
    keys := make([]string, 0, len(params))
    for k := range params {
        keys = append(keys, k)
    }
    sort.Strings(keys)

    var parts []string
    for _, k := range keys {
        parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
    }

    raw := strings.Join(parts, "&") + c.appKey
    hash := sha256.Sum256([]byte(raw))
    return fmt.Sprintf("%x", hash)
}