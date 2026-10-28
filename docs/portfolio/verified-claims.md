# Payment Gateway — Verified Claims

This document separates provable implementation details from aspirational features.

---

## ✅ VERIFIED — Directly Supported by Code

### Architecture & Design Patterns

- **Provider Strategy Pattern** — Interface-based abstraction (`internal/provider/interface.go`) with Registry for dependency inversion
- **Inbox Pattern for Webhooks** — Two-phase: store immediately, process async (`internal/webhook/service.go`)
- **Background Workers with Row-Level Locking** — `SELECT FOR UPDATE SKIP LOCKED` in webhook, outbound, and reconciliation workers
- **Graceful Shutdown** — Context cancellation cascades to all workers (`cmd/server/main.go`)
- **Repository Pattern** — Service layer abstracts database access

### Financial Integrity

- **True Double-Entry Ledger** — Every payment creates balanced debit/credit entries with `SUM(amount) = 0` validation (`internal/ledger/service.go:271`)
- **Atomic Transactions** — Payment status updates and ledger inserts happen in single PostgreSQL transaction (`internal/webhook/service.go:markSucceeded`)
- **Append-Only Ledger** — PostgreSQL trigger blocks UPDATE/DELETE on `ledger_entries` (`migrations/001_init.up.sql`)
- **UNIQUE Constraint Idempotency** — `UNIQUE(payment_intent_id, account)` prevents duplicate ledger writes (`migrations/002_webhook_idempotency.up.sql`)

### Idempotency Protection

- **Redis Idempotency Cache** — 24-hour TTL cache of payment responses (`internal/db/idempotency.go`)
- **Database UNIQUE Constraint** — `UNIQUE(merchant_id, idempotency_key)` on payment_intents (authoritative)
- **Webhook Event Deduplication** — `UNIQUE(provider, provider_event_id)` with `INSERT ... ON CONFLICT DO NOTHING` (`migrations/002_webhook_idempotency.up.sql`)
- **Idempotent Refunds** — Separate idempotency key per refund request

### Security

- **HMAC-SHA256 Webhook Verification (Stripe)** — Constant-time signature comparison with 5-minute timestamp window (`internal/provider/stripe/stripe.go:93-134`)
- **O(1) API Key Authentication** — Indexed lookup + single bcrypt compare (`internal/merchant/service.go:82-105`)
- **API Key Format** — `gw_{env}_{lookup_16hex}_{secret_48hex}` with bcrypt storage of secret half only
- **Constant-Time Comparison** — `hmac.Equal()` used for signature verification
- **Replay Protection** — Timestamp validation in webhook signatures (5-minute skew tolerance)
- **Rate Limiting** — Per-IP (200 req/min) and per-merchant (600 req/min configurable) using Redis (`cmd/server/main.go:88`, `internal/middleware/ratelimit.go`)

### Webhook Processing

- **Signature Verification Before Processing** — All providers verify signatures via `Provider.VerifyWebhook()`
- **Async Processing with Retry** — Workers retry failed webhooks up to 5 attempts with exponential backoff
- **Dead Letter Handling** — Failed webhooks after 5 attempts marked as `status: failed`
- **Transactional Processing** — Webhook status flip + payment update + ledger insert in single transaction

### Outbound Webhooks (Merchant-Facing)

- **HMAC-SHA256 Signed Delivery** — `X-Gateway-Signature: t=<unix>,v1=<hmac>` header (`internal/outbound/service.go:130`)
- **Exponential Backoff** — Base 30s, multiplier 2.0, max 6h, 8 attempts before dead letter (`internal/outbound/service.go:27-30`)
- **Idempotency-Key Header** — `Idempotency-Key: dlv_{delivery_id}` for safe retries
- **FOR UPDATE SKIP LOCKED** — Parallel workers without double-processing

### Reconciliation

- **Stale Payment Detection** — Finds payments in `processing` for >15 minutes (`internal/reconcile/reconcile.go`)
- **Provider State Synchronization** — Calls `provider.GetTransaction()` to fetch authoritative state
- **Automatic Correction** — Updates local status + writes ledger if provider says succeeded
- **Background Job** — Runs every 5 minutes

### Multi-Provider Support

- **Stripe** — HMAC-SHA256 webhooks, PaymentIntents API, Refunds API (`internal/provider/stripe/`)
- **Chapa** — Custom signature verification, charge + verify APIs (`internal/provider/chapa/`)
- **Telebirr** — RSA signature verification, H5 payment API, query API (`internal/provider/telebirr/`)
- **Unified Interface** — Business logic never knows which provider is active

### Refunds

- **Full and Partial Refunds** — Supported via `POST /payments/{id}/refunds` (`internal/payment/refund_service.go`)
- **Idempotent Refunds** — UNIQUE constraint on `(merchant_id, idempotency_key)`
- **Inverse Ledger Entries** — Refunds create DEBIT merchant + CREDIT provider entries
- **Provider Integration** — Calls `provider.Refund()` for Stripe (Chapa/Telebirr support varies)

### Observability

- **Prometheus Metrics** — HTTP latency, payment status transitions, webhook outcomes, ledger writes (`internal/metrics/metrics.go`)
- **Structured Logging** — Zerolog with JSON output (optional pretty console for dev)
- **Audit Log** — Append-only table for sensitive operations (API key rotation, refunds) (`internal/audit/audit.go`)
- **Health Check** — `GET /health` returns JSON status
- **Metrics Endpoint** — `GET /metrics` for Prometheus scraping

### Testing

- **Unit Tests** — Ledger balance validation, API error handling, webhook idempotency (`*_test.go` files)
- **Race Detector** — Tests run with `-race` flag in CI (`.github/workflows/ci.yml`)
- **Table-Driven Tests** — Multiple providers tested via same test suite (`internal/provider/*/`)

### Deployment

- **Dockerized** — Multi-stage build: golang:1.24-alpine builder + distroless runtime (`Dockerfile`)
- **Docker Compose** — Local dev environment with Postgres + Redis + app (`docker-compose.yaml`)
- **Migrations** — golang-migrate applied automatically on boot (`internal/db/migrate.go`)
- **Stateless Design** — All state in Postgres/Redis, horizontally scalable
- **CI Pipeline** — GitHub Actions: gofmt, vet, staticcheck, govulncheck, test, build

### API Design

- **RESTful Endpoints** — Standard HTTP methods (POST, GET, PUT, DELETE)
- **Structured Error Codes** — Stable codes: `invalid_request`, `unauthorized`, `idempotency_conflict`, `rate_limit_exceeded`, `provider_error`
- **Consistent Response Envelope** — `{"success":bool, "data":{}, "error_obj":{}}`
- **Middleware Stack** — Recoverer, logger, metrics, timeout, rate limit, auth

---

## ⚠️ PARTIALLY VERIFIED — Implemented but Incomplete

### Retry Logic

- **Webhook Worker Retry** — Exists (`attempts < 5`) but uses simple counter, not true exponential backoff with delay
- **Exponential Backoff (Outbound)** — Fully implemented for outbound webhooks (`internal/outbound/service.go:152`)
- **Retry Package** — Generic retry utility exists (`internal/retry/retry.go`) but not used in webhook worker

### Provider Coverage

- **Stripe Refunds** — Fully implemented (`internal/provider/stripe/stripe.go:Refund`)
- **Chapa Refunds** — Provider interface implemented but refund method may not be fully tested
- **Telebirr Refunds** — Not implemented (Telebirr historically lacks refund API)

### Reconciliation

- **Implemented** — Worker runs every 5 minutes, calls `GetTransaction()`, corrects state
- **Batch Size** — Limited to 100 per run (configurable but not auto-scaling)
- **Provider Coverage** — GetTransaction() implemented for all providers but real-world testing unclear

---

## ❌ NOT VERIFIED — Claims Not Supported by Code

### Production Readiness

- **PCI-DSS Compliance** — README explicitly states "not in scope" and lists compliance checklist
- **KYB/Sanctions Screening** — README states "not in scope"
- **Secrets Management** — Uses `.env` file, not AWS Secrets Manager/Vault (README mentions this as TODO)
- **WAF/DDoS Protection** — Not implemented (README mentions as TODO)
- **External Penetration Test** — No evidence
- **Legal Entity / Processor Agreements** — Explicitly out of scope per README

### Scale Claims

- **"Millions of Transactions"** — No load testing evidence, no benchmarks
- **"Production Traffic"** — README states "beta" status
- **"High Availability"** — Single-instance deployment in docker-compose, K8s manifests not present

### Advanced Features

- **Kubernetes Manifests** — Not present in codebase
- **Auto-Scaling Configuration** — Not present
- **Blue/Green Deployment** — Not implemented
- **Multi-Region** — Single-region design
- **Circuit Breaker** — Not implemented (no Hystrix/resilience4j equivalent)
- **Distributed Tracing** — No OpenTelemetry/Jaeger integration

### Merchant Features

- **Merchant Dashboard UI** — Explicitly "not in scope" per README
- **Merchant Self-Service Onboarding** — Manual merchant creation only
- **Webhook Testing Tools** — Not provided
- **Sandbox/Test Mode** — Environment separation exists (`live` vs `test`) but no separate sandbox infrastructure

### Advanced Security

- **Webhook Signature Rotation** — No mechanism for rotating signing secrets without downtime
- **API Key Rotation** — Implemented (`RegenerateKey`) but no notification to merchant
- **IP Allowlisting** — Not implemented
- **OAuth 2.0** — Not implemented (uses custom API keys)
- **2FA for Merchant Access** — Not applicable (no dashboard)

---

## 🔍 CANNOT VERIFY — Insufficient Information

### Real-World Usage

- **Active Merchants** — No evidence of production usage
- **Transaction Volume** — No statistics available
- **Uptime** — No SLA or uptime tracking
- **Error Rates** — Metrics exist but no baseline or targets

### Performance

- **Throughput** — No benchmarks
- **Latency P99** — No performance testing results
- **Concurrent Connection Limits** — Default Go settings, not tuned

### Provider Behavior

- **Chapa Webhook Signature Algorithm** — Code exists but verification unclear without Chapa docs
- **Telebirr RSA Verification** — Implementation present but real-world behavior untested
- **Provider Downtime Handling** — Reconciler exists but resilience under prolonged outage untested

---

## Summary

**Strong Claims (Use These):**
- Double-entry ledger with ACID transactions
- HMAC-SHA256 webhook security (Stripe verified)
- Idempotency at multiple layers (Redis + DB constraints)
- Provider abstraction via Strategy Pattern
- Background workers with row-level locking
- Exponential backoff for outbound webhooks
- Atomic financial transactions
- Append-only audit trail

**Avoid Claiming:**
- Production-ready without qualification
- PCI-DSS compliant
- High-scale proven
- Kubernetes-native
- Full KYB/compliance

**Qualified Claims (Use With Context):**
- "Beta status with core money flow implemented"
- "Designed for horizontal scaling (stateless)"
- "Multi-provider support (Stripe, Chapa, Telebirr)"
- "Implements financial best practices (double-entry, idempotency)"
