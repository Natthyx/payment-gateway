package webhook

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/ledger"
    "github.com/Natthyx/payment-gateway/internal/metrics"
    "github.com/Natthyx/payment-gateway/internal/models"
    "github.com/Natthyx/payment-gateway/internal/outbound"
    "github.com/Natthyx/payment-gateway/internal/provider"
)

// Service handles webhook ingestion and async processing.
//
// TWO-PHASE DESIGN (inbox pattern):
//   Phase 1 — Receive: verify the signature, store the raw webhook, return 200.
//             Unverifiable bodies are rejected (400) and never stored, so a
//             forged body cannot claim a real event id. A storage error
//             returns 503 so the provider retries.
//
//   Phase 2 — Process: background worker picks up pending webhooks one at a
//             time using SELECT ... FOR UPDATE SKIP LOCKED so multiple worker
//             pods can run in parallel without double-processing. Each row is
//             handled inside its own DB transaction, so the status flip and
//             the ledger insert succeed or fail together.
//
// Provider-event idempotency: on Ingest we use INSERT ... ON CONFLICT DO NOTHING
// on the (provider, provider_event_id) unique index, so re-deliveries are silently
// swallowed at the boundary.
//
// Failed processing is retried with exponential backoff (next_attempt_at)
// up to maxWebhookAttempts; errors that cannot succeed on retry (bad
// signature, amount mismatch) fail the row immediately.
type Service struct {
    db         *pgxpool.Pool
    registry   *provider.Registry
    ledger     *ledger.Service
    outbound   *outbound.Service // optional — nil disables merchant-facing webhooks
    feePercent float64
}

func NewService(db *pgxpool.Pool, registry *provider.Registry, ledgerSvc *ledger.Service, outboundSvc *outbound.Service, feePercent float64) *Service {
    return &Service{db: db, registry: registry, ledger: ledgerSvc, outbound: outboundSvc, feePercent: feePercent}
}

const (
    maxWebhookAttempts = 5
    webhookBaseBackoff = 30 * time.Second
)

// webhookBackoff is the delay before attempt n+1 after n failed attempts:
// 30s, 60s, 120s, 240s.
func webhookBackoff(failedAttempts int) time.Duration {
    return webhookBaseBackoff << (failedAttempts - 1)
}

// permanentError marks a processing failure that retrying cannot fix.
type permanentError struct{ error }

// ingestInput is what the HTTP handler hands us — everything needed to persist
// a webhook without making a network or signature decision yet.
type ingestInput struct {
    Provider        string
    ProviderEventID string
    EventType       string
    Payload         []byte
    Signature       string
}

// Ingest stores a raw webhook idempotently. Returns (nil, nil) if the same
// provider event was already stored on an earlier delivery.
func (s *Service) Ingest(ctx context.Context, in ingestInput) (*models.Webhook, error) {
    var (
        wh           models.Webhook
        providerName string
    )
    err := s.db.QueryRow(ctx, `
        INSERT INTO webhooks (provider, provider_event_id, event_type, payload, raw_payload, signature, status)
        VALUES ($1, $2, $3, $4, $6, $5, 'pending')
        ON CONFLICT (provider, provider_event_id) WHERE provider_event_id IS NOT NULL
        DO NOTHING
        RETURNING id, provider, provider_event_id, event_type, payload, signature,
                  status, attempts, created_at
    `, in.Provider, in.ProviderEventID, in.EventType, in.Payload, in.Signature, in.Payload).Scan(
        &wh.ID, &providerName, &wh.ProviderEventID, &wh.EventType, &wh.Payload,
        &wh.Signature, &wh.Status, &wh.Attempts, &wh.CreatedAt,
    )
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, nil // ON CONFLICT swallowed — duplicate delivery
        }
        return nil, fmt.Errorf("storing webhook: %w", err)
    }
    wh.Provider = models.Provider(providerName)

    log.Debug().
        Int64("webhook_id", wh.ID).
        Str("provider", providerName).
        Str("event_type", in.EventType).
        Str("provider_event_id", in.ProviderEventID).
        Msg("webhook ingested")

    return &wh, nil
}

// ProcessOnce claims and processes a single pending webhook using row-level
// locking so other worker instances skip it. Returns true if a row was found
// and handled, false if the queue was empty.
func (s *Service) ProcessOnce(ctx context.Context) (bool, error) {
    tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
    if err != nil {
        return false, fmt.Errorf("begin worker tx: %w", err)
    }
    defer tx.Rollback(ctx) //nolint:errcheck

    var (
        wh           models.Webhook
        providerName string
        rawPayload   []byte
    )
    err = tx.QueryRow(ctx, `
        SELECT id, provider, provider_event_id, event_type, payload, raw_payload, signature, attempts, created_at
        FROM webhooks
        WHERE status = 'pending' AND attempts < $1 AND next_attempt_at <= NOW()
        ORDER BY next_attempt_at ASC
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    `, maxWebhookAttempts).Scan(
        &wh.ID, &providerName, &wh.ProviderEventID, &wh.EventType,
        &wh.Payload, &rawPayload, &wh.Signature, &wh.Attempts, &wh.CreatedAt,
    )
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return false, nil // queue empty (or all rows held by peers)
        }
        return false, fmt.Errorf("claim pending webhook: %w", err)
    }
    wh.Provider = models.Provider(providerName)
    // Signatures cover the exact bytes received; the JSONB column does not
    // preserve them. Pre-migration rows have no raw copy.
    if rawPayload != nil {
        wh.Payload = rawPayload
    }

    if _, err := tx.Exec(ctx, `UPDATE webhooks SET attempts = attempts + 1 WHERE id = $1`, wh.ID); err != nil {
        return true, fmt.Errorf("increment attempts: %w", err)
    }

    // Process inside a savepoint so a failed attempt rolls back its partial
    // writes while the attempt counter and error are still recorded.
    sp, err := tx.Begin(ctx)
    if err != nil {
        return true, fmt.Errorf("savepoint: %w", err)
    }
    procErr := s.processOne(ctx, sp, wh)
    if procErr == nil {
        procErr = sp.Commit(ctx)
    } else {
        _ = sp.Rollback(ctx)
    }
    if procErr != nil {
        var perm permanentError
        final := wh.Attempts+1 >= maxWebhookAttempts || errors.As(procErr, &perm)
        if final {
            if _, err := tx.Exec(ctx, `
                UPDATE webhooks SET status = 'failed', last_error = $1 WHERE id = $2
            `, procErr.Error(), wh.ID); err != nil {
                return true, fmt.Errorf("mark webhook failed: %w", err)
            }
        } else {
            if _, err := tx.Exec(ctx, `
                UPDATE webhooks SET last_error = $1, next_attempt_at = NOW() + $2::interval WHERE id = $3
            `, procErr.Error(), fmt.Sprintf("%d seconds", int(webhookBackoff(wh.Attempts+1).Seconds())), wh.ID); err != nil {
                return true, fmt.Errorf("record webhook error: %w", err)
            }
        }
        if err := tx.Commit(ctx); err != nil {
            return true, fmt.Errorf("commit failure record: %w", err)
        }
        outcome := "failed"
        if !final {
            outcome = "retrying"
        }
        metrics.WebhookProcessed.WithLabelValues(string(wh.Provider), outcome).Inc()
        log.Error().Err(procErr).Int64("webhook_id", wh.ID).Int("attempts", wh.Attempts+1).Msg("webhook processing failed")
        return true, nil
    }

    now := time.Now()
    if _, err := tx.Exec(ctx, `
        UPDATE webhooks SET status = 'processed', processed_at = $1, last_error = NULL WHERE id = $2
    `, now, wh.ID); err != nil {
        return true, fmt.Errorf("mark webhook processed: %w", err)
    }
    if err := tx.Commit(ctx); err != nil {
        return true, fmt.Errorf("commit webhook tx: %w", err)
    }
    metrics.WebhookProcessed.WithLabelValues(string(wh.Provider), "processed").Inc()
    log.Debug().Int64("webhook_id", wh.ID).Msg("webhook processed")
    return true, nil
}

// processOne re-verifies the signature (against the time the webhook was
// received) and applies the event inside the caller's transaction, so the
// intent update, the ledger rows and the merchant notification commit
// together or not at all.
func (s *Service) processOne(ctx context.Context, tx pgx.Tx, wh models.Webhook) error {
    p, err := s.registry.Get(string(wh.Provider))
    if err != nil {
        return permanentError{fmt.Errorf("unknown provider %q: %w", wh.Provider, err)}
    }
    var event *provider.WebhookEvent
    if rv, ok := p.(provider.ReceiptVerifier); ok {
        event, err = rv.VerifyWebhookAt(wh.Payload, wh.Signature, wh.CreatedAt)
    } else {
        event, err = p.VerifyWebhook(wh.Payload, wh.Signature)
    }
    if err != nil {
        return permanentError{fmt.Errorf("signature verification failed: %w", err)}
    }
    return s.handleEvent(ctx, tx, string(wh.Provider), event)
}

// handleEvent applies a verified event to the corresponding payment intent
// inside the caller's transaction.
func (s *Service) handleEvent(ctx context.Context, tx pgx.Tx, providerName string, event *provider.WebhookEvent) error {
    switch event.EventType {
    case "payment_intent.succeeded", "payment.succeeded", "SUCCESS":
        return s.markSucceeded(ctx, tx, providerName, event)

    case "payment_intent.payment_failed", "payment.failed", "FAILED":
        _, err := tx.Exec(ctx, `
            UPDATE payment_intents
            SET status = 'failed',
                failure_reason = 'payment failed via webhook',
                updated_at = NOW()
            WHERE provider = $1 AND provider_tx_id = $2
              AND status NOT IN ('succeeded', 'refunded')
        `, providerName, event.ProviderTxID)
        return err

    default:
        // Unknown event type — record it as processed but do not mutate intents.
        log.Info().
            Str("event_type", event.EventType).
            Str("provider_tx_id", event.ProviderTxID).
            Msg("unhandled webhook event type")
        return nil
    }
}

// markSucceeded transitions the intent identified by (provider,
// provider_tx_id) to succeeded, writes its ledger entries and enqueues the
// merchant's payment.succeeded event, all in the caller's transaction.
//   - already succeeded/refunded  → no-op (re-delivery under a new event id)
//   - no matching intent yet      → retryable error (the webhook can beat the
//                                   provider_tx_id update of CreateIntent)
//   - amount/currency mismatch    → permanent error; the intent is not touched
func (s *Service) markSucceeded(ctx context.Context, tx pgx.Tx, providerName string, event *provider.WebhookEvent) error {
    var (
        pi     models.PaymentIntent
        status string
        prov   string
        meta   []byte
    )
    err := tx.QueryRow(ctx, `
        SELECT id, merchant_id, idempotency_key, amount, currency,
               provider, status, provider_tx_id, metadata, created_at, updated_at
        FROM payment_intents
        WHERE provider = $1 AND provider_tx_id = $2
        FOR UPDATE
    `, providerName, event.ProviderTxID).Scan(
        &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount, &pi.Currency,
        &prov, &status, &pi.ProviderTxID, &meta, &pi.CreatedAt, &pi.UpdatedAt,
    )
    if errors.Is(err, pgx.ErrNoRows) {
        return fmt.Errorf("no payment intent for %s transaction %q", providerName, event.ProviderTxID)
    }
    if err != nil {
        return fmt.Errorf("load intent: %w", err)
    }
    if status == string(models.StatusSucceeded) || status == string(models.StatusRefunded) {
        return nil
    }
    if event.Amount != 0 && event.Amount != pi.Amount {
        return permanentError{fmt.Errorf("amount mismatch for intent %d: webhook %d, intent %d", pi.ID, event.Amount, pi.Amount)}
    }
    if event.Currency != "" && !strings.EqualFold(event.Currency, pi.Currency) {
        return permanentError{fmt.Errorf("currency mismatch for intent %d: webhook %s, intent %s", pi.ID, event.Currency, pi.Currency)}
    }

    if err := tx.QueryRow(ctx, `
        UPDATE payment_intents SET status = 'succeeded', updated_at = NOW()
        WHERE id = $1
        RETURNING status, updated_at
    `, pi.ID).Scan(&status, &pi.UpdatedAt); err != nil {
        return fmt.Errorf("mark intent succeeded: %w", err)
    }
    pi.Status = models.PaymentStatus(status)
    pi.Provider = models.Provider(prov)
    pi.Metadata = json.RawMessage(meta)

    if err := s.ledger.RecordPaymentTx(ctx, tx, &pi, s.feePercent); err != nil {
        if !errors.Is(err, ledger.ErrLedgerAlreadyRecorded) {
            return fmt.Errorf("record ledger: %w", err)
        }
    } else {
        metrics.LedgerEntries.Inc()
    }
    if s.outbound != nil {
        if _, err := s.outbound.EnqueueTx(ctx, tx, pi.MerchantID, "payment.succeeded", &pi); err != nil {
            return err
        }
    }
    metrics.PaymentsByStatus.WithLabelValues(string(pi.Provider), "processing", "succeeded").Inc()
    return nil
}

// StartWorker drains the pending-webhook queue on a ticker.
// Calls ProcessOnce repeatedly until empty, then sleeps until the next tick.
func (s *Service) StartWorker(ctx context.Context, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    log.Info().Dur("interval", interval).Msg("webhook worker started")

    drain := func() {
        for {
            had, err := s.ProcessOnce(ctx)
            if err != nil {
                log.Error().Err(err).Msg("webhook worker error")
                return
            }
            if !had {
                return
            }
        }
    }

    for {
        select {
        case <-ticker.C:
            drain()
        case <-ctx.Done():
            log.Info().Msg("webhook worker stopped")
            return
        }
    }
}
