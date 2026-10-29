# FINDINGS — payment-gateway load test

Every claim in §1 was reproduced on the original code (raw output under `results/phase0/`). The §2 risks were
found by reading code; §0 says how each was later demonstrated or verified. File and line references are against
the ORIGINAL commit `4d69316` plus the mock-provider wiring (`cmd/server/main.go`,
`internal/config/config.go`, `internal/provider/mock/`).

All runs: local, single machine (Apple M2, 8 GB; Docker Desktop VM 8 CPUs), `PROVIDER_MODE=mock`.

> **Status: all bugs and risks below are fixed on branch `loadtest/benchmark`, at the owner's
> request.** Sections 1–4 describe the **original** code (commit `4d69316`) with the evidence
> captured before any fix. Section 0 maps each finding to its fix and to the evidence that the
> fixed build behaves correctly. The measured results in `REPORT.md` are for the fixed build.

---

## 0. Fix status

| Finding | Fixed in | Evidence on the fixed build |
|---|---|---|
| B1 Stripe/Chapa signatures never verify | `076f8e1` | `results/phase0/after_fix/b1_b5_stripe_webhook.txt`; G4/G8 (every webhook processed) |
| B2 GET endpoints 500 | `87e0367` | `results/phase0/current/b2_get_endpoints.txt` |
| B3 audit rows lost | `87e0367` | `results/phase0/current/b3_audit_ip.txt` |
| B4 every refund 500 | `e7be08b` | `results/phase0/after_fix/refund_probe.txt`; G5 |
| B5 inbox retries without delay | `87e0367` (migration 008) | `results/fixes/b5_retry_backoff.txt` |
| R1 over-refund | `87e0367` | before: `results/prefix_e7be08b/g5/run*/invariants.txt` (2 of 4 payments over-refunded); after: `results/g5/run*/invariants.txt` (0) |
| R2 equal-amount refunds collide in the ledger | `87e0367` | G5 sub-test b: before 19 × 500 with rows left `pending` (`results/prefix_e7be08b/g5/`), after 0 × 500 |
| R3 forged body claims a real event id | `87e0367` | `results/fixes/r3_forged_event_id.txt` |
| R4 replay window measured at processing time | `87e0367` | `results/fixes/r4_backlog.txt`; unit test `internal/provider/stripe/receipt_test.go` |
| R5 cross-merchant intent match | `87e0367` | `results/fixes/r5_cross_merchant.txt` |
| R6 different body replays the original | `87e0367` (migration 009) | G2: before 500/500 replayed (`results/prefix_035b9ea/g2/`), after 500/500 rejected (`results/g2/`) |
| R7 provider failure burns key / ignored update error | `87e0367` | code: `internal/payment/service.go` (`replayExisting`); not load-tested |
| R8 per-IP limit bypass via `X-Real-IP` | `87e0367`; hardened later to `TRUSTED_PROXIES` (CI fix commit, not re-measured) | G9 part C: before 300/300 allowed (`results/prefix_035b9ea/g9/`), after 27/300 |
| R9 bcrypt on every request; fixed window | `87e0367`, `633fe7b` | G3 throughput; G9 part A |
| R10 empty webhook secret accepted | `87e0367` | unit test `TestVerifyWebhookRejectsEmptySecret` |
| R11 HTTP call inside DB tx; missing events | `87e0367` | G6 (lease-based worker); G10 (reconciler now enqueues) |
| R12 webhook amount not checked | `87e0367` | `results/fixes/r12_amount_mismatch.txt` |
| R13 merchant key sent to provider (new, see §2) | `87e0367` | code only: the mock does not model provider-side idempotency |
| Append-only bypass (TRUNCATE, owner/superuser app role) | `557b272` (migration 010, runtime role) | G7: `results/g7/append_only_as_*.txt` |
| `LOG_LEVEL` ignored | `87e0367` | `results/environment_raw.txt` (LOG_LEVEL=warn honoured) |
| CI `gofmt` cannot pass | `51b1487` | `gofmt -s -l .` prints nothing |
| Auth-cache stampede (found while fixing R9) | `633fe7b` | G3; pre-fix baseline in `results/prefix_035b9ea/g3/` |

**Still true after the fixes (not fixable in code, or by design):**
- A **superuser** (the `gateway` role in docker-compose) can still disable triggers or use
  `session_replication_role`. The fix keeps the app off that role; protecting against a DBA
  needs an external audit trail.
- A rotated API key keeps working on *other* instances for up to `AUTH_CACHE_TTL` (30 s).
- An intent left in `created` (crash between insert and provider call) makes its key answer
  `409 idempotency_key_in_use` until someone resolves it; nothing sweeps such rows.
- Outbound delivery is at-least-once (lease expiry after a crash can resend). Merchants must
  dedupe on `Idempotency-Key`.
- Existing dev Postgres volumes have no `gateway_runtime` role: recreate the volume or run
  `deploy/postgres/initdb/01-runtime-role.sh` by hand.
- `docs/portfolio/verified-claims.md` was not edited. Its inaccuracies are listed in §4.

---

## 1. Bugs found (reproduced)

### B1 — Stripe and Chapa webhooks can never pass signature verification · Reproduced
- **Evidence:** `results/phase0/b1_b5_stripe_webhook.txt`
- **Cause:** the inbox stores the raw body in a `JSONB` column (`migrations/001_init.up.sql:67`).
  Postgres normalises JSONB: it reorders keys and adds whitespace. The worker reads that
  normalised text back (`internal/webhook/service.go:108-118`) and verifies the HMAC over it
  (`internal/webhook/service.go:191`). Stripe (`internal/provider/stripe/stripe.go:141-147`) and Chapa
  (`internal/provider/chapa/chapa.go:91-98`) sign the exact raw bytes, so the HMAC never matches.
  Telebirr signs parsed fields (`internal/provider/telebirr/telebirr.go:121-140`), so it survives the
  normalisation; a Telebirr webhook was processed successfully in the same setup.
- **Observed:** HMAC over the bytes sent equals the `v1` signature. HMAC over the stored JSONB text
  does not. The webhook went `failed` after 5 attempts and the intent stayed `processing`.
- **Impact:** with real Stripe or Chapa, no payment can ever reach `succeeded` through a webhook.
  No ledger entries are written and no merchant webhook is sent. Only the reconciler
  (15-minute staleness, `cmd/server/main.go:133`) could move such a payment forward.
- **Repro:** `loadtest/probes/phase0_probes.sh` (section B1). Manual version: create an intent; POST
  `/webhooks/stripe` with body `{"id":"evt_1","type":"payment_intent.succeeded",...}` and
  `Stripe-Signature: t=<now>,v1=HMAC(secret, "<t>.<body>")`; within 10 s
  `SELECT status, attempts, last_error FROM webhooks` shows `failed | 5 | signature verification failed`.

### B2 — `GET /payments`, `GET /payments/{id}` and `GET /payments/{id}/ledger` return 500 for ordinary intents · Reproduced
- **Evidence:** `results/phase0/b2_get_endpoints.txt`
- **Cause:** `failure_reason` and `provider_tx_id` are nullable `TEXT` columns, but they are scanned into
  Go `string` fields (`internal/payment/service.go:173-183` and `:205-227`, `internal/models/models.go:54-55`). pgx refuses
  to scan NULL into `string`. `failure_reason` is NULL for every intent that has not failed.
  `/ledger` calls `GetIntent` first (`internal/payment/handler.go:109`), so it fails the same way.
- **Impact:** merchants cannot read a payment or its ledger. The OpenAPI-documented read API is effectively down.

### B3 — Audit rows for `merchant.create` / `api_key.rotate` are silently lost unless a proxy header is present · Reproduced
- **Evidence:** `results/phase0/b3_audit_ip.txt`
- **Cause:** `internal/merchant/handler.go:53,86` passes `r.RemoteAddr` as the IP. Without
  `X-Real-IP`/`X-Forwarded-For`, chi's `RealIP` leaves it as `ip:port`. Inserting that into the
  `INET` column (`migrations/004_audit_log.up.sql:11`) fails. `audit.Record` only logs the error
  (`internal/audit/audit.go:56-61`), and the request still returns 201.
- **Impact:** the audit trail has gaps for exactly the security-relevant actions it exists for.

### B4 — Every refund request returns 500; the provider is never called; a `pending` refund row is left behind · Reproduced
- **Evidence:** `results/phase0/refund_probe.txt`
- **Cause:** `INSERT ... RETURNING ... provider_refund_id, failure_reason` (`internal/payment/refund_service.go:91-101`)
  scans NULL columns into `string` fields (`internal/models/models.go:177-178`). The INSERT commits (autocommit) and then the
  scan fails, so the handler returns 500 before the provider call. A retry with the same key hits
  `ON CONFLICT DO UPDATE` and returns the same NULL row, so it also gets 500.
- **Note:** to reach the refund code, the probe moved the payment to `succeeded` with SQL. B1 makes the
  webhook path unusable for Stripe, and Stripe is the only provider whose client supports refunds.
- **Impact:** refunds do not work at all. **G5 cannot exercise the concurrency logic it is meant to test.**

### B5 — Inbox "retries" happen in a tight loop with no delay · Reproduced
- **Evidence:** `results/phase0/b1_b5_stripe_webhook.txt` (all 5 attempts are logged within one second).
- **Cause:** a failed row stays `pending` with `attempts < 5` (`internal/webhook/service.go:111,141-147`). The drain loop
  (`:282-293`) calls `ProcessOnce` again immediately and picks the same row.
- **Impact:** a transient error (DB blip, deploy) uses up all 5 attempts in milliseconds and the event
  becomes `failed` for good. `docs/portfolio/verified-claims.md` describes this as
  "Async Processing with Retry ... up to 5 attempts".

---

## 2. Risks found by reading code

Found by reading the original code. The scenario that later tested each risk is in brackets; §0 has the outcome.

| # | Risk | Where |
|---|------|-------|
| R1 | **Over-refund is possible.** No cumulative check, no row lock: each refund is only checked against the full payment amount. Several partial refunds (concurrent or even sequential) can exceed the payment. Unreachable today because of B4. [G5] | `internal/payment/refund_service.go:54-60` (no `FOR UPDATE`), `:76-78` |
| R2 | The refund ledger account suffix is `:refund:<amount>`, so two refunds of the **same amount** on one payment collide on `UNIQUE(payment_intent_id, account)`. The second refund fails **after** the provider has already refunded the money; the refund row is left `pending` and the ledger has no entry. [G5] | `internal/ledger/service.go:162`, `internal/payment/refund_service.go:112-150` |
| R3 | Dedupe runs **before** signature verification. A forged body carrying a real future event id is stored first; the genuine event is then swallowed as a duplicate, and the forged one fails verification. The payment never succeeds. | `internal/webhook/handler.go:51-66`, `internal/webhook/service.go:65-81` |
| R4 | The Stripe 5-minute timestamp window is checked at **processing** time, not at receipt. If the backlog is over 5 minutes, or a webhook arrives older than about 4m50s, a genuine webhook fails verification. | `internal/webhook/service.go:191`, `internal/provider/stripe/stripe.go:133-139` |
| R5 | `markSucceeded` / the failed-event handler match `provider_tx_id = $1 OR idempotency_key = $1` **across all merchants**. An intent whose merchant-chosen idempotency key equals another payment's provider tx id is also flipped to `succeeded`. Only the first `RETURNING` row gets ledger entries. The `OR` also prevents a pure index lookup. | `internal/webhook/service.go:214,245` |
| R6 | Idempotency: the same key with a **different body** replays the original 201 (there is no request fingerprint) instead of returning 409. Concurrent identical requests get a mix of 201 and `409 idempotency_conflict`; the client cannot tell "in progress, retry" from "key misused". [G2] | `internal/payment/service.go:68-82,110-113` |
| R7 | A provider error marks the intent `failed` and the key stays used: every retry with the same key gets 409, forever. The error from `UPDATE ... status='processing'` is ignored. | `internal/payment/service.go:126-139` |
| R8 | The per-IP limiter (200/min) is hard-coded and in-memory per instance. Because `RealIP` runs before it and trusts client-supplied `X-Real-IP`/`X-Forwarded-For`, any client can bypass it. [G9] | `cmd/server/main.go:105-107` |
| R9 | Every authenticated request does a bcrypt cost-12 compare, **before** the per-merchant limiter. Rate-limited requests still cost a bcrypt, so the limiter does not protect CPU. Its window is a fixed calendar minute, so up to 2× the limit can pass across a boundary. [G3, G9] | `internal/merchant/service.go:35,122`; `cmd/server/main.go:123-124`; `internal/middleware/ratelimit.go:40` |
| R10 | `STRIPE_WEBHOOK_SECRET`, `CHAPA_SECRET_KEY` and `TELEBIRR_APP_KEY` are optional. If one is empty, the HMAC/hash key is empty and anyone can forge that provider's webhooks. | `internal/config/config.go:60-66` |
| R11 | The outbound worker makes the merchant HTTP call **inside** the DB transaction that holds the row lock, tying up a pooled connection for up to 10 s per call. Only `payment.succeeded` from the webhook worker is ever enqueued: the reconciler and refunds emit nothing. | `internal/outbound/service.go:97-167`, `internal/webhook/service.go:171-175`, `internal/reconcile/reconcile.go:121-128` |
| R12 | The webhook amount and currency are never compared with the intent. | `internal/webhook/service.go:233-268` |
| R13 | The merchant's idempotency key is sent to the provider as-is: as Stripe's `Idempotency-Key`, as Chapa's `tx_ref`, and as Telebirr's `outTradeNo`. Keys are only unique per merchant, so two merchants using `order_1` would collide at the provider. Stripe would return the first merchant's PaymentIntent to the second. Found while fixing R5. | `internal/payment/service.go:119-125`, `internal/provider/chapa/chapa.go`, `internal/provider/telebirr/telebirr.go` |

---

## 3. Phase 0 answers

### Where is idempotency enforced?
**Both Redis and Postgres.** The Postgres constraint is the authority; Redis is a replay cache.
- Redis: key `idempotency:<merchant_id>:<key>`, TTL 24 h (`internal/db/idempotency.go:12,41-75`). It is read first
  (`internal/payment/service.go:68-82`) and written only **after** a successful provider call (`:150-155`).
- Postgres: `UNIQUE (merchant_id, idempotency_key)` (`migrations/001_init.up.sql:33`). A violation returns
  `409 idempotency_conflict` (`internal/payment/service.go:110-113`).
- **Same key, same body:** if the first request has finished, the cached response is replayed (201, same intent).
  If it is still in flight, the request gets 409.
- **Same key, different body:** if the first request has finished, the **original** response is replayed (201) and
  the new body is ignored. If it is still in flight, or the Redis entry is gone, the request gets 409. (R6)
- **Per merchant:** yes, in both Redis and Postgres. The `Idempotency-Key` header overrides the body field (`internal/payment/handler.go:48-50`).

### Provider interface and the `POST /payments` happy path
- The interface is at `internal/provider/interface.go:96-117`. Refunds are an optional `Refunder` capability (`:79-81`) that only
  Stripe implements. The registry is at `:125-145`. The provider is chosen by the request's `provider` field, checked
  against a provider-to-currency map (`internal/payment/validate.go:15-19,45-54`), then looked up with `registry.Get`
  (`internal/payment/service.go:86`).
- Happy path (`internal/payment/service.go:59-167`):
  1. Redis GET.
  2. `INSERT payment_intents` with status `'created'`.
  3. `CreateCharge` (no DB transaction is held).
  4. `UPDATE` status `'processing'`, `provider_tx_id`.
  5. Redis SET.
  6. Respond 201 with the intent and `checkout_url`.
- **Tables written:** only `payment_intents` (plus the Redis key). There are no ledger, audit or outbox writes.
- **State of a new intent:** `created` until the provider returns, `processing` after. The response shows `processing`.

### Inbound webhooks: verification and duplicate prevention
- **Receive** (`internal/webhook/handler.go:36-91`):
  1. Read the body (1 MB cap).
  2. Take the provider's signature header.
  3. Extract the event id **without verifying** it (`:51`); if there is none, fall back to `sha256(body)` (`:55-58`).
  4. `INSERT ... ON CONFLICT (provider, provider_event_id) DO NOTHING` (`internal/webhook/service.go:65-81`).
  5. Always return 200.
- **Process:** one goroutine per instance on a 10 s ticker (`cmd/server/main.go:131`).
  1. Claim one row with `FOR UPDATE SKIP LOCKED` (`internal/webhook/service.go:108-118`).
  2. Increment `attempts`.
  3. Verify the signature **now** (`:191`).
  4. `markSucceeded` (`:233-272`): the status update and the ledger insert happen in one transaction.
  5. Commit.
  6. Enqueue the outbound webhook after the commit (`:171-175`).
- **Stripe check** (`internal/provider/stripe/stripe.go:108-147`): parse `t=` and `v1=`; reject if |now − t| > 5 min;
  compute `HMAC-SHA256(secret, t + "." + payload)`; compare with `hmac.Equal`. This works on raw bytes but breaks
  on JSONB text (B1).
- **Duplicate delivery:** three layers.
  1. Unique `(provider, provider_event_id)` (`migrations/002_webhook_idempotency.up.sql:14-16`).
  2. The status gate `status NOT IN ('succeeded','refunded')` (`internal/webhook/service.go:245-246`).
  3. `UNIQUE(payment_intent_id, account)` on the ledger (`migrations/002_webhook_idempotency.up.sql:21-22`).

### Ledger: schema, writes, append-only
- **Schema:** `migrations/001_init.up.sql:44-57`. `amount` is signed (debit < 0).
- **`RecordPaymentTx`** (`internal/ledger/service.go:63-127`) writes three legs inside the caller's transaction:
  - provider −amount
  - merchant +(amount − fee)
  - `gateway:fees` +fee

  The fee is truncated toward zero (`:65`). `validateBalance` checks the legs sum to zero before the insert (`:271-283`).
  A unique violation becomes `ErrLedgerAlreadyRecorded`.
- **How append-only is enforced:** `BEFORE UPDATE` / `BEFORE DELETE` **row-level triggers** that raise an exception
  (`migrations/001_init.up.sql:81-94`, `migrations/004_audit_log.up.sql:21-36`). There is **no `REVOKE`** anywhere.
- **DB role:** the app connects as `gateway`, which is the image's `POSTGRES_USER`. That role is a **superuser** and
  **owns** both tables, so "app role" and "table owner" are the same role.
- **Attempts** (`results/phase0/append_only_probe.txt`; each attempt was rolled back):

| Attempt as `gateway` (app role = owner = superuser) | `ledger_entries` | `audit_log` |
|---|---|---|
| `UPDATE` | blocked: trigger exception | blocked: trigger exception |
| `DELETE` | blocked: trigger exception | blocked: trigger exception |
| `TRUNCATE` | **not blocked** (row triggers don't fire) | **not blocked** |
| `ALTER TABLE ... DISABLE TRIGGER`, then `UPDATE` | **not blocked** | **not blocked** |
| `SET session_replication_role = replica`, then `DELETE` | **not blocked** | not tried |
| For comparison, a non-owner role with only `SELECT, INSERT`: `UPDATE` / `TRUNCATE` / `DELETE` | blocked: permission denied | blocked: permission denied |

So append-only holds against the app's ordinary DML. It does **not** hold against the app's own role, because that
role can `TRUNCATE` or disable the trigger. The `docs/` claim that a "compromised app cannot rewrite history"
(`internal/audit/audit.go:4-6`, `migrations/004_audit_log.up.sql:4`) is false in this deployment.

### Refunds under concurrency
**Neither a row lock nor a constraint.** The payment is read without `FOR UPDATE` (`internal/payment/refund_service.go:54-60`).
The only amount check is `0 < amount <= payment.amount`, per request (`:76-78`). Partial refunds leave the status
`succeeded`, so further refunds are still allowed. The provider is called outside any transaction (`:112-123`).
Refund idempotency is `UNIQUE(merchant_id, idempotency_key)` plus `ON CONFLICT DO UPDATE` (`:91-101`), but a
concurrent duplicate whose row is still `pending` calls the provider again (`:107-112`). All of this is behind B4.

### Outbound delivery worker
- **Row selection:** `SELECT ... FOR UPDATE OF d SKIP LOCKED LIMIT 1` over due `pending` rows, joined to active
  endpoints (`internal/outbound/service.go:110-118`). One goroutine per instance runs on a 5 s ticker (`cmd/server/main.go:132`).
  It drains until nothing is due.
- **Backoff:** `nextBackoff(n) = 30s × 2^(n−1)`, capped at 6 h (`internal/outbound/service.go:193-199`). After failed
  attempts 1 to 7 the waits are 30, 60, 120, 240, 480, 960 and 1920 s. That is 3810 s (63.5 min) of backoff before the
  8th attempt, plus worker tick delay.
- **Dead-lettering:** when `attempts >= 8` the status is set to `dead` (`:150-154`).
- **Idempotency key sent:** `Idempotency-Key: dlv_<delivery_id>` (`:181`). It is stable across retries of one delivery
  row, and there is one row per (event, endpoint).
- **Signature:** `X-Gateway-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, t + "." + body))`. `t` changes on every
  attempt; the body does not.
- **Endpoint URLs must be `https`** (`internal/outbound/handler.go:137`).

### Pool sizes, timeouts and other limits in effect
| Setting | Value | Source |
|---|---|---|
| pgx pool | MaxConns 25, MinConns 5 | `internal/db/postgres.go:18-19` (hard-coded) |
| Redis pool | PoolSize 20, MinIdle 5 | `internal/db/redis.go:16-17` |
| HTTP server | Read 10 s, Write 30 s (env `READ_TIMEOUT`/`WRITE_TIMEOUT`), Idle 120 s | `internal/config/config.go:53-54`, `cmd/server/main.go:138-140` |
| chi request timeout | 30 s | `cmd/server/main.go:108` |
| Per-IP limit | 200 req/min, in-memory, **not configurable** | `cmd/server/main.go:107` |
| Per-merchant limit | 600 req/min (`RATE_LIMIT_PER_MERCHANT`), Redis fixed window, fail-open | `internal/middleware/ratelimit.go` |
| bcrypt cost | 12, run on **every** authenticated request | `internal/merchant/service.go:35,122` |
| Inbox worker / outbound worker / reconciler | 10 s / 5 s / every 5 min with 15 min staleness (all hard-coded) | `cmd/server/main.go:131-133` |
| Outbound HTTP timeout; provider HTTP timeout | 10 s; 30 s | `internal/outbound/service.go:38`; provider clients |
| Graceful shutdown | 30 s; workers are cancelled before the HTTP drain | `cmd/server/main.go:150-161` |

---

## 4. Spec / code / doc mismatches

- **`LOG_LEVEL` does not exist** in the app. Every request is logged at info level (`internal/middleware/auth.go:16-27`).
  The override sets it anyway, and it has no effect.
- **There is a second rate limiter the spec doesn't mention:** per-IP, 200/min, not configurable (R8). One load generator
  would hit it, so k6 sends a synthetic `X-Real-IP` per request (no code change). This works only because of the bypass in R8.
- **There is no `mock` provider name:** `payment_intents.provider` has `CHECK (provider IN ('stripe','chapa','telebirr'))`.
  The mock therefore registers under those names and fakes only the network calls.
- **The mock adds no refund capability:** like the real clients, only `stripe` supports refunds.
- **G6 timing:** dead-lettering after 8 attempts takes at least about 63.5 min of real time (backoff is hard-coded).
- **G10 timing:** the reconciler runs every 5 min and only touches intents stale for 15 min (hard-coded).
- **CI cannot pass:** `.github/workflows/ci.yml` runs `gofmt -s -l .`, and every Go file in the repo uses 4-space
  indentation, so the job fails on every file.
- **`docs/portfolio/verified-claims.md` items contradicted by code:**
  - It says the reconciler uses `FOR UPDATE SKIP LOCKED`. It doesn't (`internal/reconcile/reconcile.go:44-51`).
  - It says Telebirr uses "RSA signature verification". It is SHA-256 over sorted params plus the app key (`internal/provider/telebirr/telebirr.go:243-257`).
  - It says per-IP rate limiting uses Redis. It is in-memory `httprate`.
  - It calls the inbox "Async Processing with Retry". See B5.
  - It says "Webhook Event Deduplication" works. It does, but see R3.
  - It says the ledger is "Append-Only". Bypassable, see the table above.

---

## 5. Owner decisions (resolved)

1. B1 and B4 were fixed in separate commits (`076f8e1`, `e7be08b`) so G4/G5/G6/G8 could run.
2. On that build, G5 showed the over-refund (re-measured for the record in
   `results/prefix_e7be08b/g5/`). The owner then asked for **all** findings to be fixed and for the full
   measurement to be run on the fixed build. That build is what REPORT.md measures.
3. G6 dead-lettering is verified by fast-forwarding `next_attempt_at` with SQL; the 30 %-failure
   drain runs in real time.
