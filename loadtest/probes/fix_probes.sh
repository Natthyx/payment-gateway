#!/usr/bin/env bash
# Reproductions for the webhook findings, run against the FIXED build:
#   R3 forged event id, R5 cross-merchant match, R12 amount mismatch,
#   B5 retry backoff, R4 backlog older than the 5-minute window.
# Writes results/fixes/*.txt. Expects a freshly reset stack.
source "$(dirname "$0")/../lib/common.sh"; ensure_secrets; set +e
OUT="$RESULTS/fixes"; mkdir -p "$OUT"
hdr() { echo "# $1 @ $(date -u +%FT%TZ) (local, single machine, PROVIDER_MODE=mock, commit $(git -C "$ROOT" describe --always --dirty))"; }
sig() { printf '%s.%s' "$1" "$2" | openssl dgst -sha256 -hmac "$LT_STRIPE_WEBHOOK_SECRET" -hex | sed 's/^.* //'; }
body() { echo "{\"id\":\"$1\",\"type\":\"payment_intent.succeeded\",\"created\":$(date +%s),\"data\":{\"object\":{\"id\":\"$2\",\"amount\":$3,\"currency\":\"usd\"}}}"; }
merchant() { curl -s -X POST "$APP_URL/merchants" -d "{\"name\":\"$1\",\"email\":\"$1-$RANDOM$RANDOM@lt.invalid\"}" | jq -r .data.api_key; }
intent() { curl -s -X POST "$APP_URL/payments" -H "Authorization: Bearer $1" -H "Idempotency-Key: $2" \
  -d "{\"amount\":$3,\"currency\":\"USD\",\"provider\":\"stripe\"}"; }
post_wh() { # body, signature-header
  curl -s -o /dev/null -w "%{http_code}" -X POST "$APP_URL/webhooks/stripe" -H "Stripe-Signature: $2" -d "$1"; }
wait_wh() { for _ in $(seq 1 25); do [[ "$(psql_val "SELECT status FROM webhooks WHERE provider_event_id='$1'")" != pending ]] && return; sleep 1; done; }

KA=$(merchant fa); KB=$(merchant fb)

# ── R3 ──────────────────────────────────────────────────────────────────────
{
  hdr "R3 forged body claims a real event id before the genuine delivery"
  P=$(intent "$KA" r3 1050); TX=$(echo "$P" | jq -r .data.payment_intent.provider_tx_id)
  B=$(body evt_r3 "$TX" 1050); T=$(date +%s)
  echo "forged (bad signature) -> HTTP $(post_wh "$B" "t=$T,v1=0000")"
  echo "webhook rows for evt_r3 after forged: $(psql_val "SELECT count(*) FROM webhooks WHERE provider_event_id='evt_r3'")"
  echo "genuine -> HTTP $(post_wh "$B" "t=$T,v1=$(sig "$T" "$B")")"
  wait_wh evt_r3
  psql_q -c "SELECT provider_event_id, status, attempts FROM webhooks WHERE provider_event_id='evt_r3'" \
         -c "SELECT id, status FROM payment_intents WHERE provider_tx_id='$TX'"
} 2>&1 | tee "$OUT/r3_forged_event_id.txt"

# ── R5 ──────────────────────────────────────────────────────────────────────
{
  hdr "R5 merchant A's idempotency key equals merchant B's provider_tx_id"
  PB=$(intent "$KB" r5-b 2000); TXB=$(echo "$PB" | jq -r .data.payment_intent.provider_tx_id)
  PA=$(intent "$KA" "$TXB" 3000)
  echo "B intent: $(echo "$PB" | jq -c '.data.payment_intent | {id, provider_tx_id}')"
  echo "A intent: $(echo "$PA" | jq -c '.data.payment_intent | {id, idempotency_key}')"
  B=$(body evt_r5 "$TXB" 2000); T=$(date +%s)
  echo "success webhook for B's transaction -> HTTP $(post_wh "$B" "t=$T,v1=$(sig "$T" "$B")")"
  wait_wh evt_r5
  psql_q -c "SELECT pi.id, pi.merchant_id, pi.idempotency_key, pi.status, (SELECT count(*) FROM ledger_entries l WHERE l.payment_intent_id = pi.id) AS ledger_rows FROM payment_intents pi WHERE pi.idempotency_key IN ('r5-b', '$TXB') ORDER BY pi.id"
} 2>&1 | tee "$OUT/r5_cross_merchant.txt"

# ── R12 ─────────────────────────────────────────────────────────────────────
{
  hdr "R12 signed webhook whose amount differs from the intent"
  P=$(intent "$KA" r12 1050); TX=$(echo "$P" | jq -r .data.payment_intent.provider_tx_id)
  B=$(body evt_r12 "$TX" 999); T=$(date +%s)
  echo "webhook (amount 999 vs intent 1050) -> HTTP $(post_wh "$B" "t=$T,v1=$(sig "$T" "$B")")"
  wait_wh evt_r12
  psql_q -c "SELECT status, attempts, last_error FROM webhooks WHERE provider_event_id='evt_r12'" \
         -c "SELECT status, (SELECT count(*) FROM ledger_entries l WHERE l.payment_intent_id = p.id) AS ledger_rows FROM payment_intents p WHERE provider_tx_id='$TX'"
} 2>&1 | tee "$OUT/r12_amount_mismatch.txt"

# ── B5 ──────────────────────────────────────────────────────────────────────
{
  hdr "B5 retryable failure (no intent for the transaction yet) is retried with backoff"
  B=$(body evt_b5 mock_does_not_exist 1050); T=$(date +%s)
  echo "webhook for unknown transaction -> HTTP $(post_wh "$B" "t=$T,v1=$(sig "$T" "$B")")"
  for _ in $(seq 1 15); do [[ "$(psql_val "SELECT attempts FROM webhooks WHERE provider_event_id='evt_b5'")" -ge 1 ]] && break; sleep 1; done
  psql_q -c "SELECT status, attempts, round(EXTRACT(EPOCH FROM next_attempt_at - now())) AS next_attempt_in_s, last_error FROM webhooks WHERE provider_event_id='evt_b5'"
  echo "after 12 more seconds (one worker tick):"
  sleep 12
  psql_q -c "SELECT status, attempts FROM webhooks WHERE provider_event_id='evt_b5'"
} 2>&1 | tee "$OUT/b5_retry_backoff.txt"

# ── R4 ──────────────────────────────────────────────────────────────────────
{
  hdr "R4 webhook received 10 minutes ago (valid on receipt) processed now"
  P=$(intent "$KA" r4 1050); TX=$(echo "$P" | jq -r .data.payment_intent.provider_tx_id)
  T=$(( $(date +%s) - 600 )); B=$(body evt_r4 "$TX" 1050); S="t=$T,v1=$(sig "$T" "$B")"
  echo "simulating a 10-minute backlog: row inserted directly with created_at = receipt time = signature time"
  psql_q -c "INSERT INTO webhooks (provider, provider_event_id, event_type, payload, raw_payload, signature, status, created_at)
             VALUES ('stripe', 'evt_r4', 'payment_intent.succeeded', '$B'::jsonb, convert_to('$B', 'UTF8'), '$S', 'pending', to_timestamp($T))"
  wait_wh evt_r4
  psql_q -c "SELECT status, attempts, last_error FROM webhooks WHERE provider_event_id='evt_r4'" \
         -c "SELECT status FROM payment_intents WHERE provider_tx_id='$TX'"
} 2>&1 | tee "$OUT/r4_backlog.txt"
exit 0
