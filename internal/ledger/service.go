package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Natthyx/payment-gateway/internal/models"
)

// ErrLedgerAlreadyRecorded means a webhook tried to record entries for a
// payment intent that already has them — replay protection caught it.
var ErrLedgerAlreadyRecorded = errors.New("ledger entries already recorded for this payment")

// Service handles all double-entry ledger operations.
//
// DOUBLE-ENTRY BOOKKEEPING PRIMER:
//
//	Every financial transaction creates exactly two entries:
//	one debit and one credit of equal and opposite amounts.
//	They must always net to zero. This is how every bank,
//	accounting system, and financial institution works.
//
// Example — merchant charges customer $10.50, gateway takes 2.9% fee:
//
//	DEBIT  provider:stripe     -1050  (money leaving provider)
//	CREDIT merchant:42         +1020  (merchant receives $10.20)
//	CREDIT gateway:fees        +30    (gateway keeps $0.30)
//	Net: -1050 + 1020 + 30 = 0 ✓
type Service struct {
	db *pgxpool.Pool
}

func NewService(db *pgxpool.Pool) *Service {
	return &Service{db: db}
}

// RecordPayment writes the ledger entries for a successful payment in a fresh
// transaction. Use RecordPaymentTx when you need to share a transaction with
// an outer caller (e.g. the webhook worker).
func (s *Service) RecordPayment(ctx context.Context, pi *models.PaymentIntent, feePercent float64) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning ledger transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := s.RecordPaymentTx(ctx, tx, pi, feePercent); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing ledger entries: %w", err)
	}
	return nil
}

// RecordPaymentTx writes the three-leg ledger entries for a successful payment
// using a caller-supplied transaction. Returns ErrLedgerAlreadyRecorded if a
// unique-constraint violation indicates the entries already exist (replay).
func (s *Service) RecordPaymentTx(ctx context.Context, tx pgx.Tx, pi *models.PaymentIntent, feePercent float64) error {
	// Smallest-unit fee, truncated toward zero (e.g. amount=1050, fee%=0.029 → fee=30).
	fee := int64(float64(pi.Amount) * feePercent)
	merchantAmount := pi.Amount - fee
	if merchantAmount <= 0 {
		return fmt.Errorf("fee exceeds payment amount")
	}

	entries := []models.LedgerEntry{
		{
			PaymentIntentID: pi.ID,
			Account:         fmt.Sprintf("provider:%s", pi.Provider),
			Amount:          -pi.Amount,
			Currency:        pi.Currency,
			EntryType:       models.EntryDebit,
			Description:     fmt.Sprintf("payment received via %s", pi.Provider),
		},
		{
			PaymentIntentID: pi.ID,
			Account:         fmt.Sprintf("merchant:%d", pi.MerchantID),
			Amount:          merchantAmount,
			Currency:        pi.Currency,
			EntryType:       models.EntryCredit,
			Description:     fmt.Sprintf("payment to merchant (net of %.1f%% fee)", feePercent*100),
		},
		{
			PaymentIntentID: pi.ID,
			Account:         "gateway:fees",
			Amount:          fee,
			Currency:        pi.Currency,
			EntryType:       models.EntryCredit,
			Description:     fmt.Sprintf("gateway fee %.1f%%", feePercent*100),
		},
	}

	if err := validateBalance(entries); err != nil {
		return err
	}

	for _, entry := range entries {
		_, err := tx.Exec(ctx, `
            INSERT INTO ledger_entries
                (payment_intent_id, account, amount, currency, entry_type, description)
            VALUES ($1, $2, $3, $4, $5, $6)
        `, entry.PaymentIntentID, entry.Account, entry.Amount,
			entry.Currency, entry.EntryType, entry.Description,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrLedgerAlreadyRecorded
			}
			return fmt.Errorf("inserting ledger entry for account %q: %w", entry.Account, err)
		}
	}

	log.Info().
		Int64("payment_intent_id", pi.ID).
		Int64("amount", pi.Amount).
		Int64("fee", fee).
		Int64("merchant_amount", merchantAmount).
		Str("currency", pi.Currency).
		Msg("ledger entries recorded")

	return nil
}

// RecordRefundTx writes the reversal for one refund: money leaves the
// merchant and flows back through the provider.
//
//	DEBIT  merchant:<id>:refund:<refund_id>    -refundAmount
//	CREDIT provider:<name>:refund:<refund_id>  +refundAmount
//
// The gateway fee is not returned (Stripe does not refund fees by default).
// Accounts carry the refund id so UNIQUE(payment_intent_id, account) allows
// any number of refunds per payment while still rejecting a second write for
// the same refund. (They used to carry the amount, so two refunds of equal
// amount collided.)
func (s *Service) RecordRefundTx(ctx context.Context, tx pgx.Tx, pi *models.PaymentIntent, refundID, refundAmount int64) error {
	if refundAmount <= 0 || refundAmount > pi.Amount {
		return fmt.Errorf("refund amount %d out of bounds for payment %d", refundAmount, pi.ID)
	}
	suffix := fmt.Sprintf(":refund:%d", refundID)

	entries := []models.LedgerEntry{
		{
			PaymentIntentID: pi.ID,
			Account:         fmt.Sprintf("merchant:%d%s", pi.MerchantID, suffix),
			Amount:          -refundAmount,
			Currency:        pi.Currency,
			EntryType:       models.EntryDebit,
			Description:     fmt.Sprintf("refund debit (payment %d)", pi.ID),
		},
		{
			PaymentIntentID: pi.ID,
			Account:         fmt.Sprintf("provider:%s%s", pi.Provider, suffix),
			Amount:          refundAmount,
			Currency:        pi.Currency,
			EntryType:       models.EntryCredit,
			Description:     fmt.Sprintf("refund credit back to provider (payment %d)", pi.ID),
		},
	}
	if err := validateBalance(entries); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.Exec(ctx, `
            INSERT INTO ledger_entries
                (payment_intent_id, account, amount, currency, entry_type, description)
            VALUES ($1, $2, $3, $4, $5, $6)
        `, e.PaymentIntentID, e.Account, e.Amount, e.Currency, e.EntryType, e.Description); err != nil {
			if isUniqueViolation(err) {
				return ErrLedgerAlreadyRecorded
			}
			return fmt.Errorf("insert refund ledger entry: %w", err)
		}
	}
	log.Info().Int64("payment_intent_id", pi.ID).Int64("refund_id", refundID).Int64("refund_amount", refundAmount).Msg("refund ledger recorded")
	return nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key")
}

// GetBalance returns the current balance for a given account.
// Sum of all ledger entries for that account.
func (s *Service) GetBalance(ctx context.Context, account, currency string) (int64, error) {
	var balance int64
	err := s.db.QueryRow(ctx, `
        SELECT COALESCE(SUM(amount), 0)
        FROM ledger_entries
        WHERE account = $1 AND currency = $2
    `, account, currency).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("computing balance for %q: %w", account, err)
	}
	return balance, nil
}

// GetEntriesForPayment fetches all ledger entries for a payment intent.
// Used for auditing and reconciliation.
func (s *Service) GetEntriesForPayment(ctx context.Context, paymentIntentID int64) ([]models.LedgerEntry, error) {
	rows, err := s.db.Query(ctx, `
        SELECT id, payment_intent_id, account, amount, currency, entry_type, COALESCE(description, ''), created_at
        FROM ledger_entries
        WHERE payment_intent_id = $1
        ORDER BY id ASC
    `, paymentIntentID)
	if err != nil {
		return nil, fmt.Errorf("querying ledger entries: %w", err)
	}
	defer rows.Close()

	var entries []models.LedgerEntry
	for rows.Next() {
		var e models.LedgerEntry
		var entryType string
		if err := rows.Scan(
			&e.ID, &e.PaymentIntentID, &e.Account, &e.Amount,
			&e.Currency, &entryType, &e.Description, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning ledger entry: %w", err)
		}
		e.EntryType = models.EntryType(entryType)
		entries = append(entries, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ledger rows: %w", err)
	}

	return entries, nil
}

// validateBalance ensures all entries net to zero before writing.
// This is the mathematical guarantee of double-entry bookkeeping.
func validateBalance(entries []models.LedgerEntry) error {
	var net int64
	for _, e := range entries {
		net += e.Amount
	}
	if net != 0 {
		return fmt.Errorf(
			"ledger imbalance detected: entries net to %d instead of 0 — critical bug",
			net,
		)
	}
	return nil
}
