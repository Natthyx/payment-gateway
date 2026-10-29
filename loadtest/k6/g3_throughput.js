// G3 throughput curve: POST /payments with distinct idempotency keys at a
// constant arrival rate. One rate per k6 run.
//
//   k6 run -e RATE=50 -e DURATION=60s g3_throughput.js
import http from 'k6/http';
import exec from 'k6/execution';
import { APP_URL, RUN_ID, STATE, TREND_STATS, authHeaders, recordStatus, writeSummary } from './lib/common.js';

const RATE = parseInt(__ENV.RATE || '50', 10);
const DURATION = __ENV.DURATION || '60s';
const MAX_VUS = parseInt(__ENV.MAX_VUS || '600', 10);
const merchants = JSON.parse(open(`${STATE}/merchants.json`));

export const options = {
  scenarios: {
    curve: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.min(MAX_VUS, Math.max(10, RATE * 2)),
      maxVUs: MAX_VUS,
      gracefulStop: '40s',
    },
  },
  summaryTrendStats: TREND_STATS,
};

export default function () {
  const i = exec.scenario.iterationInTest;
  const m = merchants[i % merchants.length];
  const res = http.post(
    `${APP_URL}/payments`,
    JSON.stringify({ amount: 1000 + (i % 9000), currency: 'USD', provider: 'stripe' }),
    { headers: authHeaders(m.api_key, { 'Idempotency-Key': `g3-${RUN_ID}-${RATE}-${i}` }), timeout: '35s' },
  );
  recordStatus(res);
}

export function handleSummary(data) {
  return writeSummary(data);
}
