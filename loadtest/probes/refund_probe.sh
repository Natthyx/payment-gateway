#!/usr/bin/env bash
# Phase 0 probe: does POST /payments/{id}/refunds work at all?
# Test-only shortcut: the payment is moved to 'succeeded' with SQL because the
# Stripe webhook path cannot verify signatures (FINDINGS.md B1).
source "$(dirname "$0")/../lib/common.sh"; ensure_secrets; set +e
echo "# refund probe @ $(date -u +%FT%TZ) (local, single machine, PROVIDER_MODE=mock, commit $(git -C "$ROOT" describe --always --dirty))"
R=$(curl -s -X POST "$APP_URL/merchants" -H "X-Real-IP: 10.9.9.9" -d "{\"name\":\"rf\",\"email\":\"rf-$RANDOM$RANDOM@lt.invalid\"}")
KEY=$(echo "$R" | jq -r .data.api_key)
P=$(curl -s -X POST "$APP_URL/payments" -H "Authorization: Bearer $KEY" -H "Idempotency-Key: rf-$RANDOM$RANDOM" \
  -d '{"amount":10000,"currency":"USD","provider":"stripe"}')
PID=$(echo "$P" | jq -r .data.payment_intent.id)
echo "payment $PID created (status $(echo "$P" | jq -r .data.payment_intent.status)); setting status=succeeded via SQL"
psql_q -c "UPDATE payment_intents SET status='succeeded' WHERE id=$PID"
for k in r1 r1; do
  echo "--- POST /payments/$PID/refunds amount=3000 Idempotency-Key=$k"
  curl -s -w "\nHTTP %{http_code}\n" -X POST "$APP_URL/payments/$PID/refunds" -H "Authorization: Bearer $KEY" \
    -H "Idempotency-Key: $k" -d '{"amount":3000,"reason":"probe"}'
done
echo "--- refunds table after"
psql_q -c "SELECT id,payment_intent_id,amount,status,provider_refund_id,failure_reason FROM refunds"
echo "--- ledger rows for payment $PID"
psql_q -c "SELECT account,amount FROM ledger_entries WHERE payment_intent_id=$PID"
echo "--- mock provider calls"
curl -s "$APP_URL/metrics" | grep -E "^gateway_mock"
echo "--- app log (refund lines)"
docker logs lt_gateway_app 2>&1 | grep -E 'refund' | tail -4
exit 0
