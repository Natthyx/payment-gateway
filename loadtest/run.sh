#!/usr/bin/env bash
# Single entry point for the payment-gateway load test.
#
#   loadtest/run.sh                # everything: env, probes, G1..G10 (≈2–3 h)
#   loadtest/run.sh g2 g4          # selected scenarios
#   RUNS=1 loadtest/run.sh g3      # fewer repetitions (report requires 3)
#
# Scenarios: env probes g1 g2 g3 g4 g5 g6 g7 g8 g9 g10 down
# Every measured scenario runs RUNS (default 3) times on a freshly reset stack.
# Raw outputs land in loadtest/results/<scenario>/run<N>/.
source "$(dirname "$0")/lib/common.sh"

RUNS="${RUNS:-3}"
G2_K="${G2_K:-500}"; G2_C="${G2_C:-10}"; G2_VUS="${G2_VUS:-20}"
G3_RATES="${G3_RATES:-50 100 200 400 800 1600 2400}"; G3_DURATION="${G3_DURATION:-60s}"
G4_N="${G4_N:-1000}"
G6_N="${G6_N:-100}"; G6_DEAD_N="${G6_DEAD_N:-10}"; G6_CAP_S="${G6_CAP_S:-2400}"
M="${M:-20}"

command -v k6 >/dev/null || die "k6 not installed (brew install k6)"
command -v jq >/dev/null || die "jq not installed"
ensure_secrets
ensure_certs

# ── shared steps ─────────────────────────────────────────────────────────────
setup_merchants() { k6_run g1_merchants.js "$1/setup_merchants" -e M="$M" >/dev/null; }

warmup() { # $1 = out dir. 10 s at 5 req/s; reported nowhere, kept for transparency.
  k6_run g3_throughput.js "$1/warmup" -e RATE=5 -e DURATION=10s -e RUN_ID=warm >/dev/null
}

save_metrics() { curl -s "${2:-$APP_URL}/metrics" | grep -E '^gateway_' >"$1"; }

run_dir() { local d="$RESULTS/$1/run$2"; rm -rf "$d"; mkdir -p "$d"; echo "$d"; }

# ── env: versions, limits, commit ────────────────────────────────────────────
s_env() {
  LT_BUILD=1 stack_reset
  local f="$RESULTS/environment_raw.txt"
  {
    echo "# captured $(date -u +%FT%TZ)"
    echo "## host"; sw_vers; sysctl -n machdep.cpu.brand_string; echo "hw.memsize=$(sysctl -n hw.memsize)"; echo "hw.ncpu=$(sysctl -n hw.ncpu)"
    echo "## docker"; docker version --format 'server {{.Server.Version}} client {{.Client.Version}}'; docker compose version
    docker info --format 'VM CPUs={{.NCPU}} VM MemTotal={{.MemTotal}} OS={{.OperatingSystem}} Kernel={{.KernelVersion}}'
    echo "## k6"; k6 version
    echo "## go (host)"; go version
    echo "## go (app image builder)"; docker run --rm golang:1.24-alpine go version
    echo "## postgres"; psql_val "SELECT version()"
    echo "## redis"; docker exec "$REDIS_CONTAINER" redis-cli INFO server | grep -E '^redis_version'
    echo "## git"; git -C "$ROOT" rev-parse HEAD; git -C "$ROOT" log --oneline -4; echo "dirty files (app code):"; git -C "$ROOT" status --short -- cmd internal migrations go.mod go.sum Dockerfile
    echo "## container limits (0 = no limit)"
    for c in lt_gateway_app lt_gateway_postgres lt_gateway_redis; do
      docker inspect "$c" --format "$c NanoCpus={{.HostConfig.NanoCpus}} Memory={{.HostConfig.Memory}}"
    done
    echo "## app container environment (secrets redacted)"
    docker inspect lt_gateway_app --format '{{range .Config.Env}}{{println .}}{{end}}' |
      sed -E -e 's/^((STRIPE_WEBHOOK_SECRET|CHAPA_SECRET_KEY|TELEBIRR_APP_KEY|OUTBOUND_WEBHOOK_SIGNING_SECRET)=).*/\1<throwaway, redacted>/' \
             -e 's#(://[a-z_]+:)[^@]*@#\1<redacted>@#' | sort
  } >"$f" 2>&1
  log "env -> $f"
  local d="$RESULTS/bcrypt"; mkdir -p "$d"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$STATE/bcryptbench" ./loadtest/tools/bcryptbench)
  docker run --rm -v "$STATE/bcryptbench:/bcryptbench:ro" gcr.io/distroless/static-debian12:nonroot /bcryptbench | tee "$d/bcrypt_cost12.txt"
}

# ── Phase 0 probes (B1–B5 reproductions on the current build) ───────────────
s_probes() {
  stack_reset
  LT_TAG=current bash "$LT_DIR/probes/phase0_probes.sh" >/dev/null 2>&1
  bash "$LT_DIR/probes/refund_probe.sh" >"$RESULTS/phase0/current/refund_probe.txt" 2>&1
  stack_reset
  bash "$LT_DIR/probes/fix_probes.sh" >/dev/null 2>&1
  log "probes -> $RESULTS/phase0/current/ and $RESULTS/fixes/"
}

# ── G1 merchants ─────────────────────────────────────────────────────────────
s_g1() {
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g1 "$r"); stack_reset
    k6_run g1_merchants.js "$d/k6" -e M="$M" >/dev/null
    psql_q -c "SELECT count(*) AS merchants, count(*) FILTER (WHERE api_key_lookup IS NOT NULL) AS with_lookup FROM merchants" >"$d/merchants.txt"
    log "g1 run $r done"
  done
}

# ── G2 idempotency storm ─────────────────────────────────────────────────────
s_g2() {
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g2 "$r"); stack_reset; setup_merchants "$d"; warmup "$d"
    local cpu; cpu=$(start_docker_stats "$d/docker_stats.csv")
    k6_run g2_idempotency.js "$d/k6" -e K="$G2_K" -e C="$G2_C" -e VUS="$G2_VUS" -e RUN_ID="r$r" >/dev/null
    kill "$cpu" 2>/dev/null || true
    psql_q -v expected_keys="$G2_K" <"$LT_DIR/sql/g2_invariants.sql" >"$d/invariants.txt" 2>&1
    save_metrics "$d/metrics.txt"
    log "g2 run $r done"
  done
}

# ── G3 throughput curve (fresh stack per rate) ───────────────────────────────
s_g3() {
  for r in $(seq 1 "$RUNS"); do
    for rate in $G3_RATES; do
      local d="$RESULTS/g3/run$r/rate_$rate"; rm -rf "$d"; mkdir -p "$d"
      stack_reset; setup_merchants "$d"; warmup "$d"
      local cpu; cpu=$(start_docker_stats "$d/docker_stats.csv")
      k6_run g3_throughput.js "$d/k6" -e RATE="$rate" -e DURATION="$G3_DURATION" -e RUN_ID="r$r" >/dev/null
      kill "$cpu" 2>/dev/null || true
      psql_q -c "SELECT status, count(*) FROM payment_intents WHERE idempotency_key LIKE 'g3-r%' GROUP BY 1" >"$d/intents.txt"
      save_metrics "$d/metrics.txt"
      log "g3 run $r rate $rate done"
    done
  done
}

# ── G4 / G8 webhook -> ledger ────────────────────────────────────────────────
webhook_run() { # $1 dir, $2 run, $3 two_instances(0|1)
  local d="$1" r="$2" two="$3"
  setup_merchants "$d"
  k6_run seed_intents.js "$d/seed" -e N="$G4_N" -e RUN_ID="s$r" >/dev/null   # also serves as warm-up
  local cpu; cpu=$(start_docker_stats "$d/docker_stats.csv")
  local t0; t0=$(python3 -c 'import time;print(time.time())')
  k6_run g4_webhooks.js "$d/k6" -e RUN_ID="w$r" -e SEED="$r" -e TWO_INSTANCES="$two" -e PLAN_OUT="$d/plan.json" >/dev/null
  local t1; t1=$(python3 -c 'import time;print(time.time())')
  wait_inbox_drained 900 || log "WARNING: inbox not drained"
  local t2; t2=$(python3 -c 'import time;print(time.time())')
  kill "$cpu" 2>/dev/null || true
  python3 -c "print(f'delivery_wall_s={$t1-$t0:.3f}\nwall_end_of_delivery_to_zero_pending_s={$t2-$t1:.3f}  (polled every ~1 s; includes waiting for the 10 s worker tick)')" >"$d/drain_wallclock.txt"
  psql_q -v expected_intents="$G4_N" -v expected_events="$(jq .unique_events "$d/plan.json")" \
    <"$LT_DIR/sql/g4_invariants.sql" >"$d/invariants.txt" 2>&1
  save_metrics "$d/metrics_app1.txt"
  [[ "$two" == 1 ]] && save_metrics "$d/metrics_app2.txt" "$APP2_URL"
  return 0
}

s_g4() {
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g4 "$r"); stack_reset; webhook_run "$d" "$r" 0; log "g4 run $r done"
  done
}

s_g8() {
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g8 "$r")
    LT_EXTRA_COMPOSE="docker-compose.second.yml" stack_reset
    LT_EXTRA_COMPOSE="docker-compose.second.yml" webhook_run "$d" "$r" 1
    log "g8 run $r done"
  done
  LT_EXTRA_COMPOSE="docker-compose.second.yml" stack_down
}

# ── G5 concurrent refunds ────────────────────────────────────────────────────
s_g5() {
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g5 "$r"); stack_reset; setup_merchants "$d"
    k6_run seed_intents.js "$d/seed" -e N=4 -e AMOUNT=10000 -e RUN_ID="s$r" >/dev/null
    k6_run g4_webhooks.js "$d/webhooks" -e RUN_ID="w$r" -e EXTRA_EVENT_FRACTION=0 >/dev/null
    wait_inbox_drained 120
    psql_q -c "SELECT id, amount, status FROM payment_intents ORDER BY id" >"$d/payments_before.txt"
    k6_run g5_refunds.js "$d/k6" -e RUN_ID="r$r" >/dev/null
    psql_q <"$LT_DIR/sql/g5_invariants.sql" >"$d/invariants.txt" 2>&1
    save_metrics "$d/metrics.txt"
    docker logs lt_gateway_app 2>&1 | grep -E '"level":"error"' | grep -i refund | sed -E 's/"time":[0-9]+,//' |
      sort | uniq -c | sort -rn >"$d/app_refund_errors.txt" || true
    log "g5 run $r done"
  done
}

# ── G6 outbound webhooks ─────────────────────────────────────────────────────
receiver() { curl -s --cacert "$STATE/certs/ca.pem" --resolve receiver:18443:127.0.0.1 "https://receiver:18443$1"; }

register_endpoint() { # $1 merchant index, $2 mode
  local key; key=$(jq -r ".[$1].api_key" "$STATE/merchants.json")
  curl -s -X POST "$APP_URL/webhook-endpoints" -H "Authorization: Bearer $key" -H 'X-Real-IP: 10.6.6.6' \
    -d "{\"url\":\"https://receiver:8443/hook/$2\"}"
}

pending_outbound() { # $1 mode
  psql_val "SELECT count(*) FROM outbound_webhook_deliveries d JOIN merchant_webhook_endpoints e ON e.id=d.endpoint_id WHERE e.url LIKE '%/hook/$1' AND d.status='pending'"
}

s_g6() {
  export LT_EXTRA_COMPOSE="docker-compose.receiver.yml"
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g6 "$r")
    LT_RECEIVER_SEED="$r" LT_BUILD=1 stack_reset
    wait_url "http://127.0.0.1:18080/health"
    until receiver /stats >/dev/null; do sleep 1; done
    setup_merchants "$d"
    { register_endpoint 0 flaky; echo; register_endpoint 1 dead; echo; } >"$d/endpoints.txt"

    # Flaky endpoint, real time.
    k6_run seed_intents.js "$d/seed_flaky" -e N="$G6_N" -e MERCHANT_INDEX=0 -e RUN_ID="f$r" >/dev/null
    k6_run g4_webhooks.js "$d/webhooks_flaky" -e RUN_ID="wf$r" -e EXTRA_EVENT_FRACTION=0 >/dev/null
    wait_inbox_drained 120
    until [[ "$(psql_val "SELECT count(*) FROM outbound_webhook_deliveries")" -ge "$G6_N" ]]; do sleep 1; done
    local start=$SECONDS
    until [[ "$(pending_outbound flaky)" == 0 ]]; do
      (( SECONDS - start < G6_CAP_S )) || { log "g6: flaky not drained within cap"; break; }
      sleep 5
    done
    echo "wall_seconds_from_inbox_drained_to_no_pending_flaky=$((SECONDS - start)) (polled every 5 s; cap ${G6_CAP_S}s)" >"$d/flaky_drain_wallclock.txt"

    # Dead endpoint, fast-forwarded: before each round, snapshot the scheduled
    # next_attempt_at, then set it to now() so the worker retries immediately.
    k6_run seed_intents.js "$d/seed_dead" -e N="$G6_DEAD_N" -e MERCHANT_INDEX=1 -e RUN_ID="d$r" >/dev/null
    k6_run g4_webhooks.js "$d/webhooks_dead" -e RUN_ID="wd$r" -e EXTRA_EVENT_FRACTION=0 >/dev/null
    wait_inbox_drained 120
    local snap="$d/deadletter_snapshots.csv"
    echo "round,id,attempts,status,next_attempt_epoch,db_now_epoch" >"$snap"
    for round in $(seq 1 12); do
      # wait until every dead delivery has made attempt `round` (or is dead)
      local w=$SECONDS
      until [[ "$(psql_val "SELECT count(*) FROM outbound_webhook_deliveries d JOIN merchant_webhook_endpoints e ON e.id=d.endpoint_id WHERE e.url LIKE '%/hook/dead' AND d.status='pending' AND d.attempts < $round")" == 0 ]]; do
        (( SECONDS - w < 60 )) || break; sleep 1
      done
      psql_q -tA -F, -c "SELECT $round, d.id, d.attempts, d.status, EXTRACT(EPOCH FROM d.next_attempt_at), EXTRACT(EPOCH FROM now()) FROM outbound_webhook_deliveries d JOIN merchant_webhook_endpoints e ON e.id=d.endpoint_id WHERE e.url LIKE '%/hook/dead' ORDER BY d.id" >>"$snap"
      [[ "$(pending_outbound dead)" == 0 ]] && break
      psql_q -c "UPDATE outbound_webhook_deliveries d SET next_attempt_at = now() FROM merchant_webhook_endpoints e WHERE e.id=d.endpoint_id AND e.url LIKE '%/hook/dead' AND d.status='pending'" >/dev/null
    done
    sleep 12   # two worker ticks: confirm no attempt after 'dead'
    psql_q <"$LT_DIR/sql/g6_outbound.sql" >"$d/outbound.txt" 2>&1
    receiver /stats >"$d/receiver_stats.json"
    receiver '/stats?full=1' >"$d/receiver_full.json"
    python3 "$LT_DIR/tools/g6_backoff.py" "$d/receiver_full.json" "$snap" >"$d/backoff.txt" 2>&1
    save_metrics "$d/metrics.txt"
    log "g6 run $r done"
  done
  stack_down; unset LT_EXTRA_COMPOSE
}

# ── G7 append-only ───────────────────────────────────────────────────────────
s_g7() {
  local d="$RESULTS/g7"; rm -rf "$d"; mkdir -p "$d"; stack_reset; setup_merchants "$d"
  k6_run seed_intents.js "$d/seed" -e N=3 -e RUN_ID=s7 >/dev/null
  k6_run g4_webhooks.js "$d/webhooks" -e RUN_ID=w7 -e EXTRA_EVENT_FRACTION=0 >/dev/null
  wait_inbox_drained 60
  # one audit row via the API (merchant.create already wrote some)
  for who in gateway_runtime gateway; do
    { echo "-- g7_append_only.sql as $who @ $(date -u +%FT%TZ)"
      docker exec -i "$PG_CONTAINER" psql -U "$who" -d gateway_db -X -e <"$LT_DIR/sql/g7_append_only.sql"
    } >"$d/append_only_as_$who.txt" 2>&1
  done
  log "g7 -> $d/append_only_as_*.txt"
}

# ── G9 rate limits at defaults ───────────────────────────────────────────────
s_g9() {
  for r in $(seq 1 "$RUNS"); do
    local d; d=$(run_dir g9 "$r")
    # A: per-merchant limit at its default (600/min). The per-IP limit stays
    # raised here, otherwise its 200/min would mask the merchant limit.
    LT_RATE_LIMIT_PER_MERCHANT=600 stack_reset
    setup_merchants "$d"
    docker inspect lt_gateway_app --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E 'RATE_LIMIT|TRUST' >"$d/limits_part_merchant.txt"
    # Start one second after a minute boundary (sliding window: the previous
    # minute is empty, so the first window's budget is exactly 600).
    local s; s=$(date +%S); s=${s#0}; sleep $(( (61 - s) % 60 ))
    k6_run g9_ratelimit.js "$d/k6_merchant" -e RATE=20 -e PART=merchant >/dev/null
    # B/C: per-IP limit at its default (200/min), proxy headers not trusted.
    LT_IP_RATE_LIMIT=200 stack_reset
    setup_merchants "$d"
    docker inspect lt_gateway_app --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E 'RATE_LIMIT|TRUST' >"$d/limits_part_ip.txt"
    k6_run g9_ratelimit.js "$d/k6_ip" -e PART=ip >/dev/null
    log "g9 run $r done"
  done
}

# ── G10 reconciler ───────────────────────────────────────────────────────────
s_g10() {
  local d; d=$(run_dir g10 1)
  LT_MOCK_TX_STATUS=succeeded stack_reset
  local started=$SECONDS
  setup_merchants "$d"
  register_endpoint 0 ok >"$d/endpoint.txt"
  k6_run seed_intents.js "$d/seed" -e N=20 -e MERCHANT_INDEX=0 -e RUN_ID=s10 >/dev/null
  # Make the intents look 20 min stale. The updated_at trigger would reset the
  # column, so triggers are skipped for this one test-data statement.
  psql_q -c "BEGIN; SET LOCAL session_replication_role = replica; UPDATE payment_intents SET updated_at = now() - interval '20 minutes'; COMMIT;" >"$d/backdate.txt"
  psql_q -c "SELECT status, count(*) FROM payment_intents GROUP BY 1" >"$d/before.txt"
  until [[ "$(psql_val "SELECT count(*) FROM payment_intents WHERE status='processing'")" == 0 ]]; do
    (( SECONDS - started < 420 )) || { log "g10: reconciler did not finish within 7 min"; break; }
    sleep 5
  done
  echo "seconds_from_app_start_to_all_reconciled~=$((SECONDS - started)) (reconciler first tick is 5 min after start)" >"$d/wallclock.txt"
  sleep 15
  { psql_q -c "SELECT status, count(*) FROM payment_intents GROUP BY 1"
    psql_q -v expected_intents=20 -v expected_events=0 <"$LT_DIR/sql/g4_invariants.sql"
    psql_q -c "SELECT count(*) AS outbound_deliveries_enqueued FROM outbound_webhook_deliveries"
  } >"$d/after.txt" 2>&1
  docker logs lt_gateway_app 2>&1 | grep 'reconciler corrected' | wc -l | sed 's/^ */reconciler_corrected_log_lines=/' >>"$d/after.txt"
  save_metrics "$d/metrics.txt"
  log "g10 done"
}

s_down() { stack_down; LT_EXTRA_COMPOSE="docker-compose.receiver.yml docker-compose.second.yml" stack_down; }

ALL=(env probes g7 g9 g1 g2 g3 g4 g8 g6 g10 g5)
targets=("$@"); [[ ${#targets[@]} -eq 0 ]] && targets=("${ALL[@]}")
# Always test the code in this checkout: the image tag is shared across checkouts.
if [[ "${targets[*]}" != "down" ]]; then
  log "building app image from $(git -C "$ROOT" describe --always --dirty)"
  dc build app >/dev/null
fi
for t in "${targets[@]}"; do
  declare -F "s_$t" >/dev/null || die "unknown scenario $t (have: ${ALL[*]} down)"
  log "=== $t ==="
  "s_$t"
done
log "finished: ${targets[*]}"
