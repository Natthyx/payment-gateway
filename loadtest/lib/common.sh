#!/usr/bin/env bash
# Shared helpers for the load-test harness. Source, don't execute.
#
# Everything here acts on the compose project "gwloadtest" only: containers
# lt_gateway_*, volume gwloadtest_postgres_data. The dev stack (project
# "payment-gateway", containers gateway_*) is never touched.

set -euo pipefail

LT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$LT_DIR/.." && pwd)"
STATE="$LT_DIR/.state"
RESULTS="$LT_DIR/results"
PROJECT="gwloadtest"
PG_CONTAINER="lt_gateway_postgres"
REDIS_CONTAINER="lt_gateway_redis"
APP_URL="${APP_URL:-http://127.0.0.1:18080}"
APP2_URL="${APP2_URL:-http://127.0.0.1:18081}"

mkdir -p "$STATE" "$RESULTS"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# ── secrets ──────────────────────────────────────────────────────────────────
# Throwaway values, generated once per checkout, never committed (.state/ is
# gitignored). Nothing here is valid anywhere outside this test stack.
ensure_secrets() {
  local f="$STATE/secrets.env"
  if [[ ! -f "$f" ]]; then
    ( umask 077
      {
        echo "LT_STRIPE_WEBHOOK_SECRET=whsec_lt_$(openssl rand -hex 24)"
        echo "LT_CHAPA_SECRET=lt_chapa_$(openssl rand -hex 24)"
        echo "LT_OUTBOUND_SECRET=$(openssl rand -hex 32)"
      } >"$f" )
  fi
  grep -q '^LT_TELEBIRR_APP_KEY=' "$f" || echo "LT_TELEBIRR_APP_KEY=lt_tb_$(openssl rand -hex 24)" >>"$f"
  grep -q '^LT_DB_RUNTIME_PASSWORD=' "$f" || echo "LT_DB_RUNTIME_PASSWORD=$(openssl rand -hex 16)" >>"$f"
  set -a; source "$f"; set +a
}

# Self-signed CA + leaf for the fake receiver (hostname "receiver" on the
# compose network). World-readable because the gateway runs as nonroot.
ensure_certs() {
  local d="$STATE/certs"
  [[ -f "$d/ca.pem" && -f "$d/server.pem" ]] && return 0
  mkdir -p "$d"
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=gwloadtest-ca" \
    -keyout "$d/ca.key" -out "$d/ca.pem" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -subj "/CN=receiver" \
    -keyout "$d/server.key" -out "$d/server.csr" 2>/dev/null
  printf 'subjectAltName=DNS:receiver,DNS:localhost\nextendedKeyUsage=serverAuth\n' >"$d/ext.cnf"
  openssl x509 -req -in "$d/server.csr" -CA "$d/ca.pem" -CAkey "$d/ca.key" -CAcreateserial \
    -days 30 -extfile "$d/ext.cnf" -out "$d/server.pem" 2>/dev/null
  chmod 755 "$d"; chmod 644 "$d"/*.pem "$d/server.key"
}

# ── compose ──────────────────────────────────────────────────────────────────
# LT_EXTRA_COMPOSE: space-separated extra override files (receiver, second app).
dc() {
  local args=(-p "$PROJECT" -f "$ROOT/docker-compose.yaml" -f "$LT_DIR/docker-compose.loadtest.yml")
  for f in ${LT_EXTRA_COMPOSE:-}; do args+=(-f "$LT_DIR/$f"); done
  (cd "$ROOT" && docker compose "${args[@]}" "$@")
}

wait_url() {
  local url="$1" deadline=$((SECONDS + ${2:-120}))
  until curl -fsS -o /dev/null "$url"; do
    (( SECONDS < deadline )) || die "timed out waiting for $url"
    sleep 1
  done
}

wait_ready() {
  wait_url "$APP_URL/health"
  if [[ "${LT_EXTRA_COMPOSE:-}" == *second* ]]; then wait_url "$APP2_URL/health"; fi
}

# Drop the test volume and start from an empty database and Redis.
stack_reset() {
  [[ "$PROJECT" == "gwloadtest" ]] || die "refusing to reset project $PROJECT"
  log "reset: down -v (project $PROJECT only)"
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  # One retry: Docker Desktop occasionally fails a single "up" transiently.
  dc up -d ${LT_BUILD:+--build} >/dev/null 2>&1 ||
    { sleep 5; dc up -d ${LT_BUILD:+--build} >/dev/null 2>&1; } || die "compose up failed"
  wait_ready
  log "reset: stack ready"
}

stack_down() { dc down -v --remove-orphans >/dev/null 2>&1 || true; }

# ── data access ──────────────────────────────────────────────────────────────
psql_q() { docker exec -i "$PG_CONTAINER" psql -U gateway -d gateway_db -v ON_ERROR_STOP=1 -X "$@"; }
psql_val() { psql_q -tA -c "$1"; }

# Run a SQL file; echo the output and save it under results/.
run_sql() {
  local sql="$1" out="$2"
  { echo "-- $(basename "$sql") @ $(date -u +%FT%TZ)"; psql_q -e <"$sql"; } 2>&1 | tee "$out"
}

# Poll until the inbox has no pending rows (not a fixed sleep).
wait_inbox_drained() {
  local deadline=$((SECONDS + ${1:-900})) n
  while :; do
    n=$(psql_val "SELECT count(*) FROM webhooks WHERE status='pending'")
    [[ "$n" == "0" ]] && return 0
    (( SECONDS < deadline )) || { log "inbox still has $n pending"; return 1; }
    sleep 1
  done
}

# ── k6 ───────────────────────────────────────────────────────────────────────
# k6_run <script> <out-prefix> [k6 args...]: writes <out-prefix>.json (summary)
# and <out-prefix>.txt (console output).
k6_run() {
  local script="$1" out="$2"; shift 2
  k6 run --quiet "$@" -e SUMMARY_OUT="$out.json" \
    -e APP_URL="$APP_URL" -e APP2_URL="$APP2_URL" -e STATE="$STATE" \
    -e STRIPE_WEBHOOK_SECRET="$LT_STRIPE_WEBHOOK_SECRET" -e TELEBIRR_APP_KEY="$LT_TELEBIRR_APP_KEY" \
    "$LT_DIR/k6/$script" 2>&1 | tee "$out.txt"
  return "${PIPESTATUS[0]}"
}

# Sample container CPU/memory every ~2 s into a file until killed.
start_docker_stats() {
  local out="$1"
  # The subshell must not inherit the caller's stdout: callers use $(...),
  # which would otherwise wait for this background loop forever.
  ( while :; do
      docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' 2>/dev/null |
        grep '^lt_' | sed "s/^/$(date +%s),/"
    done >"$out" ) </dev/null >/dev/null 2>&1 &
  echo $!
}
