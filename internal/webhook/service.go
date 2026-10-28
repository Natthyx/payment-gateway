package webhook

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
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
//   Phase 1 — Receive: store raw webhook immediately, return 200 to provider.
//             Fast. Never fails due to business logic. Provider stops retrying.
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

    log.Info().
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
        SELECT id, provider, provider_event_id, event_type, payload, raw_payload, signature, attempts
        FROM webhooks
        WHERE status = 'pending' AND attempts < 5
        ORDER BY created_at ASC
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    `).Scan(
        &wh.ID, &providerName, &wh.ProviderEventID, &wh.EventType,
        &wh.Payload, &rawPayload, &wh.Signature, &wh.Attempts,
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

    transitioned, procErr := s.processOne(ctx, tx, wh)
    if procErr != nil {
        // Mark failed only after the cap is reached; otherwise leave as
        // pending so the next tick picks it up.
        if wh.Attempts+1 >= 5 {
            if _, err := tx.Exec(ctx, `
                UPDATE webhooks SET status = 'failed', last_error = $1 WHERE id = $2
            `, procErr.Error(), wh.ID); err != nil {
                return true, fmt.Errorf("mark webhook failed: %w", err)
            }
        } else {
            if _, err := tx.Exec(ctx, `
                UPDATE webhooks SET last_error = $1 WHERE id = $2
            `, procErr.Error(), wh.ID); err != nil {
                return true, fmt.Errorf("record webhook error: %w", err)
            }
        }
        if err := tx.Commit(ctx); err != nil {
            return true, fmt.Errorf("commit failure record: %w", err)
        }
        outcome := "failed"
        if wh.Attempts+1 < 5 {
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

    // Fan out merchant-facing webhooks ONLY after we've durably committed.
    if transitioned != nil && s.outbound != nil {
        if _, err := s.outbound.Enqueue(ctx, transitioned.MerchantID, "payment.succeeded", transitioned); err != nil {
            log.Warn().Err(err).Int64("payment_intent_id", transitioned.ID).Msg("enqueue outbound payment.succeeded")
        }
    }
    metrics.WebhookProcessed.WithLabelValues(string(wh.Provider), "processed").Inc()
    log.Info().Int64("webhook_id", wh.ID).Msg("webhook processed")
    return true, nil
}

// processOne verifies the signature, dispatches the event, and (on success)
// writes ledger entries — all inside the caller-supplied transaction so the
// row stays locked and the state change is atomic. Returns the payment intent
// that was just transitioned to `succeeded`, if any — the caller fans this
// out to merchant webhooks AFTER the transaction commits.
func (s *Service) processOne(ctx context.Context, tx pgx.Tx, wh models.Webhook) (*models.PaymentIntent, error) {
    p, err := s.registry.Get(string(wh.Provider))
    if err != nil {
        return nil, fmt.Errorf("unknown provider %q: %w", wh.Provider, err)
    }
    event, err := p.VerifyWebhook(wh.Payload, wh.Signature)
    if err != nil {
        return nil, fmt.Errorf("signature verification failed: %w", err)
    }
    return s.handleEvent(ctx, tx, event)
}

// handleEvent applies a verified event to the corresponding payment intent
// inside the caller's transaction. For successful payments it also writes
// the double-entry ledger rows; both updates commit together or not at all.
// Returns the payment intent if a success transition happened (caller fans
// this out post-commit).
func (s *Service) handleEvent(ctx context.Context, tx pgx.Tx, event *provider.WebhookEvent) (*models.PaymentIntent, error) {
    switch event.EventType {
    case "payment_intent.succeeded", "payment.succeeded", "SUCCESS":
        return s.markSucceeded(ctx, tx, event)

    case "payment_intent.payment_failed", "payment.failed", "FAILED":
        _, err := tx.Exec(ctx, `
            UPDATE payment_intents
            SET status = 'failed',
                failure_reason = 'payment failed via webhook',
                updated_at = NOW()
            WHERE (provider_tx_id = $1 OR idempotency_key = $1)
              AND status NOT IN ('succeeded', 'refunded')
        `, event.ProviderTxID)
        return nil, err

    default:
        // Unknown event type — record it as processed but do not mutate intents.
        log.Info().
            Str("event_type", event.EventType).
            Str("provider_tx_id", event.ProviderTxID).
            Msg("unhandled webhook event type")
        return nil, nil
    }
}

// markSucceeded transitions a processing intent to succeeded and writes the
// ledger entries, both atomically inside the caller's transaction. The UPDATE
// is gated on status != 'succeeded' so a re-delivered webhook that slips
// through dedupe is still a no-op (RowsAffected = 0 → we skip the ledger).
func (s *Service) markSucceeded(ctx context.Context, tx pgx.Tx, event *provider.WebhookEvent) (*models.PaymentIntent, error) {
    var (
        pi     models.PaymentIntent
        status string
        prov   string
        meta   []byte
    )
    err := tx.QueryRow(ctx, `
        UPDATE payment_intents
        SET status = 'succeeded',
            provider_tx_id = COALESCE(NULLIF(provider_tx_id, ''), $1),
            updated_at = NOW()
        WHERE (provider_tx_id = $1 OR idempotency_key = $1)
          AND status NOT IN ('succeeded', 'refunded')
        RETURNING id, merchant_id, idempotency_key, amount, currency,
                  provider, status, provider_tx_id, metadata, created_at, updated_at
    `, event.ProviderTxID).Scan(
        &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount, &pi.Currency,
        &prov, &status, &pi.ProviderTxID, &meta, &pi.CreatedAt, &pi.UpdatedAt,
    )
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, nil
        }
        return nil, fmt.Errorf("mark intent succeeded: %w", err)
    }
    pi.Status = models.PaymentStatus(status)
    pi.Provider = models.Provider(prov)
    pi.Metadata = json.RawMessage(meta)

    if err := s.ledger.RecordPaymentTx(ctx, tx, &pi, s.feePercent); err != nil {
        if errors.Is(err, ledger.ErrLedgerAlreadyRecorded) {
            return &pi, nil
        }
        return nil, fmt.Errorf("record ledger: %w", err)
    }
    metrics.PaymentsByStatus.WithLabelValues(string(pi.Provider), "processing", "succeeded").Inc()
    metrics.LedgerEntries.Inc()
    return &pi, nil
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
