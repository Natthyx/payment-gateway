// G2 idempotency storm.
// For each of K keys, send C identical POST /payments in parallel (http.batch),
// then one more request with the same key and a DIFFERENT body.
//
//   k6 run -e K=500 -e C=10 -e VUS=20 g2_idempotency.js
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import { APP_URL, RUN_ID, STATE, TREND_STATS, authHeaders, recordStatus, touch, writeSummary } from './lib/common.js';

const K = parseInt(__ENV.K || '500', 10);
const C = parseInt(__ENV.C || '10', 10);
const VUS = parseInt(__ENV.VUS || '20', 10);
const merchants = JSON.parse(open(`${STATE}/merchants.json`));

// Per-key outcomes of the C identical requests.
const keysAll201Identical = new Counter('g2_keys_all_201_identical_body');
const keysSomeConflict = new Counter('g2_keys_with_409');
const keysOtherError = new Counter('g2_keys_with_non_201_non_409');
const keysMixedIntentIds = new Counter('g2_keys_with_different_intent_ids'); // must stay 0
const keysNo201 = new Counter('g2_keys_with_no_201');
const conflictCodes = new Counter('g2_409_code_idempotency_conflict');
const inUseCodes = new Counter('g2_409_code_idempotency_key_in_use');
// Same key + different body.
const diff201SameIntent = new Counter('g2_diffbody_201_replayed_original');
const diff201NewIntent = new Counter('g2_diffbody_201_new_intent'); // must stay 0
const diff409 = new Counter('g2_diffbody_409_idempotency_conflict');
const diffOther = new Counter('g2_diffbody_other_status');

export const options = {
  scenarios: {
    storm: { executor: 'shared-iterations', vus: Math.min(VUS, K), iterations: K, maxDuration: '20m' },
  },
  summaryTrendStats: TREND_STATS,
  thresholds: {
    // Declared only so k6 reports these tagged sub-metrics.
    'http_req_duration{kind:identical}': ['max>=0'],
    'http_req_duration{kind:diffbody}': ['max>=0'],
  },
};

const ALL = [keysAll201Identical, keysSomeConflict, keysOtherError, keysMixedIntentIds, keysNo201,
  conflictCodes, inUseCodes, diff201SameIntent, diff201NewIntent, diff409, diffOther];
let first = true;

export default function () {
  if (first) { first = false; touch(ALL); }
  const i = exec.scenario.iterationInTest;
  const m = merchants[i % merchants.length];
  const key = `g2-${RUN_ID}-${i}`;
  const amount = 1000 + i;
  const body = JSON.stringify({ amount, currency: 'USD', provider: 'stripe' });

  const reqs = [];
  for (let c = 0; c < C; c++) {
    reqs.push({
      method: 'POST',
      url: `${APP_URL}/payments`,
      body,
      params: { headers: authHeaders(m.api_key, { 'Idempotency-Key': key }), tags: { kind: 'identical' }, timeout: '60s' },
    });
  }
  const res = http.batch(reqs);

  const ids = new Set();
  const bodies201 = new Set();
  let n201 = 0, n409 = 0, nOther = 0;
  for (const r of res) {
    recordStatus(r);
    if (r.status === 201) {
      n201++;
      bodies201.add(r.body);
      ids.add(r.json('data.payment_intent.id'));
    } else if (r.status === 409) {
      n409++;
      const code = r.json('error_obj.code');
      if (code === 'idempotency_conflict') conflictCodes.add(1);
      else if (code === 'idempotency_key_in_use') inUseCodes.add(1);
    } else {
      nOther++;
    }
  }
  if (n201 === C && bodies201.size === 1) keysAll201Identical.add(1);
  if (n409 > 0) keysSomeConflict.add(1);
  if (nOther > 0) keysOtherError.add(1);
  if (ids.size > 1) keysMixedIntentIds.add(1);
  if (n201 === 0) keysNo201.add(1);

  // Same key, different amount: spec expects 409 idempotency_conflict.
  const diff = http.post(
    `${APP_URL}/payments`,
    JSON.stringify({ amount: amount + 1, currency: 'USD', provider: 'stripe' }),
    { headers: authHeaders(m.api_key, { 'Idempotency-Key': key }), tags: { kind: 'diffbody' }, timeout: '60s' },
  );
  recordStatus(diff);
  if (diff.status === 201) {
    const id = diff.json('data.payment_intent.id');
    if (ids.has(id) && diff.json('data.payment_intent.amount') === amount) diff201SameIntent.add(1);
    else diff201NewIntent.add(1);
  } else if (diff.status === 409 && diff.json('error_obj.code') === 'idempotency_conflict') diff409.add(1);
  else diffOther.add(1);
}

export function handleSummary(data) {
  return writeSummary(data);
}
