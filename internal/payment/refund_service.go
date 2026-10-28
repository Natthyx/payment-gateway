package payment

import (
    "context"
    "errors"
    "fmt"
    "strings"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/apierr"
    "github.com/Natthyx/payment-gateway/internal/audit"
    "github.com/Natthyx/payment-gateway/internal/ledger"
    "github.com/Natthyx/payment-gateway/internal/metrics"
    "github.com/Natthyx/payment-gateway/internal/models"
    "github.com/Natthyx/payment-gateway/internal/provider"
)

// RefundService coordinates refund creation across the provider, the payment
// intent table, the ledger (reversal entries) and the audit log.
type RefundService struct {
    db       *pgxpool.Pool
    registry *provider.Registry
    ledger   *ledger.Service
    audit    *audit.Service
}

func NewRefundService(db *pgxpool.Pool, reg *provider.Registry, ledgerSvc *ledger.Service, a *audit.Service) *RefundService {
    return &RefundService{db: db, registry: reg, ledger: ledgerSvc, audit: a}
}

// Create initiates a refund. Full or partial.
// Steps:
//   1. Load and authorise the payment intent
//   2. Insert refund row (idempotent on (merchant_id, idempotency_key))
//   3. Call provider Refund API
//   4. On success: mark refund succeeded + write reversal ledger entries
//      in the same DB transaction.
//   5. Audit the action.
func (s *RefundService) Create(
    ctx context.Context,
    merchantID, paymentIntentID int64,
    req models.CreateRefundRequest,
) (*models.Refund, error) {
    req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
    if req.IdempotencyKey == "" {
        return nil, apierr.InvalidRequest("idempotency_key is required")
    }

    var pi models.PaymentIntent
    var status, prov string
    err := s.db.QueryRow(ctx, `
        SELECT id, merchant_id, amount, currency, provider, status, provider_tx_id
        FROM payment_intents
        WHERE id = $1 AND merchant_id = $2
    `, paymentIntentID, merchantID).Scan(
        &pi.ID, &pi.MerchantID, &pi.Amount, &pi.Currency, &prov, &status, &pi.ProviderTxID,
    )
    if errors.Is(err, pgx.ErrNoRows) {
        return nil, apierr.NotFound("payment intent not found")
    }
    if err != nil {
        return nil, fmt.Errorf("loading payment intent: %w", err)
    }
    pi.Status = models.PaymentStatus(status)
    pi.Provider = models.Provider(prov)

    if pi.Status != models.StatusSucceeded {
        return nil, apierr.Conflict("only succeeded payments can be refunded")
    }
    if req.Amount == 0 {
        req.Amount = pi.Amount
    }
    if req.Amount <= 0 || req.Amount > pi.Amount {
        return nil, apierr.InvalidRequest("refund amount must be in (0, original_amount]")
    }

    p, err := s.registry.Get(string(pi.Provider))
    if err != nil {
        return nil, apierr.Provider("unknown provider")
    }
    refunder, ok := p.(provider.Refunder)
    if !ok {
        return nil, apierr.Conflict(fmt.Sprintf("provider %s does not support refunds", pi.Provider))
    }

    var rf models.Refund
    var rstatus string
    err = s.db.QueryRow(ctx, `
        INSERT INTO refunds (payment_intent_id, merchant_id, idempotency_key, amount, currency, reason, status)
        VALUES ($1, $2, $3, $4, $5, $6, 'pending')
        ON CONFLICT (merchant_id, idempotency_key) DO UPDATE SET reason = refunds.reason
        RETURNING id, payment_intent_id, merchant_id, idempotency_key, amount, currency,
                  reason, status, provider_refund_id, failure_reason, created_at, updated_at
    `, pi.ID, merchantID, req.IdempotencyKey, req.Amount, pi.Currency, req.Reason).Scan(
        &rf.ID, &rf.PaymentIntentID, &rf.MerchantID, &rf.IdempotencyKey, &rf.Amount,
        &rf.Currency, &rf.Reason, &rstatus, &rf.ProviderRefundID, &rf.FailureReason,
        &rf.CreatedAt, &rf.UpdatedAt,
    )
    if err != nil {
        return nil, fmt.Errorf("inserting refund: %w", err)
    }
    rf.Status = models.RefundStatus(rstatus)

    // Idempotent replay: if we already finalised this refund, return current state.
    if rf.Status == models.RefundSucceeded {
        return &rf, nil
    }

    pres, perr := refunder.Refund(ctx, provider.RefundRequest{
        ProviderTxID:   pi.ProviderTxID,
        Amount:         req.Amount,
        Reason:         req.Reason,
        IdempotencyKey: req.IdempotencyKey,
    })
    if perr != nil {
        _, _ = s.db.Exec(ctx, `
            UPDATE refunds SET status = 'failed', failure_reason = $1 WHERE id = $2
        `, perr.Error(), rf.ID)
        return nil, apierr.Provider(perr.Error())
    }

    // Finalise atomically: refund row + ledger reversal in one transaction.
    tx, err := s.db.Begin(ctx)
    if err != nil {
        return nil, fmt.Errorf("begin refund tx: %w", err)
    }
    defer tx.Rollback(ctx) //nolint:errcheck

    if _, err := tx.Exec(ctx, `
        UPDATE refunds SET status = 'succeeded', provider_refund_id = $1, failure_reason = NULL
        WHERE id = $2
    `, pres.ProviderRefundID, rf.ID); err != nil {
        return nil, fmt.Errorf("update refund: %w", err)
    }
    if req.Amount == pi.Amount {
        if _, err := tx.Exec(ctx, `
            UPDATE payment_intents SET status = 'refunded' WHERE id = $1
        `, pi.ID); err != nil {
            return nil, fmt.Errorf("update intent: %w", err)
        }
    }
    if err := s.ledger.RecordRefundTx(ctx, tx, &pi, req.Amount); err != nil {
        return nil, fmt.Errorf("record refund ledger: %w", err)
    }
    if err := tx.Commit(ctx); err != nil {
        return nil, fmt.Errorf("commit refund tx: %w", err)
    }

    rf.Status = models.RefundSucceeded
    rf.ProviderRefundID = pres.ProviderRefundID

    metrics.PaymentsByStatus.WithLabelValues(string(pi.Provider), string(pi.Status), "refunded").Inc()
    s.audit.Record(ctx, audit.Entry{
        MerchantID: &merchantID,
        Actor:      audit.MerchantActor(merchantID),
        Action:     "refund.create",
        Target:     fmt.Sprintf("refund:%d", rf.ID),
        Details: map[string]interface{}{
            "payment_intent_id":  pi.ID,
            "amount":             rf.Amount,
            "currency":           rf.Currency,
            "provider":           pi.Provider,
            "provider_refund_id": rf.ProviderRefundID,
        },
    })
    log.Info().Int64("refund_id", rf.ID).Int64("amount", rf.Amount).Str("currency", rf.Currency).Msg("refund succeeded")
    return &rf, nil
}

// List returns paginated refunds for a merchant.
func (s *RefundService) List(ctx context.Context, merchantID int64, page, limit int) ([]models.Refund, int, error) {
    if page < 1 {
        page = 1
    }
    if limit < 1 || limit > 100 {
        limit = 20
    }
    rows, err := s.db.Query(ctx, `
        SELECT id, payment_intent_id, merchant_id, idempotency_key, amount, currency,
               reason, status, provider_refund_id, failure_reason, created_at, updated_at
        FROM refunds WHERE merchant_id = $1
        ORDER BY created_at DESC LIMIT $2 OFFSET $3
    `, merchantID, limit, (page-1)*limit)
    if err != nil {
        return nil, 0, fmt.Errorf("listing refunds: %w", err)
    }
    defer rows.Close()
    var out []models.Refund
    for rows.Next() {
        var r models.Refund
        var st string
        if err := rows.Scan(&r.ID, &r.PaymentIntentID, &r.MerchantID, &r.IdempotencyKey,
            &r.Amount, &r.Currency, &r.Reason, &st, &r.ProviderRefundID, &r.FailureReason,
            &r.CreatedAt, &r.UpdatedAt); err != nil {
            return nil, 0, err
        }
        r.Status = models.RefundStatus(st)
        out = append(out, r)
    }
    var total int
    _ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM refunds WHERE merchant_id = $1`, merchantID).Scan(&total)
    if out == nil {
        out = []models.Refund{}
    }
    return out, total, nil
}
