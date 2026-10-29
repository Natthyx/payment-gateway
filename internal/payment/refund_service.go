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
	"github.com/Natthyx/payment-gateway/internal/outbound"
	"github.com/Natthyx/payment-gateway/internal/provider"
)

// RefundService coordinates refund creation across the provider, the payment
// intent table, the ledger (reversal entries) and the audit log.
type RefundService struct {
	db       *pgxpool.Pool
	registry *provider.Registry
	ledger   *ledger.Service
	audit    *audit.Service
	outbound *outbound.Service // optional — nil disables refund.succeeded events
}

func NewRefundService(db *pgxpool.Pool, reg *provider.Registry, ledgerSvc *ledger.Service, a *audit.Service, out *outbound.Service) *RefundService {
	return &RefundService{db: db, registry: reg, ledger: ledgerSvc, audit: a, outbound: out}
}

// Create initiates a refund. Full or partial.
// Steps:
//  1. In one transaction holding the payment row lock: authorise, resolve
//     idempotent replays, check the amount against what is still refundable
//     (succeeded + pending refunds count as used), insert the refund as
//     'pending'. The lock serialises concurrent refunds of one payment, so
//     their sum can never exceed the payment amount.
//  2. Call the provider Refund API (no transaction held).
//  3. On success: mark refund succeeded, write reversal ledger entries, mark
//     the payment 'refunded' once fully refunded, enqueue refund.succeeded —
//     all in one transaction. On failure: mark it failed, which releases the
//     reserved amount.
//  4. Audit the action.
func (s *RefundService) Create(
	ctx context.Context,
	merchantID, paymentIntentID int64,
	req models.CreateRefundRequest,
) (*models.Refund, error) {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" {
		return nil, apierr.InvalidRequest("idempotency_key is required")
	}
	if req.Amount < 0 {
		return nil, apierr.InvalidRequest("refund amount must be > 0")
	}

	pi, rf, err := s.reserve(ctx, merchantID, paymentIntentID, req)
	if err != nil || rf.Status != models.RefundPending {
		return rf, err // error, or idempotent replay of a finished refund
	}

	p, err := s.registry.Get(string(pi.Provider))
	if err != nil {
		return nil, apierr.Provider("unknown provider")
	}
	pres, perr := p.(provider.Refunder).Refund(ctx, provider.RefundRequest{
		ProviderTxID: pi.ProviderTxID,
		Amount:       rf.Amount,
		Reason:       rf.Reason,
		// Unique per refund across the whole gateway: merchant-chosen keys
		// are only unique per merchant and must not collide at the provider.
		IdempotencyKey: fmt.Sprintf("gw_re_%d", rf.ID),
	})
	if perr != nil {
		_, _ = s.db.Exec(ctx, `
            UPDATE refunds SET status = 'failed', failure_reason = $1 WHERE id = $2 AND status = 'pending'
        `, perr.Error(), rf.ID)
		return nil, apierr.Provider(perr.Error())
	}

	if err := s.finalize(ctx, pi, rf, pres.ProviderRefundID); err != nil {
		return nil, err
	}

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
	return rf, nil
}

// reserve runs step 1 of Create. It returns the refund row: 'pending' means
// the caller owns it and must call the provider; any other status is a replay.
func (s *RefundService) reserve(
	ctx context.Context, merchantID, paymentIntentID int64, req models.CreateRefundRequest,
) (*models.PaymentIntent, *models.Refund, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin refund tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var pi models.PaymentIntent
	var status, prov string
	err = tx.QueryRow(ctx, `
        SELECT id, merchant_id, amount, currency, provider, status, COALESCE(provider_tx_id, '')
        FROM payment_intents
        WHERE id = $1 AND merchant_id = $2
        FOR UPDATE
    `, paymentIntentID, merchantID).Scan(
		&pi.ID, &pi.MerchantID, &pi.Amount, &pi.Currency, &prov, &status, &pi.ProviderTxID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, apierr.NotFound("payment intent not found")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("loading payment intent: %w", err)
	}
	pi.Status = models.PaymentStatus(status)
	pi.Provider = models.Provider(prov)

	// Idempotent replay. Checked before the status test so a retry of the
	// refund that fully refunded the payment still gets its result.
	if existing, err := scanRefund(tx.QueryRow(ctx, refundSelect+`
        WHERE merchant_id = $1 AND idempotency_key = $2
    `, merchantID, req.IdempotencyKey)); err == nil {
		switch {
		case existing.PaymentIntentID != pi.ID || (req.Amount != 0 && req.Amount != existing.Amount):
			return nil, nil, apierr.IdempotencyConflict("idempotency_key already used for a different refund")
		case existing.Status == models.RefundPending:
			return nil, nil, apierr.IdempotencyInUse("a refund with this idempotency_key is in progress; retry later")
		default:
			return &pi, existing, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("loading refund: %w", err)
	}

	if pi.Status != models.StatusSucceeded {
		return nil, nil, apierr.Conflict("only succeeded payments can be refunded")
	}
	p, err := s.registry.Get(string(pi.Provider))
	if err != nil {
		return nil, nil, apierr.Provider("unknown provider")
	}
	if _, ok := p.(provider.Refunder); !ok {
		return nil, nil, apierr.Conflict(fmt.Sprintf("provider %s does not support refunds", pi.Provider))
	}

	var used int64
	if err := tx.QueryRow(ctx, `
        SELECT COALESCE(SUM(amount), 0) FROM refunds
        WHERE payment_intent_id = $1 AND status IN ('pending', 'succeeded')
    `, pi.ID).Scan(&used); err != nil {
		return nil, nil, fmt.Errorf("summing refunds: %w", err)
	}
	remaining := pi.Amount - used
	amount := req.Amount
	if amount == 0 {
		amount = remaining // default: refund whatever is left
	}
	if remaining <= 0 {
		return nil, nil, apierr.Conflict("payment is already fully refunded (or refunds are in progress)")
	}
	if amount > remaining {
		e := apierr.Conflict(fmt.Sprintf("refund amount %d exceeds refundable amount %d", amount, remaining))
		e.Details = map[string]interface{}{"refundable_amount": remaining}
		return nil, nil, e
	}

	rf, err := scanRefund(tx.QueryRow(ctx, `
        INSERT INTO refunds (payment_intent_id, merchant_id, idempotency_key, amount, currency, reason, status)
        VALUES ($1, $2, $3, $4, $5, $6, 'pending')
        RETURNING `+refundColumns, pi.ID, merchantID, req.IdempotencyKey, amount, pi.Currency, req.Reason))
	if err != nil {
		if isUniqueViolation(err) {
			// Same key raced in for a different payment of this merchant.
			return nil, nil, apierr.IdempotencyConflict("idempotency_key already used for a different refund")
		}
		return nil, nil, fmt.Errorf("inserting refund: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit refund reservation: %w", err)
	}
	return &pi, rf, nil
}

// finalize runs step 3 of Create.
func (s *RefundService) finalize(ctx context.Context, pi *models.PaymentIntent, rf *models.Refund, providerRefundID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin refund tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the payment again so the "fully refunded" check sees every refund.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM payment_intents WHERE id = $1 FOR UPDATE`, pi.ID); err != nil {
		return fmt.Errorf("lock payment: %w", err)
	}
	tag, err := tx.Exec(ctx, `
        UPDATE refunds SET status = 'succeeded', provider_refund_id = $1, failure_reason = NULL
        WHERE id = $2 AND status = 'pending'
    `, providerRefundID, rf.ID)
	if err != nil {
		return fmt.Errorf("update refund: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("refund %d is no longer pending", rf.ID)
	}
	if err := s.ledger.RecordRefundTx(ctx, tx, pi, rf.ID, rf.Amount); err != nil {
		return fmt.Errorf("record refund ledger: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE payment_intents SET status = 'refunded'
        WHERE id = $1 AND amount <= (
            SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_intent_id = $1 AND status = 'succeeded')
    `, pi.ID); err != nil {
		return fmt.Errorf("update intent: %w", err)
	}
	rf.Status = models.RefundSucceeded
	rf.ProviderRefundID = providerRefundID
	if s.outbound != nil {
		if _, err := s.outbound.EnqueueTx(ctx, tx, pi.MerchantID, "refund.succeeded", rf); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit refund tx: %w", err)
	}
	return nil
}

const refundColumns = `id, payment_intent_id, merchant_id, idempotency_key, amount, currency,
    COALESCE(reason, ''), status, COALESCE(provider_refund_id, ''), COALESCE(failure_reason, ''),
    created_at, updated_at`

const refundSelect = `SELECT ` + refundColumns + ` FROM refunds `

func scanRefund(row pgx.Row) (*models.Refund, error) {
	var rf models.Refund
	var st string
	if err := row.Scan(&rf.ID, &rf.PaymentIntentID, &rf.MerchantID, &rf.IdempotencyKey, &rf.Amount,
		&rf.Currency, &rf.Reason, &st, &rf.ProviderRefundID, &rf.FailureReason,
		&rf.CreatedAt, &rf.UpdatedAt); err != nil {
		return nil, err
	}
	rf.Status = models.RefundStatus(st)
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
               COALESCE(reason, ''), status, COALESCE(provider_refund_id, ''),
               COALESCE(failure_reason, ''), created_at, updated_at
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
