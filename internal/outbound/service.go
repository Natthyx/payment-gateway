// Package outbound delivers events from us to merchant-controlled URLs.
//
// Contract with merchants:
//   - POST application/json
//   - Header X-Gateway-Signature: t=<unix>,v1=<hex(HMAC-SHA256(secret, t + "." + body))>
//   - Idempotency-Key header so retries are safe.
//   - 2xx response = delivered. Anything else triggers exponential retry.
//
// Workers use FOR UPDATE SKIP LOCKED so multiple pods can drain in parallel.
package outbound

import (
    "bytes"
    "context"
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "math"
    "net/http"
    "strconv"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/metrics"
)

const (
    maxAttempts = 8
    baseBackoff = 30 * time.Second
    maxBackoff  = 6 * time.Hour
    httpTimeout = 10 * time.Second
)

type Service struct {
    db            *pgxpool.Pool
    signingSecret string
    httpClient    *http.Client
}

func NewService(db *pgxpool.Pool, signingSecret string) *Service {
    return &Service{
        db:            db,
        signingSecret: signingSecret,
        httpClient:    &http.Client{Timeout: httpTimeout},
    }
}

// Enqueue fans out an event to all active endpoints of a merchant.
// Returns the number of deliveries enqueued.
func (s *Service) Enqueue(ctx context.Context, merchantID int64, eventType string, payload interface{}) (int, error) {
    body, err := json.Marshal(map[string]interface{}{
        "event_type": eventType,
        "merchant_id": merchantID,
        "data":       payload,
        "created_at": time.Now().UTC().Format(time.RFC3339Nano),
    })
    if err != nil {
        return 0, fmt.Errorf("marshal payload: %w", err)
    }
    rows, err := s.db.Query(ctx, `
        SELECT id FROM merchant_webhook_endpoints
        WHERE merchant_id = $1 AND is_active = true
    `, merchantID)
    if err != nil {
        return 0, fmt.Errorf("list endpoints: %w", err)
    }
    defer rows.Close()
    var endpointIDs []int64
    for rows.Next() {
        var id int64
        if err := rows.Scan(&id); err == nil {
            endpointIDs = append(endpointIDs, id)
        }
    }
    if len(endpointIDs) == 0 {
        return 0, nil
    }
    for _, id := range endpointIDs {
        if _, err := s.db.Exec(ctx, `
            INSERT INTO outbound_webhook_deliveries (endpoint_id, merchant_id, event_type, payload)
            VALUES ($1, $2, $3, $4)
        `, id, merchantID, eventType, body); err != nil {
            log.Error().Err(err).Int64("endpoint_id", id).Msg("enqueue outbound delivery")
        }
    }
    return len(endpointIDs), nil
}

// DeliverOnce drains one due delivery. Returns true if a row was processed.
func (s *Service) DeliverOnce(ctx context.Context) (bool, error) {
    tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
    if err != nil {
        return false, err
    }
    defer tx.Rollback(ctx) //nolint:errcheck

    var (
        deliveryID, endpointID, merchantID int64
        eventType, url                     string
        payload                            []byte
        attempts                           int
    )
    err = tx.QueryRow(ctx, `
        SELECT d.id, d.endpoint_id, d.merchant_id, d.event_type, d.payload, d.attempts, e.url
        FROM outbound_webhook_deliveries d
        JOIN merchant_webhook_endpoints e ON e.id = d.endpoint_id
        WHERE d.status = 'pending' AND d.next_attempt_at <= NOW() AND e.is_active = true
        ORDER BY d.next_attempt_at ASC
        FOR UPDATE OF d SKIP LOCKED
        LIMIT 1
    `).Scan(&deliveryID, &endpointID, &merchantID, &eventType, &payload, &attempts, &url)
    if errors.Is(err, pgx.ErrNoRows) {
        return false, nil
    }
    if err != nil {
        return false, err
    }

    status, sendErr := s.send(ctx, url, payload, deliveryID)
    attempts++

    if sendErr == nil && status >= 200 && status < 300 {
        if _, err := tx.Exec(ctx, `
            UPDATE outbound_webhook_deliveries
            SET status = 'delivered', attempts = $1, last_status = $2,
                last_error = NULL, delivered_at = NOW()
            WHERE id = $3
        `, attempts, status, deliveryID); err != nil {
            return true, err
        }
        metrics.OutboundDeliveries.WithLabelValues("success").Inc()
    } else {
        outcome := "http_error"
        errMsg := ""
        if sendErr != nil {
            outcome = "network_error"
            errMsg = sendErr.Error()
        } else {
            errMsg = fmt.Sprintf("non-2xx %d", status)
        }
        metrics.OutboundDeliveries.WithLabelValues(outcome).Inc()

        next := nextBackoff(attempts)
        finalStatus := "pending"
        if attempts >= maxAttempts {
            finalStatus = "dead"
        }
        if _, err := tx.Exec(ctx, `
            UPDATE outbound_webhook_deliveries
            SET attempts = $1, last_status = NULLIF($2, 0), last_error = $3,
                next_attempt_at = NOW() + $4::interval,
                status = $5
            WHERE id = $6
        `, attempts, status, errMsg, fmt.Sprintf("%d seconds", int(next.Seconds())), finalStatus, deliveryID); err != nil {
            return true, err
        }
    }

    return true, tx.Commit(ctx)
}

func (s *Service) send(ctx context.Context, url string, body []byte, deliveryID int64) (int, error) {
    ts := time.Now().Unix()
    mac := hmac.New(sha256.New, []byte(s.signingSecret))
    mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + string(body)))
    sig := hex.EncodeToString(mac.Sum(nil))

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
    if err != nil {
        return 0, err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-Gateway-Signature", fmt.Sprintf("t=%d,v1=%s", ts, sig))
    req.Header.Set("Idempotency-Key", fmt.Sprintf("dlv_%d", deliveryID))
    req.Header.Set("User-Agent", "payment-gateway/1.0")

    resp, err := s.httpClient.Do(req)
    if err != nil {
        return 0, err
    }
    defer resp.Body.Close()
    _, _ = io.Copy(io.Discard, resp.Body)
    return resp.StatusCode, nil
}

func nextBackoff(attempts int) time.Duration {
    d := time.Duration(float64(baseBackoff) * math.Pow(2, float64(attempts-1)))
    if d > maxBackoff {
        d = maxBackoff
    }
    return d
}

// Start runs DeliverOnce on a ticker, draining what's due each tick.
func (s *Service) Start(ctx context.Context, interval time.Duration) {
    t := time.NewTicker(interval)
    defer t.Stop()
    log.Info().Dur("interval", interval).Msg("outbound delivery worker started")
    for {
        select {
        case <-t.C:
            for {
                had, err := s.DeliverOnce(ctx)
                if err != nil {
                    log.Error().Err(err).Msg("outbound delivery error")
                    break
                }
                if !had {
                    break
                }
            }
        case <-ctx.Done():
            log.Info().Msg("outbound delivery worker stopped")
            return
        }
    }
}
