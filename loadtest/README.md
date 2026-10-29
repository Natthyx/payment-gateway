# Load test and benchmark harness

Reproducible load and correctness tests for the payment gateway. Results are
in [`REPORT.md`](REPORT.md), bugs and spec mismatches are in
[`FINDINGS.md`](FINDINGS.md), and raw evidence is under [`results/`](results/).

## Run it

```bash
loadtest/run.sh                 # everything, 3 runs per scenario (about 2.5–3 h)
loadtest/run.sh g2 g4           # selected scenarios
RUNS=1 loadtest/run.sh g3       # quick look (the report always uses RUNS=3)
python3 loadtest/tools/summarize.py   # rebuild results/summary.md from raw files
loadtest/run.sh down            # remove the test stack and its volume
```

**Prerequisites:** Docker Desktop (Compose ≥ 2.24, for `!reset`/`!override`), `k6`
(`brew install k6`), `jq`, `openssl`, `python3` and Go (only for the bcrypt micro-benchmark).

## What it touches, and what it never touches

- **Its own stack only.** Compose project `gwloadtest`; containers `lt_gateway_*` and
  `lt_receiver`; volume `gwloadtest_postgres_data`. Postgres and Redis are not published
  to the host. The app listens on `127.0.0.1:18080` (and `:18081` for the second
  instance in G8). Every reset runs `docker compose -p gwloadtest down -v`. The dev
  stack (`gateway_*`, port 5432/6379/8080) is never touched.
- **No real payment providers.** `PROVIDER_MODE=mock` registers in-process mocks
  (`internal/provider/mock`). They fake charge, refund and lookup calls and use the
  real clients only to verify webhook signatures, which is local computation. `.env` is
  not loaded (`env_file: !reset`). Provider API keys are empty, and in G6 the gateway
  trusts only a throwaway CA.
- **No committed secrets.** Webhook secrets, the outbound signing secret, TLS
  material and merchant API keys are generated at test time into `loadtest/.state/`,
  which is gitignored. Summary JSON files are written without k6 `setup_data`, so no
  API key reaches `results/`.

## Changes made to the application

| Commit | Change |
|---|---|
| `035b9ea` | mock provider behind `PROVIDER_MODE=mock` (default `real`; refused with `ENV=live`) |
| `076f8e1` | **fix B1**: verify webhook signatures over the raw body |
| `e7be08b` | **fix B4**: refund insert no longer scans NULL into a string |
| `87e0367` | **fixes B2, B3, B5, R1–R13** (see FINDINGS.md §0) |
| `557b272` | **append-only hardening**: TRUNCATE triggers and a least-privilege runtime DB role |
| `51b1487` | `gofmt -s` (CI formatting check) |
| `633fe7b` | auth cache: one bcrypt per key per miss (singleflight) |

`REPORT.md` measures `633fe7b`. The original behaviour is documented with evidence in
`FINDINGS.md` and `results/phase0/`.

Test-only configuration (`docker-compose*.yml` in this directory) is listed in
[`results/ENVIRONMENT.md`](results/ENVIRONMENT.md). k6 still sends a random `X-Real-IP`.
The fixed gateway ignores that header unless `TRUST_PROXY_HEADERS=true`, so instead the
override raises `IP_RATE_LIMIT_PER_MINUTE`. G9 tests the default 200/min.

## Scenarios

| | What it does | What it shows (and what it does not) |
|---|---|---|
| G1 | create 20 merchants via `POST /merchants` | setup; keys go to `.state/merchants.json` |
| G2 | 500 keys × 10 identical parallel `POST /payments`, then the same key with a different body | at most one intent per (merchant, key) under concurrency; what clients actually receive (201 vs 409) |
| G3 | distinct keys at 50/100/200/400/800/1600/2400 req/s for 60 s each, fresh stack per rate | latency/throughput curve and the knee on this machine; app/Postgres CPU |
| G4 | 1000 intents; signed success webhooks, each event 1–5× concurrently, random order, 5 % with a second event id | inbox dedupe, exactly-once ledger, balanced ledger, drain time |
| G5 | concurrent and sequential refunds whose sum exceeds the payment; same-key repeats | whether total refunded can exceed the payment, and ledger balance |
| G6 | outbound webhooks to a fake HTTPS merchant: 30 % failure (real time) and always-fail (fast-forwarded) | backoff schedule, dead-lettering after 8 attempts, stable idempotency key, signature |
| G7 | `UPDATE`/`DELETE`/`TRUNCATE`/trigger bypass on `ledger_entries` and `audit_log`, as the runtime role and as the owner | what actually enforces append-only |
| G8 | G4 spread across two gateway instances | `FOR UPDATE SKIP LOCKED`: no double processing across instances |
| G9 | default per-merchant limit (600/min) and per-IP limit (200/min) | 429s with `rate_limit_exceeded`; a spoofed `X-Real-IP` no longer bypasses the IP limit |
| G10 | intents left `processing` past 15 min, mock reports `succeeded` | reconciler corrects state and ledger; whether merchants are notified |

Every measured scenario runs 3 times on a freshly reset database (G3: fresh stack per rate).
Warm-up (10 s at 5 req/s, or the seeding phase for G4/G8) is kept out of the measured
k6 run. Setup (merchants, intents) always runs before the measured phase.

## Layout

```
run.sh                     entry point
lib/common.sh              compose wrapper, reset, psql helpers, secrets/certs
k6/*.js                    scenarios (lib/common.js: signing, status counters, summary writer)
sql/*.sql                  invariant checks run after each scenario
probes/*.sh                Phase 0 bug reproductions (phase0_probes, refund_probe) and fix checks (fix_probes)
tools/receiver/            fake merchant webhook receiver (Go, HTTPS)
tools/bcryptbench/         bcrypt cost-12 micro-benchmark (runs inside the Docker VM)
tools/g6_backoff.py        retry gaps / scheduled backoff from receiver log + DB snapshots
tools/summarize.py         median/min/max tables -> results/summary.md
results/                   raw evidence (k6 JSON + text, psql output, metrics, docker stats)
```

## Caveats for anyone quoting the numbers

All numbers come from one Apple M2 (8 GB) laptop. k6, the gateway, Postgres and Redis
share the same 8 cores, and requests pass through Docker Desktop's port forwarding.
They are **local, single-machine** numbers: useful for comparing scenarios with each
other, not as production capacity.
