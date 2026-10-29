// G9 rate limits at DEFAULT settings.
//   A) per-merchant limiter (RATE_LIMIT_PER_MERCHANT, default 600/min, Redis fixed
//      calendar-minute window): one merchant, RATE req/s for 50 s, started by
//      run.sh just after a minute boundary so all requests share one window.
//   B) per-IP limiter (200/min, hard-coded): 300 GET /health from one client IP.
//   C) same as B but each request claims a different X-Real-IP. With
//      no trusted proxies configured (default) the header must be ignored.
import http from 'k6/http';
import { Counter } from 'k6/metrics';
import { APP_URL, STATE, TREND_STATS, authHeaders, fakeIP, recordStatus, touch, writeSummary } from './lib/common.js';

const RATE = parseInt(__ENV.RATE || '20', 10);
const merchant = JSON.parse(open(`${STATE}/merchants.json`))[0];

const c = {};
for (const n of ['a_201', 'a_429_rate_limit_exceeded', 'a_429_other', 'a_other',
  'b_200', 'b_429', 'b_other', 'c_200', 'c_429', 'c_other']) c[n] = new Counter(`g9${n}`);
let first = true;

// PART=merchant runs A (stack with the per-IP limit raised, so it cannot mask
// the per-merchant limit); PART=ip runs B and C (stack with the default per-IP limit).
const ALL_SCENARIOS = {
  a_merchant: { executor: 'constant-arrival-rate', rate: RATE, timeUnit: '1s', duration: '50s',
    preAllocatedVUs: 40, maxVUs: 200, exec: 'merchantScenario', startTime: '0s', gracefulStop: '10s' },
  b_ip_fixed: { executor: 'per-vu-iterations', vus: 1, iterations: 300, exec: 'ipFixed', startTime: '0s' },
  c_ip_rotating: { executor: 'per-vu-iterations', vus: 1, iterations: 300, exec: 'ipRotating', startTime: '65s' },
};
const PART = __ENV.PART || 'merchant';
const scenarios = PART === 'ip'
  ? { b_ip_fixed: ALL_SCENARIOS.b_ip_fixed, c_ip_rotating: ALL_SCENARIOS.c_ip_rotating }
  : { a_merchant: ALL_SCENARIOS.a_merchant };

export const options = {
  scenarios,
  summaryTrendStats: TREND_STATS,
  thresholds: PART === 'ip' ? {} : {
    'http_req_duration{scenario:a_merchant,status:201}': ['max>=0'],
    'http_req_duration{scenario:a_merchant,status:429}': ['max>=0'],
  },
};

function init() {
  if (first) { first = false; touch(Object.values(c)); }
}

export function merchantScenario() {
  init();
  const r = http.post(`${APP_URL}/payments`,
    JSON.stringify({ amount: 1000, currency: 'USD', provider: 'stripe' }),
    { headers: authHeaders(merchant.api_key, { 'Idempotency-Key': `g9-${__VU}-${__ITER}-${Date.now()}` }) });
  recordStatus(r);
  if (r.status === 201) c.a_201.add(1);
  else if (r.status === 429) {
    let code = '';
    try { code = r.json('error_obj.code'); } catch (_) { /* not JSON */ }
    (code === 'rate_limit_exceeded' ? c.a_429_rate_limit_exceeded : c.a_429_other).add(1);
  } else c.a_other.add(1);
}

export function ipFixed() {
  init();
  const r = http.get(`${APP_URL}/health`);
  recordStatus(r);
  (r.status === 200 ? c.b_200 : r.status === 429 ? c.b_429 : c.b_other).add(1);
}

export function ipRotating() {
  init();
  const r = http.get(`${APP_URL}/health`, { headers: { 'X-Real-IP': fakeIP() } });
  recordStatus(r);
  (r.status === 200 ? c.c_200 : r.status === 429 ? c.c_429 : c.c_other).add(1);
}

export function handleSummary(data) {
  return writeSummary(data);
}
