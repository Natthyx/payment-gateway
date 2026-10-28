package payment

import (
    "bytes"
    "context"
    "crypto/sha256"
    "encoding/hex"
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
//  1. Check idempotency cache — replay if already seen with the same request
//  2. Validate request
//  3. Insert payment intent with status "created" and the request fingerprint
//     (a key collision is resolved against the stored intent, see replayExisting)
//  4. Call provider to get checkout URL
//  5. Update intent with provider TX ID, checkout URL, status "processing"
//  6. Cache response for idempotency replay
//  7. Return checkout URL to caller
func (s *Service) CreateIntent(
    ctx context.Context,
    merchantID int64,
    req models.CreatePaymentIntentRequest,
) (*models.CreatePaymentIntentResponse, error) {
    fp := requestFingerprint(req)

    // ── Step 1: Idempotency check ─────────────────────────────────────────────
    // If we've seen this key before, replay the cached response.
    // The merchant gets the same response without hitting the provider again.
    cached, err := s.idempotency.Get(ctx, merchantID, req.IdempotencyKey)
    if err != nil {
        log.Warn().Err(err).Msg("idempotency cache read failed — proceeding without cache")
    }
    if cached != nil {
        if cached.Fingerprint != "" && cached.Fingerprint != fp {
            return nil, apierr.IdempotencyConflict("idempotency_key already used with different request parameters")
        }
        var resp models.CreatePaymentIntentResponse
        if err := unmarshalJSON(cached.Body, &resp); err == nil {
            log.Debug().
                Int64("merchant_id", merchantID).
                Str("idempotency_key", req.IdempotencyKey).
                Msg("idempotency cache hit — replaying cached response")
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
            (merchant_id, idempotency_key, amount, currency, provider, status, metadata, request_fingerprint)
        VALUES ($1, $2, $3, $4, $5, 'created', $6, $7)
        RETURNING id, merchant_id, idempotency_key, amount, currency,
                  provider, status, created_at, updated_at
    `, merchantID, req.IdempotencyKey, req.Amount,
        req.Currency, string(req.Provider), req.Metadata, fp,
    ).Scan(
        &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount,
        &pi.Currency, &pi.Provider, &status,
        &pi.CreatedAt, &pi.UpdatedAt,
    )
    if err != nil {
        if isUniqueViolation(err) {
            return s.replayExisting(ctx, merchantID, req.IdempotencyKey, fp)
        }
        return nil, fmt.Errorf("inserting payment intent: %w", err)
    }
    pi.Status = models.PaymentStatus(status)

    // ── Step 4: Call provider ─────────────────────────────────────────────────
    chargeResp, err := p.CreateCharge(ctx, provider.ChargeRequest{
        Amount:   pi.Amount,
        Currency: pi.Currency,
        // Unique across the whole gateway. The merchant's key is only unique
        // per merchant; sent as-is, two merchants using "order_1" would get
        // each other's provider transaction (Stripe replays by key; Chapa
        // and Telebirr use it as the transaction reference).
        IdempotencyKey: fmt.Sprintf("gw_pi_%d", pi.ID),
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
    if _, err := s.db.Exec(ctx, `
        UPDATE payment_intents
        SET status = 'processing', provider_tx_id = $1, checkout_url = $2
        WHERE id = $3
    `, chargeResp.ProviderTxID, chargeResp.CheckoutURL, pi.ID); err != nil {
        // The provider has a transaction we could not link. Surface it rather
        // than reporting success for an intent webhooks cannot find.
        log.Error().Err(err).Int64("payment_intent_id", pi.ID).Str("provider_tx_id", chargeResp.ProviderTxID).
            Msg("charge created but intent update failed")
        return nil, fmt.Errorf("recording provider transaction: %w", err)
    }

    pi.ProviderTxID = chargeResp.ProviderTxID
    pi.Status = models.StatusProcessing

    resp := &models.CreatePaymentIntentResponse{
        PaymentIntent: &pi,
        CheckoutURL:   chargeResp.CheckoutURL,
    }

    // ── Step 6: Cache for idempotency replay ──────────────────────────────────
    if body, err := marshalJSON(resp); err == nil {
        s.idempotency.Set(ctx, merchantID, req.IdempotencyKey, db.CachedResponse{ //nolint:errcheck
            StatusCode:  201,
            Body:        body,
            Fingerprint: fp,
        })
    }

    metrics.PaymentsCreated.WithLabelValues(string(pi.Provider), pi.Currency).Inc()
    log.Info().
        Int64("payment_intent_id", pi.ID).
        Int64("amount", pi.Amount).
        Str("currency", pi.Currency).
        Str("provider", string(pi.Provider)).
        Msg("payment intent created")

    return resp, nil
}

// replayExisting resolves an idempotency-key collision against the intent
// already stored for (merchant, key):
//   - different request parameters   → 409 idempotency_conflict
//   - first request still in flight  → 409 idempotency_key_in_use (retry later)
//   - first request failed           → 409 idempotency_conflict (use a new key)
//   - otherwise                      → replay the original response
func (s *Service) replayExisting(ctx context.Context, merchantID int64, key, fp string) (*models.CreatePaymentIntentResponse, error) {
    var pi models.PaymentIntent
    var status, prov, storedFP, checkoutURL string
    err := s.db.QueryRow(ctx, `
        SELECT id, merchant_id, idempotency_key, amount, currency, provider, status,
               COALESCE(provider_tx_id, ''), metadata, created_at, updated_at,
               COALESCE(request_fingerprint, ''), COALESCE(checkout_url, '')
        FROM payment_intents
        WHERE merchant_id = $1 AND idempotency_key = $2
    `, merchantID, key).Scan(
        &pi.ID, &pi.MerchantID, &pi.IdempotencyKey, &pi.Amount, &pi.Currency, &prov, &status,
        &pi.ProviderTxID, &pi.Metadata, &pi.CreatedAt, &pi.UpdatedAt, &storedFP, &checkoutURL,
    )
    if err != nil {
        return nil, fmt.Errorf("loading intent for idempotent replay: %w", err)
    }
    pi.Status = models.PaymentStatus(status)
    pi.Provider = models.Provider(prov)

    switch {
    case storedFP != "" && storedFP != fp:
        return nil, apierr.IdempotencyConflict("idempotency_key already used with different request parameters")
    case pi.Status == models.StatusCreated:
        return nil, apierr.IdempotencyInUse("a request with this idempotency_key is in progress; retry later")
    case pi.Status == models.StatusFailed:
        return nil, apierr.IdempotencyConflict("the request with this idempotency_key failed; retry with a new key")
    }
    // The cached response had status "processing"; keep replays identical
    // to it even if the intent has moved on since.
    if pi.Status != models.StatusProcessing {
        pi.Status = models.StatusProcessing
    }
    return &models.CreatePaymentIntentResponse{PaymentIntent: &pi, CheckoutURL: checkoutURL}, nil
}

// requestFingerprint identifies the parameters an idempotency key was first
// used with. Metadata is compacted so whitespace differences do not matter.
func requestFingerprint(req models.CreatePaymentIntentRequest) string {
    meta := []byte(req.Metadata)
    var buf bytes.Buffer
    if len(meta) > 0 && json.Compact(&buf, meta) == nil {
        meta = buf.Bytes()
    }
    sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", req.Amount, req.Currency, req.Provider, meta)))
    return hex.EncodeToString(sum[:])
}

// GetIntent fetches a payment intent by ID, scoped to the merchant.
func (s *Service) GetIntent(ctx context.Context, merchantID, intentID int64) (*models.PaymentIntent, error) {
    var pi models.PaymentIntent
    var status, prov string
    err := s.db.QueryRow(ctx, `
        SELECT id, merchant_id, idempotency_key, amount, currency,
               provider, status, COALESCE(provider_tx_id, ''), COALESCE(failure_reason, ''),
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
               provider, status, COALESCE(provider_tx_id, ''), COALESCE(failure_reason, ''),
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