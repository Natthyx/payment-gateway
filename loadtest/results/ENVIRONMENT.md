# Test environment

Raw capture: [`environment_raw.txt`](environment_raw.txt) (taken at the start of the full run by `run.sh env`).

**Every number in this load test is local, single machine.** k6, the gateway, Postgres and
Redis share one laptop. k6 runs on macOS and reaches the gateway through Docker Desktop port
forwarding; the services run in Docker Desktop's Linux VM.

| Item | Value |
|---|---|
| Machine | Apple M2, 8 CPU cores, 8 GB RAM (`hw.memsize=8589934592`) |
| OS | macOS 26.3 (build 25D125) |
| Docker | Engine 28.5.1, Compose v2.40.2-desktop.1 |
| Docker Desktop VM | 8 CPUs, MemTotal 8,322,969,600 bytes, kernel 6.10.14-linuxkit |
| Per-container CPU / memory limits | none (`NanoCpus=0 Memory=0` for app, Postgres, Redis) |
| k6 | v2.3.0 (go1.27.1, darwin/arm64) |
| Go (app build) | go1.24.13 linux/arm64 (`golang:1.24-alpine`); host Go 1.26.3 |
| PostgreSQL | 16.15 (`postgres:16-alpine`) |
| Redis | 7.4.12 (`redis:7-alpine`) |
| App commit | `633fe7b` (branch `loadtest/benchmark`; no uncommitted app-code changes) |
| bcrypt cost-12 compare in the VM | median 259.1 ms (single goroutine); 18.7 compares/s with 8 goroutines ([`bcrypt/bcrypt_cost12.txt`](bcrypt/bcrypt_cost12.txt)) |

## Environment variables changed for the test

Set by [`../docker-compose.loadtest.yml`](../docker-compose.loadtest.yml), plus the scenario
overrides noted below. `.env` is not loaded.

| Variable | Test value | Default | Why |
|---|---|---|---|
| `PROVIDER_MODE` | `mock` | `real` | never call Stripe/Chapa/Telebirr |
| `MOCK_PROVIDER_LATENCY` | `0s` | `0s` | mock adds no artificial latency |
| `MOCK_PROVIDER_TX_STATUS` | `pending` (G10: `succeeded`) | `pending` | what the mock reports to the reconciler |
| `RATE_LIMIT_PER_MERCHANT` | `1000000` (G9 part A: `600`) | `600` | the default would turn load into 429s |
| `IP_RATE_LIMIT_PER_MINUTE` | `1000000` (G9 parts B/C: `200`) | `200` | one load generator = one client IP |
| `LOG_LEVEL` | `warn` | `info` | quiet logs (per-request logging is at info) |
| `ENV` | `test` | `live` | test-mode API keys; mock is refused with `live` |
| `STRIPE_WEBHOOK_SECRET`, `CHAPA_SECRET_KEY`, `TELEBIRR_APP_KEY`, `OUTBOUND_WEBHOOK_SIGNING_SECRET` | random, generated per checkout into `.state/` | — | throwaway |
| `STRIPE_SECRET_KEY`, Telebirr ids | empty | — | no real provider credentials anywhere |
| `DATABASE_URL` | `gateway_runtime` role, random password | compose: `gateway_runtime` | least-privilege runtime role |
| `BASE_CALLBACK_URL` | `https://gateway.loadtest.invalid` | — | required, unused by the mock |
| `SSL_CERT_FILE` (G6 only) | throwaway test CA | image default | the fake merchant receiver uses a self-signed cert |
| `AUTH_CACHE_TTL`, `TRUST_PROXY_HEADERS` | unchanged: `30s`, `false` | `30s`, `false` | |

Every run is preceded by `docker compose -p gwloadtest down -v`, so each run starts with an
empty Postgres volume and an empty Redis.
