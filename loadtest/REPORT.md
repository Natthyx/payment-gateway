# Payment gateway — load test and correctness report

**Build measured:** commit `633fe7b` on branch `loadtest/benchmark` (all fixes from
[`FINDINGS.md`](FINDINGS.md) applied). **Pre-fix comparison:** see "Before vs after".
**Environment:** [`results/ENVIRONMENT.md`](results/ENVIRONMENT.md). Every number is
**local, single machine** (Apple M2, 8 GB). The load generator, the gateway, Postgres and Redis
share the same 8 cores. These numbers compare scenarios with each other; they are not a
production capacity figure.
**Payment providers:** in-process mock (`PROVIDER_MODE=mock`) for every request. No measurement
includes a real Stripe/Chapa/Telebirr round trip.
**Reproduce:** `loadtest/run.sh` (or the scenarios individually), then `python3 loadtest/tools/summarize.py`.

Format in all tables: **median (min–max) over 3 runs**, latency in milliseconds, unless stated.
Every row cites the raw files it comes from. The full generated tables are in
[`results/summary.md`](results/summary.md).

---

## Outcome at a glance

| Scenario | Result on the fixed build |
|---|---|
| G1 merchant setup | 20/20 created in every run |
| G2 idempotency storm (500 keys × 10 parallel + different body) | **0 duplicate intents**; concurrent duplicates get a retryable 409; reused key with a different body → 409 in 500/500 cases |
| G3 throughput (`POST /payments`) | **2,400 req/s for 60 s with 0 errors** (highest rate tested); no saturation point found in the tested range |
| G4 webhook → ledger (1,000 payments, 3,144 deliveries) | **all invariants hold**: ledger balanced, 3 legs per payment, every event stored once |
| G5 concurrent refunds | **no over-refund**; ledger balanced; provider refund calls = succeeded refunds |
| G6 outbound webhooks | backoff exactly 30·2ⁿ⁻¹ s; dead-lettered after exactly 8 attempts; all signatures valid; idempotency key stable |
| G7 append-only | runtime role: every UPDATE/DELETE/TRUNCATE/trigger bypass **blocked**; owner (superuser) can still disable triggers |
| G8 two instances | same invariants as G4; work split ~50/50 between instances; no double processing |
| G9 rate limits at defaults | per-merchant: exactly 600 allowed, then 429 `rate_limit_exceeded`; per-IP: spoofed `X-Real-IP` no longer bypasses |
| G10 reconciler | 20/20 stale intents corrected, ledger written, merchants notified |

---

## G1 — merchant creation (setup)

| Metric | Value | Source |
|---|---|---|
| merchants created (201) | 20 (20–20) | `results/g1/run*/k6.json` |
| `POST /merchants` p50 / p95 / max | 271 (266–272) / 297 (296–303) / 299 (296–303) | `results/g1/run*/k6.json` |

Latency here is one bcrypt hash at cost 12. The bcrypt micro-benchmark in the same VM measured a
median of 259.1 ms per compare (`results/bcrypt/bcrypt_cost12.txt`).

## G2 — idempotency storm

500 keys. For each key, 10 identical `POST /payments` were sent in parallel (`http.batch`), then
one more request with the same key and a different amount. 20 VUs, after a 10 s warm-up.

| Metric | Value | Source |
|---|---|---|
| requests | 5,500 (5,500–5,500) | `results/g2/run*/k6.json` |
| identical requests p50 / p95 / p99 | 12.0 (11.6–13.1) / 26.2 (20.4–27.9) / 88.3 (59.4–89.8) | `results/g2/run*/k6.json` |
| different-body requests p50 / p99 | 2.74 (2.58–3.49) / 13.2 (12.0–14.7) | `results/g2/run*/k6.json` |
| status 201 / 409 / 429 / 5xx / no response | 3,440 / 2,060 / 0 / 0 / 0 (medians) | `results/g2/run*/k6.json` |
| 409 `idempotency_key_in_use` (a duplicate arrived while the first was in flight) | 1,560 (1,536–1,561) | `results/g2/run*/k6.json` |
| keys where the 201 responses carried different intent ids | **0 (0–0)** | `results/g2/run*/k6.json` |
| different body → 409 `idempotency_conflict` | **500 (500–500)** | `results/g2/run*/k6.json` |
| different body → 201 (old behaviour: silent replay) | 0 (0–0) | `results/g2/run*/k6.json` |
| SQL: intents for G2 keys / duplicate (merchant, key) groups | 500 / **0** in every run | `results/g2/run*/invariants.txt` |
| SQL: intents created from the different body | 0 in every run | `results/g2/run*/invariants.txt` |

**Invariant: one intent per (merchant, key): held in all runs.** Note: most concurrent duplicates
get `409 idempotency_key_in_use` rather than an identical 201, because they arrive while the first
request is still in flight. Only 1 (0–4) of 500 keys had all 10 responses as byte-identical 201s.
That is the intended contract (retry with the same key). Clients must treat the code as retryable.

## G3 — throughput curve (`POST /payments`, distinct keys)

Fresh stack per rate; 10 s warm-up; k6 `constant-arrival-rate` for 60 s; 20 merchants; maxVUs 600.

| Target req/s | 201/s achieved | p50 | p95 | p99 | Error % | Dropped | App CPU % | Postgres CPU % |
|---|---|---|---|---|---|---|---|---|
| 50 | 50.0 (50.0–50.0) | 4.14 (3.95–4.37) | 7.42 (6.10–8.36) | 257 (256–258) | 0 | 0 | 6.94 | 5.66 |
| 100 | 100 (100–100) | 2.80 (2.79–2.82) | 5.30 (5.27–5.48) | 47.5 (45.7–48.0) | 0 | 0 | 10.3 | 8.23 |
| 200 | 200 (200–200) | 2.04 (1.99–2.07) | 3.56 (3.50–3.67) | 46.3 (41.5–46.9) | 0 | 0 | 15.3 | 9.90 |
| 400 | 400 (400–400) | 1.57 (1.54–1.62) | 3.58 (3.03–90.3) | 51.3 (38.8–406) | 0 | 0 | 22.7 | 14.4 |
| 800 | 800 (800–800) | 0.93 (0.92–0.94) | 5.93 (2.88–9.13) | 55.6 (31.4–98.4) | 0 | 0 | 23.9 | 16.2 |
| 1600 | 1,600 (1,600–1,600) | 0.79 (0.78–0.80) | 6.41 (5.39–15.8) | 43.6 (40.1–109) | 0 | 0 | 37.0 | 29.8 |
| 2400 | 2,400 (2,396–2,400) | 0.82 (0.80–1.02) | 16.9 (15.2–87.7) | 75.4 (74.3–230) | 0 | 0 (0–248) | 51.3 | 50.7 |

Source: `results/g3/run*/rate_<N>/k6.json` and `docker_stats.csv`. CPU is the median
`docker stats` sample (100 % = one core). Error % = non-201 responses / responses.

- **Knee:** none found up to 2,400 req/s. Every rate was sustained with 0 errors. At 2,400 req/s,
  p95 rose to 16.9 ms and one run dropped 248 iterations (k6 had no free VU). The highest app
  CPU sample at 2,400 req/s was about 225 % (`results/g3/run1/rate_2400/docker_stats.csv`), so the
  gateway was not CPU-bound. Where the limit lies, and whether k6 on the same machine is part
  of it, was not established.
- **The p99 at 50 req/s (257 ms) is higher than at 100–1,600 req/s.** This is consistent with
  bcrypt: the API-key cache entry for each of the 20 merchants expires every 30 s
  (`AUTH_CACHE_TTL`), and the next request for that merchant then waits for one bcrypt compare
  (~259 ms). At a low rate those waits are a larger share of all requests. This is an
  inference from the configuration and the bcrypt benchmark, not a separate measurement.

## G4 — webhook → ledger (one instance)

1,000 intents were created (this seeding is also the warm-up). Then a Stripe-signed
`payment_intent.succeeded` event was sent for each one. Each event was delivered 1–5 times, with
the copies of an event sent concurrently and events in seeded random order across 50 VUs. About
5 % of intents also got a second, distinct event id.

| Metric | Value | Source |
|---|---|---|
| unique event ids / deliveries | 1,051 (1,047–1,057) / 3,144 (3,110–3,183) | `results/g4/run*/plan.json`, `k6.json` |
| ingest responses 200 / not 200 | 3,144 / **0** | `results/g4/run*/k6.json` |
| ingest p50 / p95 / p99 | 12.8 (12.1–13.3) / 45.7 (24.1–57.3) / 76.1 (64.1–117) | `results/g4/run*/k6.json` |
| ingest throughput (delivery phase) | 8,277 (7,442–9,037) req/s | `results/g4/run*/k6.json` |
| time from end of delivery to 0 pending (wall clock, 1 s polling) | 5.66 (5.65–10.2) s | `results/g4/run*/drain_wallclock.txt` |
| inbox processing span / rate (DB clocks) | 1.09 (1.09–1.12) s / 956 (936–966) rows/s | `results/g4/run*/invariants.txt` |

The drain time is dominated by the inbox worker's 10 s ticker (hard-coded): processing itself took
about 1.1 s for ~1,050 rows.

| Invariant (every run) | Result |
|---|---|
| I1 ledger sum = 0 | **0** |
| I2 intents with unbalanced ledger | **0** |
| I3 succeeded intents without ledger / with ≠ 3 legs | **0 / 0** |
| I4 non-succeeded intents with ledger rows | **0** |
| I5 seeded intents succeeded | **1,000 / 1,000** |
| I6 webhook rows = distinct (provider, event id) = unique events sent | **yes** (e.g. 1,051 = 1,051) |
| I7 provider-leg amount ≠ intent amount | **0** |

Source: `results/g4/run*/invariants.txt` (all `ok` columns true in all 3 runs).

## G5 — concurrent refunds

Four payments of 10,000 each, all succeeded via webhook. Sub-tests a, b and c ran at the same
time (50 concurrent refund requests).

| Sub-test | 201 | 409 | 500 | Source |
|---|---|---|---|---|
| a: 20 parallel, distinct keys, amounts 1000..1019 (sum 20,190) | 9 (9–9) | 11 (11–11) | 0 | `results/g5/run*/k6.json` |
| b: 20 parallel, distinct keys, 3,000 each (sum 60,000) | 3 (3–3) | 17 (17–17) | 0 | `results/g5/run*/k6.json` |
| c: 10 parallel, same key, 2,000 | 5 (5–9) | 5 (1–5) | 0 | `results/g5/run*/k6.json` |
| d: sequential 6,000 then 6,001 | 1 | 1 | 0 | `results/g5/run*/k6.json` |

| Invariant (every run) | Result | Source |
|---|---|---|
| payments with succeeded refunds > amount | **0** | `results/g5/run*/invariants.txt` |
| ledger sum overall / payments with unbalanced ledger | **0 / 0** | `results/g5/run*/invariants.txt` |
| refund rows for the repeated key (c) | **1** | `results/g5/run*/invariants.txt` |
| refunds succeeded vs mock provider `Refund` calls | **14 vs 14** | `invariants.txt`, `metrics.txt` |

In (c), the 201s are replays of the one refund and the 409s are `idempotency_key_in_use`;
the provider was called once for that key.

## G6 — outbound (merchant) webhooks

Fake HTTPS merchant receiver (`tools/receiver`). 100 payments → `/hook/flaky` (30 % deterministic
failure per key and attempt, seed = run number; real time). 10 payments → `/hook/dead` (always
500; retries fast-forwarded by setting `next_attempt_at = now()` with SQL after recording the
scheduled time).

| Metric | run1 | run2 | run3 | Source |
|---|---|---|---|---|
| flaky: delivered / deliveries | 100/100 | 100/100 | 100/100 | `results/g6/run*/outbound.txt` |
| flaky: max attempts needed | 4 | 4 | 5 | `results/g6/run*/outbound.txt` |
| flaky: first enqueue → last delivered (DB) | 230.0 s | 230.1 s | 475.2 s | `results/g6/run*/outbound.txt` |
| flaky: median enqueue → delivered (DB) | 5.0 s | 5.0 s | 5.2 s | `results/g6/run*/outbound.txt` |
| dead: status `dead` after exactly 8 attempts | 10/10 | 10/10 | 10/10 | `results/g6/run*/outbound.txt`, `backoff.txt` |
| receiver: requests / valid signatures / invalid | 218 / 218 / 0 | 225 / 225 / 0 | 221 / 221 / 0 | `results/g6/run*/receiver_stats.json` |
| keys whose body changed between attempts; events seen under > 1 key | 0; 0 | 0; 0 | 0; 0 | `results/g6/run*/receiver_stats.json` |

Backoff scheduled by the gateway after attempt n (dead endpoint, `next_attempt_at − arrival`; 10
samples per n per run): **30.00, 60.00, 120.00, 240.00, 480.00, 960.00, 1920.00 s** in every run
(`results/g6/run*/backoff.txt`). The real-time gaps on the flaky endpoint were the backoff plus up
to one 5 s worker tick, for example 34.95–35.42 s for n = 1 (`backoff.txt`). Drain time with a 30 %
failure rate is set by the backoff schedule, not by load: one delivery needing 5 attempts gives
about 30 + 60 + 120 + 240 s plus ticks (run 3).

## G7 — append-only ledger and audit log

Each attempt ran in its own transaction and was rolled back.

| Attempt | As `gateway_runtime` (app's runtime role) | As `gateway` (owner, superuser) |
|---|---|---|
| UPDATE ledger_entries / audit_log | blocked: permission denied | blocked: trigger exception |
| DELETE | blocked: permission denied | blocked: trigger exception |
| TRUNCATE | blocked: permission denied | **blocked: TRUNCATE trigger** (was NOT blocked before the fix) |
| ALTER TABLE … DISABLE TRIGGER, then UPDATE | blocked: must be owner | **not blocked** |
| SET session_replication_role = replica, then DELETE | blocked: permission denied | **not blocked** |
| INSERT | allowed | allowed |

Source: `results/g7/append_only_as_gateway_runtime.txt`, `results/g7/append_only_as_gateway.txt`.
One run (a correctness check, not a measurement). A database superuser can always bypass
triggers; the fix ensures the application does not connect as one.

## G8 — two gateway instances, same Postgres and Redis

Same workload as G4; each delivery copy goes randomly to instance 1 or 2.

| Metric | Value | Source |
|---|---|---|
| deliveries to app1 / app2 | 1,587 / 1,541 (medians) | `results/g8/run*/k6.json` |
| webhooks processed by app1 / app2 (Prometheus) | 527 (524–529) / 523 (522–530) | `results/g8/run*/metrics_app1.txt`, `metrics_app2.txt` |
| inbox processing span / rate (DB clocks) | 0.66 (0.53–0.75) s / 1,591 (1,401–1,988) rows/s | `results/g8/run*/invariants.txt` |
| ingest p50 / p99 | 11.4 (11.0–13.3) / 63.9 (63.4–117) | `results/g8/run*/k6.json` |
| I1–I7 (as G4) | **all hold in every run** | `results/g8/run*/invariants.txt` |

Per run, app1 + app2 processed counts equal the unique events sent exactly: 527 + 530 = 1,057;
529 + 522 = 1,051; 524 + 523 = 1,047 (`metrics_app*.txt`, `plan.json`). `FOR UPDATE SKIP LOCKED`
split the inbox between the instances with no double processing: one ledger set per payment and
no duplicate event rows.

## G9 — rate limits at default settings

| Metric | Value | Source |
|---|---|---|
| A: per-merchant limit 600/min, 20 req/s for 50 s → 201 | **600 (600–600)** | `results/g9/run*/k6_merchant.json` |
| A: → 429 with code `rate_limit_exceeded` | 401 (400–401) | `results/g9/run*/k6_merchant.json` |
| A: p50 of 201 / of 429 responses | 5.58 / 2.56 ms | `results/g9/run*/k6_merchant.json` |
| B: one client IP, limit 200/min, 300 × `GET /health` → 200 / 429 | 179 / 121 | `results/g9/run*/k6_ip.json` |
| C: same, each request with a different `X-Real-IP` → 200 / 429 | 27 (24–28) / 273 (272–276) | `results/g9/run*/k6_ip.json` |

B allows 179 rather than 200 because the 20 `POST /merchants` setup calls from the same IP fall
into the same window. C shows the spoofed header is ignored (proxy headers are not trusted by
default). Before the fix, the header gave every request a fresh limit (FINDINGS R8).

## G10 — reconciler (1 run)

20 intents were left `processing`, back-dated 20 min, and the mock reports them `succeeded`.

| Metric | Value | Source |
|---|---|---|
| intents corrected to succeeded | 20 / 20 | `results/g10/run1/after.txt` |
| succeeded without ledger; ledger sum | 0; 0 | `results/g10/run1/after.txt` |
| merchant `payment.succeeded` deliveries enqueued | 20 (was 0 before the fix) | `results/g10/run1/after.txt` |
| time until corrected | ~301 s after app start (first reconciler tick is at 5 min) | `results/g10/run1/wallclock.txt` |

---

## Before vs after

The same k6 scripts, machine and settings were run against the pre-fix code:
- `035b9ea`: the original code plus only the mock provider. Used for G2, G3 and G9.
- `e7be08b`: the same plus the B1/B4 fixes. Used for G5, because refunds return 500 before B4.

Raw files: [`results/prefix_035b9ea/`](results/prefix_035b9ea/) and
[`results/prefix_e7be08b/`](results/prefix_e7be08b/); generated tables are in each `summary.md`.

**Caveat for every throughput comparison:** the pre-fix build ignores `LOG_LEVEL`, so it logged
every request at info level. During its runs the app container used about 8 cores
(`docker stats` median 818–848 % at 20–100 req/s), and one bcrypt compare costs about 259 ms.
Both point to bcrypt on every request as the limit, but the logging overhead was not isolated.

| Scenario | Metric | Pre-fix | Fixed (`633fe7b`) | Sources |
|---|---|---|---|---|
| G3 | 201/s at a **50 req/s** target | 22.2 (1.94–25.1) | **50.0 (50.0–50.0)** | `prefix_035b9ea/g3/run*/rate_50/`, `g3/run*/rate_50/` |
| G3 | p50 latency at 50 req/s | 23,814 ms (21,846–32,906) | **4.14 ms (3.95–4.37)** | same |
| G3 | dropped arrivals at 50 req/s (k6 had no free VU) | 1,416 (1,282–1,701) | 0 | same |
| G3 | 201/s at a **100 req/s** target | 9.21 (1.61–24.2); error % 39.2 (0–89.5) | **100 (100–100)**; error % 0 | `…/rate_100/` |
| G3 | highest rate tested with 0 errors in all runs | 10 req/s (p50 265 ms) | 2,400 req/s (p50 0.82 ms) | `…/rate_10/`, `…/rate_2400/` |
| G2 | same key + different body | **201, original intent replayed: 500/500** | **409 `idempotency_conflict`: 500/500** | `prefix_035b9ea/g2/run*/k6.json`, `g2/run*/k6.json` |
| G2 | concurrent duplicates got 409 with code… | `idempotency_conflict` (1,177), the same code as a misused key | `idempotency_key_in_use` (1,560), retryable | same |
| G2 | duplicate intents | 0 | 0 | `…/g2/run*/invariants.txt` |
| G5 | payments over-refunded (of 4) | **2 in every run**: 20,190 and 12,001 refunded on 10,000 payments | **0** | `prefix_e7be08b/g5/run*/invariants.txt`, `g5/run*/invariants.txt` |
| G5 | provider refund calls vs refunds recorded as succeeded | 43 vs 24: 19 refunds executed at the provider were left `pending` | 14 vs 14 | `…/g5/run*/metrics.txt`, `invariants.txt` |
| G5 | 20 parallel 3,000 refunds | 1 × 201, 19 × 500 | 3 × 201, 17 × 409 | `…/g5/run*/k6.json` |
| G9 | rotating `X-Real-IP`, 300 requests, limit 200/min | **300 allowed (bypass)** | 27 (24–28) allowed | `…/g9/run*/k6_ip.json` |
| G9 | per-merchant 429 body | code under `error` (non-standard envelope) | `error_obj.code = rate_limit_exceeded` | `…/g9/run*/k6_merchant.json` |
| G9 | p50 latency of a rejected (429) request | 276 ms (bcrypt runs before the limiter) | 2.56 ms | same |
| G4 | Stripe-signed webhook processed | **no: verification always failed (B1)** (single probe) | 3,144/3,144 deliveries accepted; 1,000/1,000 payments succeeded | `phase0/b1_b5_stripe_webhook.txt`, `g4/run*/` |
| G7 | `TRUNCATE ledger_entries` as the app's role | **succeeded** (single probe) | permission denied | `phase0/append_only_probe.txt`, `g7/append_only_as_gateway_runtime.txt` |

G4, G6, G8 and G10 were not load-tested on the pre-fix code: B1 meant no payment could succeed
through a webhook, so those scenarios had nothing to measure.

---

## Claims supported by evidence

Each claim holds only as stated: local, single machine (Apple M2, 8 GB), mocked payment
provider, 3 runs unless noted. The file next to each claim is the evidence.

1. **Built a reproducible load and correctness harness** (k6, SQL invariant checks, a fake HTTPS
   merchant receiver, a mock provider) for a Go/Postgres/Redis payment gateway. One command,
   `loadtest/run.sh`. → `loadtest/README.md`
2. **Found and fixed 5 reproducible bugs**: every Stripe/Chapa webhook failed signature
   verification (the inbox stored JSONB); every refund returned 500; payment read endpoints
   returned 500; audit rows were silently dropped; inbox retries had no delay.
   → `results/phase0/*.txt` (before), `results/phase0/after_fix/`, `results/phase0/current/` (after)
3. **Eliminated an over-refund race:** with 50 concurrent refund requests, 2 of 4 payments were
   over-refunded before the fix (20,190 and 12,001 refunded on 10,000), and 0 after, in 3 of 3 runs.
   The ledger stayed balanced. Provider refund calls now equal recorded refunds (14 = 14; before:
   43 vs 24). → `results/prefix_e7be08b/g5/`, `results/g5/`
4. **Removed a bcrypt bottleneck:** at a 50 req/s target, created payments went from 22.2 to
   50.0 per second and p50 latency from 23.8 s to 4.1 ms. The fix caches API-key verification
   (bcrypt cost 12 ≈ 259 ms) and collapses concurrent cache misses with singleflight. (The pre-fix
   build also logged every request; that factor was not isolated.)
   → `results/prefix_035b9ea/g3/`, `results/g3/`, `results/bcrypt/`
5. **The fixed build sustained 2,400 `POST /payments` per second for 60 s with 0 errors**
   (p50 0.82 ms, p99 75 ms, 3 runs). This is the highest rate tested, not a measured maximum.
   → `results/g3/run*/rate_2400/`
6. **Idempotency under concurrency:** 5,500 requests (500 keys × 10 parallel + 1 conflicting) gave
   0 duplicate payment intents. A key reused with different parameters is rejected (409) in 500/500
   cases; before the fix, 500/500 were silently answered with the original payment.
   → `results/g2/`, `results/prefix_035b9ea/g2/`
7. **Exactly-once webhook-to-ledger processing:** about 3,144 deliveries (each event 1–5 times,
   concurrent, random order) for 1,000 payments. Every event was stored once, and every payment got
   exactly one set of 3 balanced ledger entries, in 3 of 3 runs, on one instance and on two instances.
   With two instances, `FOR UPDATE SKIP LOCKED` split the work (524–530 rows each) and per-instance
   counts summed exactly to the events sent. → `results/g4/`, `results/g8/`
8. **Outbound webhook retries match the design:** the scheduled backoff measured exactly 30·2ⁿ⁻¹ s
   for n = 1..7; deliveries dead-letter after exactly 8 attempts; all 664 deliveries to the receiver
   carried a valid HMAC signature and a stable per-delivery idempotency key. → `results/g6/`
9. **Append-only enforced against the application:** the app's runtime DB role cannot UPDATE,
   DELETE or TRUNCATE the ledger or audit log, nor disable their triggers (1 run).
   → `results/g7/append_only_as_gateway_runtime.txt`
10. **Closed a rate-limit bypass:** a spoofed `X-Real-IP` let 300/300 requests through a 200/min
    per-IP limit; after the fix 27/300 got through (the rest 429). → `results/prefix_035b9ea/g9/`, `results/g9/`

## Claims NOT supported

- **Any production or real-provider throughput or latency.** All traffic used an in-process mock
  provider with 0 latency on one laptop, with k6 sharing the CPU. Statements like "handles 2,400 TPS"
  without that context are not supported.
- **A saturation point ("knee") for the fixed build.** None was found up to 2,400 req/s. Whether the
  gateway, Postgres or the co-located k6 would limit first was not established.
- **That the throughput gain comes only from the auth cache.** The pre-fix build also logged every
  request at info level; the two were not separated.
- **Chapa and Telebirr under load.** Only Stripe-format webhooks were load-tested. The Chapa/Telebirr
  rounding fix and their webhook paths have no load-test evidence.
- **R13 (provider idempotency-key collision across merchants) is fixed in code but untested.** The
  mock does not model provider-side idempotency.
- **Provider failures and timeouts.** The mock never fails, so refund/charge failure paths (R7)
  and provider-latency effects were not exercised.
- **"Tamper-proof" ledger.** A database superuser (the `gateway` owner role) can still disable
  triggers or bypass them with `session_replication_role` (`results/g7/append_only_as_gateway.txt`).
- **End-to-end exactly-once delivery to merchants.** Outbound delivery is at-least-once by design
  (lease expiry after a crash can resend). Merchants must dedupe on `Idempotency-Key`.
- **Reconciler at scale.** G10 is 1 run with 20 intents. G7 is also a single run.
- **Pre-fix behaviour of webhooks, outbound delivery, two instances and the reconciler under load.**
  Not measured: B1 made those flows impossible on the original code. The B1 and append-only
  "before" evidence is a single probe each.
- **Soak or long-duration stability, graceful shutdown under load, and Redis or Postgres outages**
  (including the limiter's fail-open path). None were tested; the longest measured run is 60 s per rate.
- **Statements in `docs/portfolio/verified-claims.md` that FINDINGS §4 lists as wrong** (for example
  the reconciler using `SKIP LOCKED`, or Telebirr RSA verification). That file was not updated.
