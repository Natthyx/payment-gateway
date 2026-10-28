# Payment Gateway

A multi-provider payment orchestration service. Merchants integrate once;
the gateway routes charges through Stripe, Chapa, or Telebirr, runs a
double-entry ledger, delivers signed webhook events back to merchants,
reconciles with each provider, and exposes Prometheus metrics.

> **Status — beta.** Core money flow, refunds, reconciliation, and outbound
> webhooks are implemented and unit-tested. Before going live with real
> funds you must complete the **Compliance & legal checklist** at the end
> of this README. None of that is solvable in code.

---

## Features

| Capability                          | Status |
| ----------------------------------- | ------ |
| Provider abstraction (Stripe / Chapa / Telebirr) | done |
| Idempotent payment intent creation  | done |
| Provider-event webhook idempotency  | done |
| Double-entry ledger (append-only)   | done |
| Refunds (full and partial)          | done |
| Reconciliation worker               | done |
| Outbound (merchant-facing) signed webhooks with retry | done |
| Per-merchant + per-IP rate limiting | done |
| Audit log (append-only)             | done |
| Prometheus `/metrics`               | done |
| Structured error codes              | done |
| O(1) API key authentication         | done |
| golang-migrate migrations           | done |
| Unit + handler tests with race detector | done |
| OpenAPI 3 spec                      | done |
| CI (vet, test, govulncheck, build)  | done |
| Merchant dashboard UI               | not in scope |
| KYB / sanctions screening           | not in scope (see checklist) |
| PCI-DSS attestation                 | not in scope (see checklist) |

---

## Quick start

```bash
cp .env.example .env
# Edit .env: set BASE_CALLBACK_URL, STRIPE_*, OUTBOUND_WEBHOOK_SIGNING_SECRET
docker compose up -d postgres redis
go run ./cmd/server
```

Migrations apply automatically on boot.

### Create a merchant

```bash
curl -s -X POST http://localhost:8080/merchants \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme","email":"ops@acme.com"}'
```

The response contains an `api_key` of the form `gw_test_<lookup>_<secret>`.
**This is the only time the key is shown.** Store it.

### Create a payment intent

```bash
curl -s -X POST http://localhost:8080/payments \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Idempotency-Key: order_42' \
  -H 'Content-Type: application/json' \
  -d '{"amount":1050,"currency":"USD","provider":"stripe"}'
```

### Refund

```bash
curl -s -X POST http://localhost:8080/payments/123/refunds \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Idempotency-Key: refund_for_order_42' \
  -d '{"amount":1050,"reason":"customer request"}'
```

### Register a webhook endpoint

```bash
curl -s -X POST http://localhost:8080/webhook-endpoints \
  -H "Authorization: Bearer $API_KEY" \
  -d '{"url":"https://merchant.example.com/hooks/payments"}'
```

Every event we send carries `X-Gateway-Signature: t=<unix>,v1=<hex(HMAC-SHA256)>`.
Verify in your handler before trusting the body.

---

## Architecture

```
┌────────┐   POST /payments      ┌──────────────┐   provider HTTP    ┌─────────┐
│ client │ ───────────────────▶ │  payment.Svc │ ─────────────────▶ │ Stripe  │
└────────┘                       └──────────────┘                    └─────────┘
                                        │
                                  Postgres tx
                                        │
                                        ▼
                       ┌────────────────────────────────┐
                       │ payment_intents (status: ...)  │
                       │ ledger_entries  (append-only)  │
                       └────────────────────────────────┘
                                        ▲
                                        │ FOR UPDATE SKIP LOCKED
                                        │
┌──────────┐  POST /webhooks/*  ┌────────────────┐
│ provider │ ─────────────────▶ │ webhooks inbox │
└──────────┘                     └────────────────┘
                                        │
                                  webhook.Worker
                                        │
                                        ▼
                              ledger.RecordPaymentTx
                              outbound.Enqueue
                                        │
                                        ▼
                              outbound.DeliveryWorker
                                        │
                                        ▼
                        ┌────────────────────────────┐
                        │ merchant_webhook_endpoint  │
                        │  (signed, retried, dead-   │
                        │   lettered after 8 fails)  │
                        └────────────────────────────┘
```

Two background workers run in-process:

1. **Webhook inbox worker** — drains `webhooks` rows with `FOR UPDATE SKIP LOCKED`,
   verifies the signature, updates the payment intent, and writes ledger
   entries all in one transaction.
2. **Outbound delivery worker** — drains `outbound_webhook_deliveries` rows
   the same way, POSTs to merchant URLs with HMAC signature + Idempotency-Key,
   exponential backoff up to 6h, dead-letters at 8 attempts.
3. **Reconciler** — every 5 minutes, picks payment intents that have been
   `processing` for >15 minutes, calls `provider.GetTransaction`, and
   corrects local state if the provider says it's already settled.

Multiple instances of the binary can run safely behind a load balancer;
all critical state lives in Postgres / Redis and locking is row-level.

---

## Environment variables

See `.env.example`. The required ones with no default are:

| Variable                          | Purpose                              |
| --------------------------------- | ------------------------------------ |
| `DATABASE_URL`                    | Postgres DSN                         |
| `BASE_CALLBACK_URL`               | Public base URL of this service      |
| `OUTBOUND_WEBHOOK_SIGNING_SECRET` | HMAC key for outbound webhooks       |

Everything else has a documented default.

---

## API responses

```json
{
  "success": true,
  "data": { ... },
  "meta":  { "total": 142, "page": 1, "limit": 20 }
}
```

```json
{
  "success": false,
  "error_obj": {
    "code": "idempotency_conflict",
    "message": "idempotency_key already used"
  }
}
```

Stable codes: `invalid_request`, `unauthorized`, `forbidden`, `not_found`,
`conflict`, `idempotency_conflict`, `rate_limit_exceeded`, `provider_error`,
`internal_error`.

---

## Compliance & legal checklist (outside this codebase)

These items are **not** solvable in code. You must complete them before
processing real money for third-party merchants.

- [ ] Form a legal entity in your operating jurisdiction.
- [ ] Sign processor / acquirer agreements (Stripe Connect Custom for
      re-sellers, Chapa partner agreement, Telebirr merchant agreement).
- [ ] PCI-DSS scope analysis: SAQ A or A-EP if redirect-only.
      Engage a QSA. Get an Attestation of Compliance.
- [ ] KYB onboarding flow: integrate a vendor (Persona, Sumsub, Veriff…).
- [ ] Sanctions / PEP screening (OFAC, EU consolidated, UN).
- [ ] Money transmitter / electronic money licence as required
      (FinCEN MSB in the US, NBE directive in Ethiopia, etc).
- [ ] Terms of Service, AUP, Privacy Policy, DPA — drafted by counsel.
- [ ] Incident response + breach notification policy (72-hour clock).
- [ ] External pen-test before launch.
- [ ] Move secrets out of `.env` into AWS Secrets Manager / Vault.
- [ ] Configure WAF + DDoS protection in front of `/webhooks`.
- [ ] Provision separate `live` and `test` API key envs (already supported
      via `ENV=live|test`).

---

## Development

```bash
make test       # unit tests
make test-race  # race detector
make vet
make lint       # staticcheck
make vuln       # govulncheck
make build      # static binary in bin/server
```

---

## License

Proprietary. All rights reserved.
