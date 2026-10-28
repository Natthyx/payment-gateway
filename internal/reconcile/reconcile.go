// Package reconcile compares our payment intent state with the provider's
// authoritative state and emits a metric/log on every mismatch.
//
// Why: webhooks can be lost (network errors, signing-key rotation, provider
// outages). Without reconciliation a "succeeded" payment on Stripe can sit
// forever as "processing" in our DB. The job below catches those.
package reconcile

import (
    "context"
    "errors"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/ledger"
    "github.com/Natthyx/payment-gateway/internal/models"
    "github.com/Natthyx/payment-gateway/internal/provider"
)

type Service struct {
    db         *pgxpool.Pool
    registry   *provider.Registry
    ledger     *ledger.Service
    feePercent float64
}

func NewService(db *pgxpool.Pool, reg *provider.Registry, l *ledger.Service, feePercent float64) *Service {
    return &Service{db: db, registry: reg, ledger: l, feePercent: feePercent}
}

// RunOnce reconciles payment intents that have been in `processing` for
// longer than `staleAfter`. It calls provider.GetTransaction and, if the
// provider says the payment succeeded/failed, updates our row + ledger
// inside a transaction.
func (s *Service) RunOnce(ctx context.Context, staleAfter time.Duration, batch int) error {
    if batch <= 0 {
        batch = 100
    }
    cutoff := time.Now().Add(-staleAfter)
    rows, err := s.db.Query(ctx, `
        SELECT id, merchant_id, idempotency_key, amount, currency, provider,
               status, provider_tx_id, created_at, updated_at
        FROM payment_intents
        WHERE status = 'processing' AND updated_at < $1
        ORDER BY updated_at ASC
        LIMIT $2
    `, cutoff, batch)
    if err != nil {
        return fmt.Errorf("query stale intents: %w", err)
    }
    defer rows.Close()

    var stale []models.PaymentIntent
    for rows.Next() {
        var pi models.PaymentIntent
        var st, prov string
        if err := rows.Scan(&pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount,
            &pi.Currency, &prov, &st, &pi.ProviderTxID, &pi.CreatedAt, &pi.UpdatedAt); err != nil {
            log.Error().Err(err).Msg("reconcile scan")
            continue
        }
        pi.Status = models.PaymentStatus(st)
        pi.Provider = models.Provider(prov)
        stale = append(stale, pi)
    }
    if err := rows.Err(); err != nil {
        return err
    }

    for _, pi := range stale {
        if err := s.reconcileOne(ctx, pi); err != nil {
            log.Error().Err(err).Int64("payment_intent_id", pi.ID).Msg("reconcile failed")
        }
    }
    return nil
}

func (s *Service) reconcileOne(ctx context.Context, pi models.PaymentIntent) error {
    if pi.ProviderTxID == "" {
        return errors.New("missing provider_tx_id")
    }
    p, err := s.registry.Get(string(pi.Provider))
    if err != nil {
        return fmt.Errorf("registry: %w", err)
    }
    tx, err := p.GetTransaction(ctx, pi.ProviderTxID)
    if err != nil {
        return fmt.Errorf("provider get tx: %w", err)
    }

    // Provider authoritative state → drives our state.
    newStatus := mapProviderStatus(tx.Status)
    if newStatus == "" || newStatus == pi.Status {
        return nil
    }

    dbTx, err := s.db.Begin(ctx)
    if err != nil {
        return err
    }
    defer dbTx.Rollback(ctx) //nolint:errcheck

    var st string
    err = dbTx.QueryRow(ctx, `
        UPDATE payment_intents
        SET status = $1, updated_at = NOW()
        WHERE id = $2 AND status = 'processing'
        RETURNING status
    `, string(newStatus), pi.ID).Scan(&st)
    if errors.Is(err, pgx.ErrNoRows) {
        return nil // someone else moved it
    }
    if err != nil {
        return err
    }

    if newStatus == models.StatusSucceeded {
        if err := s.ledger.RecordPaymentTx(ctx, dbTx, &pi, s.feePercent); err != nil && !errors.Is(err, ledger.ErrLedgerAlreadyRecorded) {
            return fmt.Errorf("record ledger: %w", err)
        }
    }
    if err := dbTx.Commit(ctx); err != nil {
        return err
    }
    log.Warn().
        Int64("payment_intent_id", pi.ID).
        Str("from", string(pi.Status)).
        Str("to", string(newStatus)).
        Msg("reconciler corrected stale payment intent")
    return nil
}

func mapProviderStatus(s string) models.PaymentStatus {
    switch s {
    case "succeeded", "SUCCESS", "success":
        return models.StatusSucceeded
    case "failed", "FAILED", "canceled":
        return models.StatusFailed
    default:
        return ""
    }
}

// Start runs RunOnce on a ticker. Use a wider stale window in prod (e.g. 15m).
func (s *Service) Start(ctx context.Context, interval, staleAfter time.Duration) {
    t := time.NewTicker(interval)
    defer t.Stop()
    log.Info().Dur("interval", interval).Dur("stale_after", staleAfter).Msg("reconciler started")
    for {
        select {
        case <-t.C:
            if err := s.RunOnce(ctx, staleAfter, 100); err != nil {
                log.Error().Err(err).Msg("reconciler error")
            }
        case <-ctx.Done():
            log.Info().Msg("reconciler stopped")
            return
        }
    }
}
