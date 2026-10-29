// Shared helpers for the k6 scenarios.
import crypto from 'k6/crypto';
import { Counter } from 'k6/metrics';

export const APP_URL = __ENV.APP_URL || 'http://127.0.0.1:18080';
export const APP2_URL = __ENV.APP2_URL || 'http://127.0.0.1:18081';
export const STATE = __ENV.STATE;
export const RUN_ID = __ENV.RUN_ID || `${Date.now()}`;

export const TREND_STATS = ['min', 'med', 'avg', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'];

// The gateway's per-IP limiter (200/min, hard-coded) keys on the address that
// chi's RealIP middleware takes from X-Real-IP. One load generator would be
// one IP, so each request claims a random 10.x.y.z address. This relies on the
// gateway trusting client-supplied headers (FINDINGS.md R8).
export function fakeIP() {
  const r = () => Math.floor(Math.random() * 256);
  return `10.${r()}.${r()}.${1 + Math.floor(Math.random() * 254)}`;
}

export function jsonHeaders(extra) {
  return Object.assign({ 'Content-Type': 'application/json', 'X-Real-IP': fakeIP() }, extra || {});
}

export function authHeaders(apiKey, extra) {
  return jsonHeaders(Object.assign({ Authorization: `Bearer ${apiKey}` }, extra || {}));
}

// Status-code distribution. k6 has no built-in per-status breakdown, so each
// response increments one counter; status 0 means no HTTP response (timeout,
// connection error).
const STATUS_CODES = [200, 201, 204, 400, 401, 404, 409, 429, 500, 502, 503, 504];
const statusCounters = {};
for (const c of STATUS_CODES) statusCounters[c] = new Counter(`status_${c}`);
const statusOther = new Counter('status_other');
const statusNone = new Counter('status_0_no_response');

// k6 omits metrics that never got a sample; adding 0 once per VU makes zero
// counts show up explicitly in the summary.
let touched = false;
export function touch(counters) {
  for (const c of counters) c.add(0);
}

export function recordStatus(res) {
  if (!touched) {
    touched = true;
    touch(Object.values(statusCounters).concat([statusOther, statusNone]));
  }
  if (!res || res.status === 0) statusNone.add(1);
  else if (statusCounters[res.status]) statusCounters[res.status].add(1);
  else statusOther.add(1);
}

// Stripe-style signature exactly as internal/provider/stripe VerifyWebhook
// checks it: "t=<unix>,v1=hex(HMAC-SHA256(secret, t + "." + body))".
export function stripeSignature(body, secret, t) {
  const ts = t || Math.floor(Date.now() / 1000);
  return `t=${ts},v1=${crypto.hmac('sha256', secret, `${ts}.${body}`, 'hex')}`;
}

// Deterministic PRNG (mulberry32) so delivery order and multiplicity are
// reproducible for a given seed.
export function prng(seed) {
  let a = seed >>> 0;
  return function () {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function shuffle(arr, rand) {
  for (let i = arr.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    [arr[i], arr[j]] = [arr[j], arr[i]];
  }
  return arr;
}

// Compact, human-readable summary: every metric with its values.
function textSummary(data) {
  const lines = [];
  const st = data.state || {};
  lines.push(`test duration: ${(st.testRunDurationMs / 1000).toFixed(1)} s`);
  for (const name of Object.keys(data.metrics).sort()) {
    const m = data.metrics[name];
    const v = Object.entries(m.values)
      .map(([k, x]) => `${k}=${typeof x === 'number' ? +x.toFixed(3) : x}`)
      .join(' ');
    lines.push(`${name.padEnd(48)} ${v}`);
  }
  return lines.join('\n') + '\n';
}

// handleSummary helper: writes the raw k6 summary JSON to SUMMARY_OUT (with
// setup_data removed so API keys never land in results/) plus extra files.
export function writeSummary(data, extraFiles) {
  const out = {};
  const clean = Object.assign({}, data);
  delete clean.setup_data;
  if (__ENV.SUMMARY_OUT) out[__ENV.SUMMARY_OUT] = JSON.stringify(clean, null, 2);
  out.stdout = textSummary(data);
  return Object.assign(out, extraFiles || {});
}
