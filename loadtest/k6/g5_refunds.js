// G5 concurrent refunds. Needs 4 succeeded intents of amount 10000 in
// .state/intents.json (run.sh seeds them and delivers success webhooks).
//
//   a) 20 parallel refunds, distinct keys, distinct amounts 1000..1019 (sum 20190 > 10000)
//   b) 20 parallel refunds, distinct keys, same amount 3000          (sum 60000 > 10000)
//   c) 10 parallel refunds, SAME key, amount 2000                     (exactly one refund expected)
//   d) 2 sequential refunds 6000 then 6001, no concurrency            (sum 12001 > 10000)
import http from 'k6/http';
import { Counter } from 'k6/metrics';
import { APP_URL, RUN_ID, STATE, TREND_STATS, authHeaders, recordStatus, touch, writeSummary } from './lib/common.js';

const intents = JSON.parse(open(`${STATE}/intents.json`));
const keyByMerchant = {};
for (const m of JSON.parse(open(`${STATE}/merchants.json`))) keyByMerchant[m.id] = m.api_key;

const outcome = {};
for (const t of ['a', 'b', 'c', 'd']) {
  for (const s of ['201', '409', '500', 'other']) outcome[`${t}_${s}`] = new Counter(`g5${t}_status_${s}`);
}

export const options = {
  vus: 1,
  iterations: 1,
  summaryTrendStats: TREND_STATS,
  thresholds: {
    'http_req_duration{sub:a}': ['max>=0'],
    'http_req_duration{sub:b}': ['max>=0'],
    'http_req_duration{sub:c}': ['max>=0'],
    'http_req_duration{sub:d}': ['max>=0'],
  },
};

function refundReq(pi, key, amount, sub) {
  return {
    method: 'POST',
    url: `${APP_URL}/payments/${pi.id}/refunds`,
    body: JSON.stringify({ amount, reason: `g5${sub}` }),
    params: { headers: authHeaders(keyByMerchant[pi.merchant_id], { 'Idempotency-Key': key }), tags: { sub }, timeout: '60s' },
  };
}

function record(sub, res) {
  recordStatus(res);
  const s = ['201', '409', '500'].includes(String(res.status)) ? String(res.status) : 'other';
  outcome[`${sub}_${s}`].add(1);
}

export default function () {
  touch(Object.values(outcome));
  if (intents.length < 4) throw new Error('need 4 seeded intents');
  const [pa, pb, pc, pd] = intents;

  const a = [];
  for (let i = 0; i < 20; i++) a.push(refundReq(pa, `g5a-${RUN_ID}-${i}`, 1000 + i, 'a'));
  const b = [];
  for (let i = 0; i < 20; i++) b.push(refundReq(pb, `g5b-${RUN_ID}-${i}`, 3000, 'b'));
  const c = [];
  for (let i = 0; i < 10; i++) c.push(refundReq(pc, `g5c-${RUN_ID}`, 2000, 'c'));

  // a, b and c run at the same time: 50 concurrent refund requests.
  const res = http.batch(a.concat(b, c));
  res.forEach((r, i) => record(i < 20 ? 'a' : i < 40 ? 'b' : 'c', r));

  for (const [i, amt] of [[0, 6000], [1, 6001]]) {
    const r = http.post(refundReq(pd, `g5d-${RUN_ID}-${i}`, amt, 'd').url,
      JSON.stringify({ amount: amt, reason: 'g5d' }),
      { headers: authHeaders(keyByMerchant[pd.merchant_id], { 'Idempotency-Key': `g5d-${RUN_ID}-${i}` }), tags: { sub: 'd' } });
    record('d', r);
  }
}

export function handleSummary(data) {
  return writeSummary(data);
}
