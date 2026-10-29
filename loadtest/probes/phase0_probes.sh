#!/usr/bin/env bash
# Phase 0 reproductions for the bugs listed in FINDINGS.md (B1, B2, B3, B5).
# Run against a freshly reset test stack. Writes results/phase0/*.txt.
source "$(dirname "$0")/../lib/common.sh"; ensure_secrets; set +e
OUT="$RESULTS/phase0${LT_TAG:+/$LT_TAG}"; mkdir -p "$OUT"
hdr() { echo "# $1 @ $(date -u +%FT%TZ) (local, single machine, PROVIDER_MODE=mock, commit $(git -C "$ROOT" describe --always --dirty))"; }

R=$(curl -s -X POST "$APP_URL/merchants" -d '{"name":"p0","email":"p0@lt.invalid"}')
KEY=$(echo "$R" | jq -r .data.api_key)

# ── B3: audit row for merchant.create (no proxy header) ─────────────────────
{
  hdr "B3 audit_log write for POST /merchants without X-Forwarded-For/X-Real-IP"
  echo "--- audit_log rows for action merchant.create:"
  psql_q -c "SELECT count(*) FROM audit_log WHERE action='merchant.create'"
  echo "--- app log:"
  docker logs lt_gateway_app 2>&1 | grep 'audit write failed' | tail -1
  echo "--- same request WITH X-Real-IP header:"
  curl -s -o /dev/null -w "HTTP %{http_code}\n" -X POST "$APP_URL/merchants" -H "X-Real-IP: 10.1.2.3" -d '{"name":"p0b","email":"p0b@lt.invalid"}'
  psql_q -c "SELECT action, ip FROM audit_log WHERE action='merchant.create'"
} 2>&1 | tee "$OUT/b3_audit_ip.txt"

# ── B2: read endpoints ───────────────────────────────────────────────────────
P=$(curl -s -X POST "$APP_URL/payments" -H "Authorization: Bearer $KEY" -H "Idempotency-Key: p0-1" \
  -d '{"amount":1050,"currency":"USD","provider":"stripe"}')
PID=$(echo "$P" | jq -r .data.payment_intent.id); TX=$(echo "$P" | jq -r .data.payment_intent.provider_tx_id)
{
  hdr "B2 GET endpoints for a normal processing intent (failure_reason IS NULL)"
  echo "POST /payments -> $(echo "$P" | jq -c '{success, id: .data.payment_intent.id, status: .data.payment_intent.status}')"
  for path in "/payments/$PID" "/payments" "/payments/$PID/ledger"; do
    echo "--- GET $path"
    curl -s -w "\nHTTP %{http_code}\n" "$APP_URL$path" -H "Authorization: Bearer $KEY"
  done
  psql_q -c "SELECT id, status, provider_tx_id, failure_reason IS NULL AS failure_reason_is_null FROM payment_intents WHERE id=$PID"
  echo "--- app log:"
  docker logs lt_gateway_app 2>&1 | grep 'cannot scan NULL' | tail -3
} 2>&1 | tee "$OUT/b2_get_endpoints.txt"

# ── B1 + B5: Stripe-signed webhook ──────────────────────────────────────────
TS=$(date +%s)
BODY="{\"id\":\"evt_p0_$TS\",\"type\":\"payment_intent.succeeded\",\"created\":$TS,\"data\":{\"object\":{\"id\":\"$TX\",\"amount\":1050,\"currency\":\"usd\"}}}"
hmac() { printf '%s.%s' "$TS" "$1" | openssl dgst -sha256 -hmac "$LT_STRIPE_WEBHOOK_SECRET" -hex | sed 's/^.* //'; }
SIG=$(hmac "$BODY")
{
  hdr "B1/B5 Stripe-signed webhook through the inbox"
  echo "raw body sent: $BODY"
  curl -s -w "  HTTP %{http_code}\n" -X POST "$APP_URL/webhooks/stripe" -H "Stripe-Signature: t=$TS,v1=$SIG" -d "$BODY"
  for _ in $(seq 1 20); do
    s=$(psql_val "SELECT status FROM webhooks WHERE provider_event_id='evt_p0_$TS'"); [[ "$s" != pending ]] && break; sleep 1
  done
  psql_q -c "SELECT id, status, attempts, last_error FROM webhooks WHERE provider_event_id='evt_p0_$TS'"
  STORED=$(psql_val "SELECT payload::text FROM webhooks WHERE provider_event_id='evt_p0_$TS'")
  echo "payload as stored (JSONB) and re-read by the worker: $STORED"
  [[ "$(hmac "$BODY")" == "$SIG" ]] && echo "HMAC(raw body sent)      == Stripe-Signature v1: yes"
  [[ "$(hmac "$STORED")" == "$SIG" ]] && echo "HMAC(stored JSONB text) == Stripe-Signature v1: yes" || echo "HMAC(stored JSONB text) == Stripe-Signature v1: NO"
  psql_q -c "SELECT id, status FROM payment_intents WHERE id=$PID"
  echo "--- B5: worker log lines for this webhook (all attempts, note timestamps):"
  WID=$(psql_val "SELECT id FROM webhooks WHERE provider_event_id='evt_p0_$TS'")
  docker logs lt_gateway_app 2>&1 | grep "\"webhook_id\":$WID," | grep 'processing failed'
} 2>&1 | tee "$OUT/b1_b5_stripe_webhook.txt"
exit 0
