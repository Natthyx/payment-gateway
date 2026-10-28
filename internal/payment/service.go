package payment

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rs/zerolog/log"

    "github.com/Natthyx/payment-gateway/internal/apierr"
    "github.com/Natthyx/payment-gateway/internal/db"
    "github.com/Natthyx/payment-gateway/internal/ledger"
    "github.com/Natthyx/payment-gateway/internal/metrics"
    "github.com/Natthyx/payment-gateway/internal/models"
    "github.com/Natthyx/payment-gateway/internal/provider"
)

// Service orchestrates the full payment lifecycle.
// It coordinates: idempotency → provider → DB → ledger
type Service struct {
    db           *pgxpool.Pool
    registry     *provider.Registry
    ledger       *ledger.Service
    idempotency  *db.IdempotencyStore
    feePercent   float64
    callbackURL  func(provider string) string
}

func NewService(
    pool *pgxpool.Pool,
    registry *provider.Registry,
    ledger *ledger.Service,
    idempotency *db.IdempotencyStore,
    feePercent float64,
    callbackURL func(string) string,
) *Service {
    return &Service{
        db:          pool,
        registry:    registry,
        ledger:      ledger,
        idempotency: idempotency,
        feePercent:  feePercent,
        callbackURL: callbackURL,
    }
}

// CreateIntent is the core payment flow. Steps:
//  1. Check idempotency cache — replay if already seen
//  2. Validate request
//  3. Insert payment intent with status "created"
//  4. Call provider to get checkout URL
//  5. Update intent with provider TX ID + status "processing"
//  6. Cache response for idempotency replay
//  7. Return checkout URL to caller
func (s *Service) CreateIntent(
    ctx context.Context,
    merchantID int64,
    req models.CreatePaymentIntentRequest,
) (*models.CreatePaymentIntentResponse, error) {

    // ── Step 1: Idempotency check ─────────────────────────────────────────────
    // If we've seen this key before, replay the cached response.
    // The merchant gets the same response without hitting the provider again.
    cached, err := s.idempotency.Get(ctx, merchantID, req.IdempotencyKey)
    if err != nil {
        log.Warn().Err(err).Msg("idempotency cache read failed — proceeding without cache")
    }
    if cached != nil {
        log.Info().
            Int64("merchant_id", merchantID).
            Str("idempotency_key", req.IdempotencyKey).
            Msg("idempotency cache hit — replaying cached response")

        var resp models.CreatePaymentIntentResponse
        if err := unmarshalJSON(cached.Body, &resp); err == nil {
            return &resp, nil
        }
    }

    // Step 2: validation is performed by the handler via ValidateCreatePaymentIntent
    // before this service is reached. We re-check the provider registry only.
    p, err := s.registry.Get(string(req.Provider))
    if err != nil {
        return nil, apierr.InvalidRequest(fmt.Sprintf("unsupported provider %q", req.Provider))
    }

    // ── Step 3: Persist intent with "created" status ──────────────────────────
    // We write to DB before calling the provider.
    // WHY: if the provider call succeeds but DB write fails, we'd have
    // charged the customer with no record. DB first = safer failure mode.
    var pi models.PaymentIntent
    var status string
    err = s.db.QueryRow(ctx, `
        INSERT INTO payment_intents
            (merchant_id, idempotency_key, amount, currency, provider, status, metadata)
        VALUES ($1, $2, $3, $4, $5, 'created', $6)
        RETURNING id, merchant_id, idempotency_key, amount, currency,
                  provider, status, created_at, updated_at
    `, merchantID, req.IdempotencyKey, req.Amount,
        req.Currency, string(req.Provider), req.Metadata,
    ).Scan(
        &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount,
        &pi.Currency, &pi.Provider, &status,
        &pi.CreatedAt, &pi.UpdatedAt,
    )
    if err != nil {
        if isUniqueViolation(err) {
            return nil, apierr.IdempotencyConflict("idempotency_key already used")
        }
        return nil, fmt.Errorf("inserting payment intent: %w", err)
    }
    pi.Status = models.PaymentStatus(status)

    // ── Step 4: Call provider ─────────────────────────────────────────────────
    chargeResp, err := p.CreateCharge(ctx, provider.ChargeRequest{
        Amount:         pi.Amount,
        Currency:       pi.Currency,
        IdempotencyKey: pi.IdempotencyKey,
        Description:    fmt.Sprintf("Payment #%d", pi.ID),
        CallbackURL:    s.callbackURL(string(req.Provider)),
    })
    if err != nil {
        _, _ = s.db.Exec(ctx, `
            UPDATE payment_intents
            SET status = 'failed', failure_reason = $1 WHERE id = $2
        `, err.Error(), pi.ID)
        return nil, apierr.Provider(fmt.Sprintf("provider %s charge failed: %v", req.Provider, err))
    }

    // ── Step 5: Update intent with provider TX ID ─────────────────────────────
    s.db.Exec(ctx, `
        UPDATE payment_intents
        SET status = 'processing', provider_tx_id = $1
        WHERE id = $2
    `, chargeResp.ProviderTxID, pi.ID) //nolint:errcheck

    pi.ProviderTxID = chargeResp.ProviderTxID
    pi.Status = models.StatusProcessing

    resp := &models.CreatePaymentIntentResponse{
        PaymentIntent: &pi,
        CheckoutURL:   chargeResp.CheckoutURL,
    }

    // ── Step 6: Cache for idempotency replay ──────────────────────────────────
    if body, err := marshalJSON(resp); err == nil {
        s.idempotency.Set(ctx, merchantID, req.IdempotencyKey, db.CachedResponse{ //nolint:errcheck
            StatusCode: 201,
            Body:       body,
        })
    }

    metrics.PaymentsCreated.WithLabelValues(string(pi.Provider), pi.Currency).Inc()
    log.Info().
        Int64("payment_intent_id", pi.ID).
        Int64("amount", pi.Amount).
        Str("currency", pi.Currency).
        Str("provider", string(pi.Provider)).
        Str("checkout_url", chargeResp.CheckoutURL).
        Msg("payment intent created")

    return resp, nil
}

// GetIntent fetches a payment intent by ID, scoped to the merchant.
func (s *Service) GetIntent(ctx context.Context, merchantID, intentID int64) (*models.PaymentIntent, error) {
    var pi models.PaymentIntent
    var status, prov string
    err := s.db.QueryRow(ctx, `
        SELECT id, merchant_id, idempotency_key, amount, currency,
               provider, status, provider_tx_id, failure_reason,
               metadata, created_at, updated_at
        FROM payment_intents
        WHERE id = $1 AND merchant_id = $2
    `, intentID, merchantID).Scan(
        &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount, &pi.Currency,
        &prov, &status, &pi.ProviderTxID, &pi.FailureReason,
        &pi.Metadata, &pi.CreatedAt, &pi.UpdatedAt,
    )
    if errors.Is(err, pgx.ErrNoRows) {
        return nil, apierr.NotFound("payment intent not found")
    }
    if err != nil {
        return nil, fmt.Errorf("loading payment intent: %w", err)
    }
    pi.Status = models.PaymentStatus(status)
    pi.Provider = models.Provider(prov)
    return &pi, nil
}

// ListIntents returns paginated payment intents for a merchant.
func (s *Service) ListIntents(ctx context.Context, merchantID int64, page, limit int) ([]models.PaymentIntent, int, error) {
    if page < 1 {
        page = 1
    }
    if limit < 1 || limit > 100 {
        limit = 20
    }
    offset := (page - 1) * limit

    rows, err := s.db.Query(ctx, `
        SELECT id, merchant_id, idempotency_key, amount, currency,
               provider, status, provider_tx_id, failure_reason,
               created_at, updated_at
        FROM payment_intents
        WHERE merchant_id = $1
        ORDER BY created_at DESC
        LIMIT $2 OFFSET $3
    `, merchantID, limit, offset)
    if err != nil {
        return nil, 0, fmt.Errorf("querying payment intents: %w", err)
    }
    defer rows.Close()

    var intents []models.PaymentIntent
    for rows.Next() {
        var pi models.PaymentIntent
        var status, prov string
        if err := rows.Scan(
            &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount, &pi.Currency,
            &prov, &status, &pi.ProviderTxID, &pi.FailureReason,
            &pi.CreatedAt, &pi.UpdatedAt,
        ); err != nil {
            return nil, 0, fmt.Errorf("scanning intent: %w", err)
        }
        pi.Status = models.PaymentStatus(status)
        pi.Provider = models.Provider(prov)
        intents = append(intents, pi)
    }

    if err := rows.Err(); err != nil {
        return nil, 0, fmt.Errorf("iterating intents: %w", err)
    }

    var total int
    s.db.QueryRow(ctx, `SELECT COUNT(*) FROM payment_intents WHERE merchant_id = $1`, merchantID).Scan(&total) //nolint:errcheck

    if intents == nil {
        intents = []models.PaymentIntent{}
    }

    return intents, total, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func isUniqueViolation(err error) bool {
    if err == nil {
        return false
    }
    msg := err.Error()
    return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key")
}

func marshalJSON(v interface{}) ([]byte, error) { return json.Marshal(v) }

func unmarshalJSON(data []byte, v interface{}) error { return json.Unmarshal(data, v) }