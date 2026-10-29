// G1 setup: create M merchants through POST /merchants and save their API keys
// to .state/merchants.json (gitignored). Keys never go into results/.
//
//   k6 run -e M=20 g1_merchants.js
import http from 'k6/http';
import { check } from 'k6';
import { APP_URL, RUN_ID, TREND_STATS, jsonHeaders, recordStatus, writeSummary } from './lib/common.js';

const M = parseInt(__ENV.M || '20', 10);
const BATCH = 5;

export const options = {
  vus: 1,
  iterations: 1,
  setupTimeout: '5m',
  summaryTrendStats: TREND_STATS,
};

export function setup() {
  const merchants = [];
  for (let start = 0; start < M; start += BATCH) {
    const reqs = [];
    for (let i = start; i < Math.min(M, start + BATCH); i++) {
      reqs.push({
        method: 'POST',
        url: `${APP_URL}/merchants`,
        body: JSON.stringify({ name: `lt-merchant-${i}`, email: `lt-${RUN_ID}-${i}@loadtest.invalid` }),
        params: { headers: jsonHeaders() },
      });
    }
    for (const res of http.batch(reqs)) {
      recordStatus(res);
      const ok = check(res, { 'merchant created (201)': (r) => r.status === 201 });
      if (ok) {
        const d = res.json('data');
        merchants.push({ id: d.merchant.id, api_key: d.api_key });
      }
    }
  }
  if (merchants.length !== M) throw new Error(`created ${merchants.length}/${M} merchants`);
  return { merchants };
}

export default function () {}

export function handleSummary(data) {
  return writeSummary(data, {
    [`${__ENV.STATE}/merchants.json`]: JSON.stringify(data.setup_data.merchants),
  });
}
