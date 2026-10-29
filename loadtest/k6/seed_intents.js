// Setup helper (not a measured scenario): create N payment intents spread
// over the merchants and save {id, merchant_id, provider_tx_id, amount} to
// .state/intents.json for the webhook scenarios.
//
//   k6 run -e N=1000 -e CURRENCY=USD -e PROVIDER=stripe seed_intents.js
import http from 'k6/http';
import { APP_URL, RUN_ID, STATE, TREND_STATS, authHeaders, recordStatus, writeSummary } from './lib/common.js';

const N = parseInt(__ENV.N || '1000', 10);
const PROVIDER = __ENV.PROVIDER || 'stripe';
const CURRENCY = __ENV.CURRENCY || 'USD';
const MERCHANT_LIMIT = parseInt(__ENV.MERCHANTS || '0', 10); // 0 = all
const FIXED_AMOUNT = parseInt(__ENV.AMOUNT || '0', 10); // 0 = 1000 + i
const BATCH = 20;
const MERCHANT_INDEX = __ENV.MERCHANT_INDEX; // use only this merchant
let merchants = JSON.parse(open(`${STATE}/merchants.json`));
if (MERCHANT_LIMIT > 0) merchants = merchants.slice(0, MERCHANT_LIMIT);
if (MERCHANT_INDEX !== undefined) merchants = [merchants[parseInt(MERCHANT_INDEX, 10)]];

export const options = { vus: 1, iterations: 1, setupTimeout: '20m', summaryTrendStats: TREND_STATS };

export function setup() {
  const intents = [];
  for (let start = 0; start < N; start += BATCH) {
    const reqs = [];
    for (let i = start; i < Math.min(N, start + BATCH); i++) {
      const m = merchants[i % merchants.length];
      reqs.push({
        method: 'POST',
        url: `${APP_URL}/payments`,
        body: JSON.stringify({ amount: FIXED_AMOUNT || 1000 + i, currency: CURRENCY, provider: PROVIDER }),
        params: { headers: authHeaders(m.api_key, { 'Idempotency-Key': `seed-${RUN_ID}-${i}` }), timeout: '60s' },
      });
    }
    for (const res of http.batch(reqs)) {
      recordStatus(res);
      if (res.status !== 201) throw new Error(`seed failed: ${res.status} ${res.body}`);
      const pi = res.json('data.payment_intent');
      intents.push({ id: pi.id, merchant_id: pi.merchant_id, tx: pi.provider_tx_id, amount: pi.amount });
    }
  }
  return { intents };
}

export default function () {}

export function handleSummary(data) {
  return writeSummary(data, { [`${STATE}/intents.json`]: JSON.stringify(data.setup_data.intents) });
}
