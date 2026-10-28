# Payment Gateway — Technical Architecture Overview

## System Summary

A multi-provider payment orchestration service that abstracts Stripe, Chapa, and Telebirr behind a unified API. Implements double-entry ledger accounting, multi-layered idempotency protection, webhook security via HMAC-SHA256, and background workers for async processing.

**Status:** Beta — core financial logic implemented and tested  
**Language:** Go 1.24  
**Database:** PostgreSQL 16  
**Cache:** Redis 7  
**Architecture Style:** Monolithic with background workers

---

## Core Components

### 1. HTTP API Layer (`cmd/server/main.go`)

**Framework:** Chi router  
**Port:** 8080 (configurable)  
**Middleware Stack:**
- Panic recovery
- Structured logging (zerolog)
- Request ID correlation
- Real IP extraction
- Global rate limit (200 req/min per IP)
- Request timeout (30s)
- Prometheus instrumentation
- API key authentication (merchant routes only)
- Per-merchant rate limit (600 req/min, Redis-backed)

**Key Routes:**
```
GET    /health                       # Health check
GET    /metrics                      # Prometheus metrics
POST   /merchants                    # Create merchant + API key
POST   /payments                     # Create payment intent (auth required)
GET    /payments/:id                 # Fetch payment intent (auth required)
POST   /payments/:id/refunds         # Refund payment (auth required)
POST   /webhooks/stripe              # Stripe webhook receiver
POST   /webhooks/chapa               # Chapa webhook receiver
POST   /webhooks/telebirr            # Telebirr webhook receiver
POST   /webhook-endpoints            # Register merchant webhook (auth required)
```

---

### 2. Payment Service (`internal/payment/service.go`)

**Responsibilities:**
- Payment intent creation
- Idempotency management
- Provider coordination
- Payment lifecycle

**Key Operations:**

#### CreateIntent Flow:
```
1. Check Redis idempotency cache (24h TTL)
   ├─ HIT  → return cached response (no DB/provider call)
   └─ MISS → continue

2. Validate request (amount > 0, currency valid, provider exists)

3. Insert payment_intents row (status: created)
   └─ UNIQUE(merchant_id, idempotency_key) enforced

4. Call provider.CreateCharge()
   └─ Returns checkout_url + provider_tx_id

5. Update payment_intents (status: processing, provider_tx_id set)

6. Cache response in Redis (24h TTL)

7. Return checkout_url to merchant
```

**Idempotency Guarantee:**
- Duplicate requests return identical response
- Customer never charged twice for same idempotency key
- Redis cache optimizes performance
- Database UNIQUE constraint is authoritative

**Code Reference:** `internal/payment/service.go:60-170`

---

### 3. Ledger Service (`internal/ledger/service.go`)

**Responsibilities:**
- Double-entry bookkeeping
- Ledger entry validation
- Balance calculation

**Double-Entry Implementation:**

Every successful payment creates exactly three entries:

```
DEBIT  provider:stripe     -1050  (money leaves provider)
CREDIT merchant:42         +1020  (merchant receives net)
CREDIT gateway:fees        +30    (gateway commission)

Validation: -1050 + 1020 + 30 = 0 ✓
```

**Validation Logic:**
```go
func validateBalance(entries []models.LedgerEntry) error {
    var net int64
    for _, e := range entries {
        net += e.Amount
    }
    if net != 0 {
        return fmt.Errorf("ledger imbalance: net=%d", net)
    }
    return nil
}
```

**If validation fails:**
- Error returned before COMMIT
- Transaction rolls back
- No partial ledger writes possible

**Database Protections:**
```sql
-- Prevents duplicate ledger writes
UNIQUE INDEX (payment_intent_id, account)

-- Enforces append-only
CREATE TRIGGER prevent_ledger_modification
BEFORE UPDATE OR DELETE ON ledger_entries
RAISE EXCEPTION 'ledger_entries is append-only'
```

**Refund Accounting:**
Refunds create inverse entries with `:refund:<amount>` suffix for uniqueness:
```
DEBIT  merchant:42:refund:1050    -1050
CREDIT provider:stripe:refund:1050 +1050
```

**Code Reference:** `internal/ledger/service.go`, `migrations/001_init.up.sql`

---

### 4. Webhook Service (`internal/webhook/service.go`)

**Architecture:** Inbox Pattern (Two-Phase Processing)

#### Phase 1 — Receive (Synchronous, Fast)
```
POST /webhooks/stripe
  ├─ Extract provider_event_id from body
  ├─ INSERT INTO webhooks (provider, provider_event_id, payload, signature)
  │    ON CONFLICT (provider, provider_event_id) DO NOTHING
  │
  └─ Return 200 OK immediately (< 50ms typical)
```

**Goal:** Acknowledge webhook before provider timeout (30s)

#### Phase 2 — Process (Asynchronous, Transactional)
```go
// Worker polls every 10 seconds
tx, _ := db.Begin()

row := tx.QueryRow(`
    SELECT * FROM webhooks
    WHERE status = 'pending' AND attempts < 5
    ORDER BY created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
`)

// Row is locked, other workers skip it

provider.VerifyWebhook(payload, signature)
  ├─ Parse HMAC signature
  ├─ Verify timestamp within 5min window
  ├─ Compute expected HMAC-SHA256
  └─ Constant-time comparison

UPDATE payment_intents SET status = 'succeeded' WHERE ...
  └─ Only updates if current status NOT IN ('succeeded', 'refunded')

ledger.RecordPaymentTx(tx, payment_intent, fee_percent)
  └─ Inserts 3 ledger entries, validates sum = 0

tx.Commit()

outbound.Enqueue(merchant_id, 'payment.succeeded', payment_intent)
```

**Retry Logic:**
- Signature verification fails → retry (up to 5 attempts)
- Database error → retry
- Ledger already exists → mark processed (idempotent)
- After 5 failures → status: failed (dead letter)

**Concurrency:**
- `FOR UPDATE SKIP LOCKED` enables parallel workers
- Multiple pods can run without leader election
- Each worker processes different rows

**Code Reference:** `internal/webhook/service.go:74-256`

---

### 5. Outbound Webhook Service (`internal/outbound/service.go`)

**Purpose:** Deliver events to merchant-configured webhook URLs

**Flow:**
```
Payment Succeeded
  └─ Enqueue(merchant_id, 'payment.succeeded', payload)
       └─ INSERT INTO outbound_webhook_deliveries FOR EACH merchant endpoint

Worker polls every 5 seconds
  ├─ SELECT WHERE status = 'pending' AND next_attempt_at <= NOW()
  │    FOR UPDATE SKIP LOCKED LIMIT 1
  │
  ├─ Compute HMAC-SHA256:
  │    secret = OUTBOUND_WEBHOOK_SIGNING_SECRET
  │    payload = timestamp + "." + json_body
  │    signature = HMAC-SHA256(secret, payload)
  │
  ├─ POST to merchant URL
  │    Header: X-Gateway-Signature: t=<unix>,v1=<hex_signature>
  │    Header: Idempotency-Key: dlv_<delivery_id>
  │    Timeout: 10 seconds
  │
  └─ Response handling:
       ├─ 2xx → status: delivered, delivered_at: NOW
       └─ Non-2xx or timeout → exponential backoff
            ├─ Attempt 1: next_attempt_at = NOW + 30s
            ├─ Attempt 2: NOW + 1m
            ├─ Attempt 3: NOW + 2m
            ├─ ...
            ├─ Attempt 8: NOW + 64m (capped at 6h)
            └─ After 8 → status: dead (requires manual intervention)
```

**Exponential Backoff Formula:**
```go
delay := baseBackoff * math.Pow(2, attempt-1)
if delay > maxBackoff { delay = maxBackoff }
```

**Merchant Verification:**
Merchants must verify `X-Gateway-Signature` header:
```go
timestamp, v1 := parseSignature(header)

// Check timestamp within 5-minute window
if time.Since(timestamp) > 5*time.Minute {
    return 401 Unauthorized
}

// Compute expected HMAC
mac := hmac.New(sha256.New, []byte(shared_secret))
mac.Write([]byte(timestamp + "." + request_body))
expected := hex.EncodeToString(mac.Sum(nil))

// Constant-time compare
if !hmac.Equal([]byte(expected), []byte(v1)) {
    return 401 Unauthorized
}
```

**Code Reference:** `internal/outbound/service.go`

---

### 6. Provider Abstraction (`internal/provider/`)

**Design Pattern:** Strategy Pattern

**Interface:**
```go
type Provider interface {
    Name() string
    CreateCharge(ctx, ChargeRequest) (*ChargeResponse, error)
    VerifyWebhook(payload, signature []byte) (*WebhookEvent, error)
    ParseEventMetadata(payload []byte) (*EventMetadata, error)
    GetTransaction(ctx, txID) (*Transaction, error)
}

type Refunder interface {
    Refund(ctx, RefundRequest) (*RefundResponse, error)
}
```

**Implementations:**

#### Stripe (`internal/provider/stripe/`)
- **API:** PaymentIntents (form-encoded POST, HTTP Basic Auth)
- **Webhook Signature:** `Stripe-Signature: t=<ts>,v1=<hmac>`
- **HMAC Algorithm:** SHA256
- **Timestamp Tolerance:** 5 minutes
- **Refunds:** Supported via Refunds API

#### Chapa (`internal/provider/chapa/`)
- **API:** Charge API (JSON POST, bearer token)
- **Webhook Signature:** Custom format
- **Verification:** API (calls verify endpoint to confirm)

#### Telebirr (`internal/provider/telebirr/`)
- **API:** H5 payment API
- **Webhook Signature:** RSA signature
- **Verification:** RSA public key

**Registry Pattern:**
```go
registry := provider.NewRegistry()
registry.Register(stripe.New(secretKey, webhookKey))
registry.Register(chapa.New(secretKey))
registry.Register(telebirr.New(appID, appKey, publicKey, shortCode))

// Business logic
provider, _ := registry.Get("stripe")
response, _ := provider.CreateCharge(ctx, request)
```

**Benefits:**
- Business logic depends on interface, not concrete providers
- Adding new provider requires no changes to PaymentService
- Easy to mock for testing

**Code Reference:** `internal/provider/interface.go`, `internal/provider/*/`

---

### 7. Reconciliation Service (`internal/reconcile/reconcile.go`)

**Purpose:** Detect and correct provider state drift

**Algorithm:**
```
Every 5 minutes:
  ├─ Find payments WHERE status = 'processing' AND updated_at < NOW() - 15min
  │
  ├─ For each stale payment:
  │    ├─ provider.GetTransaction(provider_tx_id)
  │    ├─ Compare local status vs provider status
  │    └─ If provider says 'succeeded':
  │         ├─ BEGIN TRANSACTION
  │         ├─ UPDATE payment_intents SET status = 'succeeded'
  │         ├─ ledger.RecordPaymentTx()
  │         └─ COMMIT
  │
  └─ Log correction (WARNING level)
```

**Why Necessary:**
- Webhook delivery can fail (network, provider outage, signing key rotation)
- Provider API may succeed but webhook never arrives
- Catches cases where provider and gateway disagree

**Example Scenario:**
1. Stripe charge succeeds at 10:00
2. Webhook delivery fails (network issue)
3. Gateway still shows status: processing
4. Reconciler runs at 10:20
5. Calls Stripe API: GetTransaction(tx_id)
6. Stripe says: "succeeded at 10:00"
7. Reconciler updates status to succeeded + writes ledger
8. Payment now correctly reflects reality

**Tuning:**
- Interval: 5 minutes (configurable)
- Stale threshold: 15 minutes (configurable)
- Batch size: 100 per run (configurable)

**Code Reference:** `internal/reconcile/reconcile.go`

---

### 8. Authentication (`internal/merchant/service.go`, `internal/middleware/merchant.go`)

**API Key Format:**
```
gw_<env>_<lookup_16hex>_<secret_48hex>

Example: gw_live_a1b2c3d4e5f6g7h8_0123456789abcdef...
```

**Components:**
- `gw_`: Product prefix
- `live|test`: Environment (prevents test keys in production)
- `lookup`: 16 hex chars (8 random bytes), stored plaintext, indexed
- `secret`: 48 hex chars (24 random bytes), bcrypt-hashed, never stored plaintext

**Authentication Flow:**
```
1. Extract Authorization header: "Bearer gw_live_..."
2. Parse key: split by "_", extract env + lookup + secret
3. Validate environment matches config (live vs test)
4. Query: SELECT * FROM merchants WHERE api_key_lookup = $1
   └─ O(1) lookup via index
5. bcrypt.CompareHashAndPassword(stored_hash, provided_secret)
   └─ Constant-time comparison
6. Check merchant.is_active = true
7. Inject merchant into request context
```

**Security Properties:**
- **O(1) lookup:** No table scan, scales to millions of merchants
- **Bcrypt:** Cost factor 12, computationally expensive to crack
- **Split key:** Compromise of `api_key_lookup` alone is harmless
- **Environment isolation:** Test keys rejected in live environment

**Key Generation:**
```go
func mintKey() (apiKey, lookup, hash string, err error) {
    lookup, _ := randomHex(8)  // 16 chars
    secret, _ := randomHex(24) // 48 chars
    hash, _ := bcrypt.GenerateFromPassword([]byte(secret), 12)
    apiKey = fmt.Sprintf("gw_%s_%s_%s", env, lookup, secret)
    return apiKey, lookup, hash, nil
}
```

**Code Reference:** `internal/merchant/service.go:82-105`, `internal/middleware/merchant.go`

---

### 9. Database Layer (`internal/db/`)

**Connection Management:**
- pgx/v5 connection pool
- Pool size: Configurable (default: 4 min, 16 max)
- Idle timeout: 30 minutes
- Health checks: 5 second interval

**Migrations:**
- golang-migrate for versioning
- Applied automatically on boot
- File location: `file://migrations` (configurable)
- Migrations bundled in Docker image

**Key Tables:**

#### merchants
```sql
CREATE TABLE merchants (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    email        TEXT NOT NULL UNIQUE,
    api_key_hash TEXT NOT NULL,
    api_key_lookup TEXT NOT NULL,  -- indexed for O(1) auth
    is_active    BOOLEAN DEFAULT true,
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_merchants_lookup ON merchants(api_key_lookup);
```

#### payment_intents
```sql
CREATE TABLE payment_intents (
    id               BIGSERIAL PRIMARY KEY,
    merchant_id      BIGINT REFERENCES merchants(id),
    idempotency_key  TEXT NOT NULL,
    amount           BIGINT CHECK (amount > 0),
    currency         TEXT NOT NULL,
    provider         TEXT CHECK (provider IN ('stripe','chapa','telebirr')),
    status           TEXT CHECK (status IN ('created','processing','succeeded','failed','refunded')),
    provider_tx_id   TEXT,
    failure_reason   TEXT,
    metadata         JSONB,
    created_at       TIMESTAMPTZ DEFAULT NOW(),
    updated_at       TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (merchant_id, idempotency_key)  -- idempotency constraint
);

CREATE INDEX idx_pi_merchant_id ON payment_intents(merchant_id);
CREATE INDEX idx_pi_status ON payment_intents(status);
CREATE INDEX idx_pi_provider_tx ON payment_intents(provider_tx_id);
```

#### ledger_entries
```sql
CREATE TABLE ledger_entries (
    id                BIGSERIAL PRIMARY KEY,
    payment_intent_id BIGINT REFERENCES payment_intents(id),
    account           TEXT NOT NULL,
    amount            BIGINT NOT NULL,
    currency          TEXT NOT NULL,
    entry_type        TEXT CHECK (entry_type IN ('debit','credit')),
    description       TEXT,
    created_at        TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_ledger_payment_intent ON ledger_entries(payment_intent_id);
CREATE INDEX idx_ledger_account ON ledger_entries(account);
CREATE UNIQUE INDEX uq_ledger_payment_account ON ledger_entries(payment_intent_id, account);

-- Append-only enforcement
CREATE TRIGGER trg_no_update_ledger BEFORE UPDATE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_modification();

CREATE TRIGGER trg_no_delete_ledger BEFORE DELETE ON ledger_entries
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_modification();
```

#### webhooks
```sql
CREATE TABLE webhooks (
    id                BIGSERIAL PRIMARY KEY,
    provider          TEXT NOT NULL,
    provider_event_id TEXT,  -- for idempotency
    event_type        TEXT NOT NULL,
    payload           JSONB NOT NULL,
    signature         TEXT NOT NULL,
    status            TEXT CHECK (status IN ('pending','processed','failed')),
    attempts          INT DEFAULT 0,
    last_error        TEXT,
    created_at        TIMESTAMPTZ DEFAULT NOW(),
    processed_at      TIMESTAMPTZ
);

CREATE INDEX idx_webhooks_status ON webhooks(status);
CREATE UNIQUE INDEX uq_webhooks_provider_event ON webhooks(provider, provider_event_id)
    WHERE provider_event_id IS NOT NULL;
```

**Code Reference:** `internal/db/postgres.go`, `migrations/*.up.sql`

---

### 10. Redis Layer (`internal/db/idempotency.go`)

**Use Cases:**
1. Idempotency cache (24h TTL)
2. Rate limiting (per-merchant, per-IP)

**Idempotency Store:**
```go
type CachedResponse struct {
    StatusCode int
    Body       json.RawMessage
}

// Key format: "idempotency:<merchant_id>:<idempotency_key>"
// TTL: 24 hours
```

**Operations:**
```go
// Check cache
cached, _ := idempotency.Get(ctx, merchantID, key)
if cached != nil {
    return replayCachedResponse(cached.StatusCode, cached.Body)
}

// Store response
idempotency.Set(ctx, merchantID, key, CachedResponse{
    StatusCode: 201,
    Body: json.Marshal(response),
})
```

**Why 24h TTL:**
- Matches Stripe's idempotency window
- Balances memory usage vs replay window
- After 24h, Redis expires key → falls back to database check

**Redis Configuration:**
- Host: Configurable via REDIS_ADDR (default: localhost:6379)
- Password: Optional via REDIS_PASSWORD
- Database: Configurable via REDIS_DB (default: 0)
- Connection timeout: 5 seconds

**Code Reference:** `internal/db/idempotency.go`, `internal/db/redis.go`

---

## Background Workers

Three workers run in-process as goroutines:

### 1. Webhook Worker
- **Interval:** 10 seconds
- **Query:** `SELECT ... WHERE status = 'pending' FOR UPDATE SKIP LOCKED`
- **Retries:** 5 attempts
- **Actions:** Verify signature, update payment, write ledger

### 2. Outbound Delivery Worker
- **Interval:** 5 seconds
- **Query:** `SELECT ... WHERE status = 'pending' AND next_attempt_at <= NOW() FOR UPDATE SKIP LOCKED`
- **Retries:** 8 attempts with exponential backoff
- **Actions:** POST to merchant URL with HMAC signature

### 3. Reconciliation Worker
- **Interval:** 5 minutes
- **Query:** `SELECT ... WHERE status = 'processing' AND updated_at < NOW() - 15min`
- **Actions:** Call provider.GetTransaction(), correct local state

**Graceful Shutdown:**
```go
workerCtx, cancelWorkers := context.WithCancel(ctx)
defer cancelWorkers()

go webhookService.StartWorker(workerCtx, 10*time.Second)
go outboundService.Start(workerCtx, 5*time.Second)
go reconcileService.Start(workerCtx, 5*time.Minute, 15*time.Minute)

// On SIGINT/SIGTERM:
cancelWorkers() // Propagates to all workers
srv.Shutdown(30 * time.Second) // Graceful HTTP shutdown
```

**Code Reference:** `cmd/server/main.go:95-98`, `cmd/server/main.go:117-130`

---

## Configuration

**Environment Variables:**

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `ENV` | No | `live` | Environment: `live` or `test` |
| `PORT` | No | `8080` | HTTP server port |
| `DATABASE_URL` | **Yes** | - | PostgreSQL DSN |
| `REDIS_ADDR` | No | `localhost:6379` | Redis address |
| `REDIS_PASSWORD` | No | empty | Redis password |
| `BASE_CALLBACK_URL` | **Yes** | - | Public base URL |
| `OUTBOUND_WEBHOOK_SIGNING_SECRET` | **Yes** | - | HMAC secret for merchant webhooks |
| `STRIPE_SECRET_KEY` | No | empty | Stripe API key |
| `STRIPE_WEBHOOK_SECRET` | No | empty | Stripe webhook signing secret |
| `CHAPA_SECRET_KEY` | No | empty | Chapa API key |
| `TELEBIRR_APP_ID` | No | empty | Telebirr app ID |
| `TELEBIRR_APP_KEY` | No | empty | Telebirr app key |
| `TELEBIRR_PUBLIC_KEY` | No | empty | Telebirr RSA public key |
| `FEE_PERCENT` | No | `0.029` | Gateway fee (2.9% default) |
| `RATE_LIMIT_PER_MERCHANT` | No | `600` | Requests per merchant per minute |

**Code Reference:** `internal/config/config.go`

---

## Observability

### Prometheus Metrics (`/metrics`)

```
# HTTP metrics
gateway_http_requests_total{method, route, status_class}
gateway_http_request_duration_seconds{method, route, status_class}

# Payment metrics
gateway_payments_created_total{provider, currency}
gateway_payments_status_transitions_total{provider, from, to}

# Webhook metrics
gateway_webhooks_processed_total{provider, outcome}

# Outbound metrics
gateway_outbound_webhook_attempts_total{outcome}

# Ledger metrics
gateway_ledger_entries_written_total
```

### Structured Logging (Zerolog)

```json
{
  "level": "info",
  "time": "2024-01-15T10:30:45Z",
  "method": "POST",
  "path": "/payments",
  "status": 201,
  "latency_ms": 145,
  "message": "request"
}

{
  "level": "info",
  "payment_intent_id": 42,
  "amount": 1050,
  "currency": "USD",
  "provider": "stripe",
  "message": "payment intent created"
}
```

**Code Reference:** `internal/metrics/metrics.go`, `internal/middleware/auth.go:18`

---

## Security Summary

### Implemented Protections:

✅ **API Authentication**
- O(1) indexed lookup + bcrypt
- Environment isolation (live/test)
- Active merchant check

✅ **Webhook Security**
- HMAC-SHA256 signature verification
- Timestamp-based replay protection (5min window)
- Constant-time comparison
- Provider event deduplication

✅ **Rate Limiting**
- Per-IP: 200 req/min (global)
- Per-merchant: 600 req/min (Redis-backed)

✅ **Input Validation**
- Struct validation on API inputs
- Parameterized SQL queries (no SQL injection)
- Provider enum validation

✅ **Audit Trail**
- Append-only audit_log table
- Records API key rotation, refunds, sensitive ops

### Not Implemented (Per README):

⚠️ WAF/DDoS protection  
⚠️ Secrets rotation mechanisms  
⚠️ IP allowlisting  
⚠️ PCI-DSS attestation  

---

## Deployment

### Docker

**Multi-stage build:**
```dockerfile
FROM golang:1.24-alpine AS builder
# Build static binary

FROM gcr.io/distroless/static-debian12:nonroot
# Runtime: no shell, no package manager, minimal attack surface
COPY --from=builder /out/server /server
ENTRYPOINT ["/server"]
```

**docker-compose.yaml:**
```yaml
services:
  postgres:
    image: postgres:16-alpine
    healthcheck: pg_isready
  
  redis:
    image: redis:7-alpine
    healthcheck: redis-cli ping
  
  app:
    build: .
    depends_on:
      - postgres
      - redis
    env_file: .env
```

### CI/CD (GitHub Actions)

```yaml
jobs:
  build:
    - gofmt -s -l .
    - go vet ./...
    - staticcheck ./...
    - govulncheck ./...
    - go test -race -count=1 -coverprofile=coverage.out ./...
    - go build -ldflags="-w -s" ./cmd/server
```

**Code Reference:** `Dockerfile`, `docker-compose.yaml`, `.github/workflows/ci.yml`

---

## Scalability Considerations

### Horizontal Scaling:
- ✅ Stateless design (all state in DB/Redis)
- ✅ Row-level locking for worker coordination
- ✅ No in-memory session storage
- ✅ Connection pooling (pgx, Redis)

### Bottlenecks:
- ⚠️ PostgreSQL write throughput (single primary)
- ⚠️ Redis single-threaded nature
- ⚠️ Provider API rate limits (external)

### Future Improvements:
- Read replicas for queries
- Database sharding by merchant_id
- Redis clustering
- Auto-scaling pods (K8s HPA)

---

## Testing

### Unit Tests:
- Ledger balance validation
- API error handling
- Provider abstraction
- Idempotency logic

### Race Detection:
```bash
go test -race -count=1 ./...
```

### Coverage:
```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

**Not Implemented:**
- Integration tests with real providers
- Load/stress testing
- Chaos engineering

**Code Reference:** `*_test.go` files

---

## Project Status: Beta

**What's Implemented:**
- ✅ Core payment flow (create, process, succeed, fail, refund)
- ✅ Double-entry ledger with validation
- ✅ Multi-layer idempotency
- ✅ Webhook security (HMAC-SHA256)
- ✅ Background workers with concurrency
- ✅ Provider abstraction (Stripe, Chapa, Telebirr)
- ✅ Reconciliation engine
- ✅ Audit logging
- ✅ Prometheus metrics
- ✅ CI pipeline

**Not Production-Ready (Per README):**
- ❌ PCI-DSS attestation
- ❌ KYB/sanctions screening
- ❌ Secrets management (Vault/AWS Secrets Manager)
- ❌ WAF/DDoS protection
- ❌ External penetration test
- ❌ Legal entity + processor agreements
- ❌ Merchant dashboard UI

**Before Processing Live Funds:**
- Complete compliance checklist in README
- External security audit
- Load testing + capacity planning
- Infrastructure hardening (secrets, WAF, monitoring)

---

## Further Reading

- **Portfolio Case Study:** `docs/portfolio/upwork-case-study.md`
- **Verified Claims:** `docs/portfolio/verified-claims.md`
- **Portfolio Strategy:** `docs/portfolio/portfolio-recommendation.md`
- **Diagrams:** `docs/diagrams/*.mmd`
