// G4 / G8 webhook → ledger. Deliver a Stripe-signed payment_intent.succeeded
// event for every seeded intent. Each event is delivered 1–5 times; the copies
// of one event go out concurrently (http.batch) and events are processed in a
// seeded random order by many VUs. A fraction of intents also get a SECOND,
// distinct event id (a provider re-sending under a new id), which must be
// absorbed by the status gate rather than by event-id dedupe.
//
// TWO_INSTANCES=1 spreads copies randomly across APP_URL and APP2_URL (G8).
//
//   k6 run -e SEED=1 -e VUS=50 g4_webhooks.js
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import {
  APP_URL, APP2_URL, RUN_ID, STATE, TREND_STATS, fakeIP, prng, recordStatus, shuffle, stripeSignature, touch, writeSummary,
} from './lib/common.js';

const SECRET = __ENV.STRIPE_WEBHOOK_SECRET;
const SEED = parseInt(__ENV.SEED || '1', 10);
const VUS = parseInt(__ENV.VUS || '50', 10);
const EXTRA_EVENT_FRACTION = parseFloat(__ENV.EXTRA_EVENT_FRACTION || '0.05');
const TWO = __ENV.TWO_INSTANCES === '1';
const intents = JSON.parse(open(`${STATE}/intents.json`));

// Build the event plan once (identical in every VU because the PRNG is seeded).
const rand = prng(SEED);
const events = [];
for (const pi of intents) {
  events.push({ evt: `evt_${RUN_ID}_${pi.id}`, pi, copies: 1 + Math.floor(rand() * 5) });
  if (rand() < EXTRA_EVENT_FRACTION) {
    events.push({ evt: `evt_${RUN_ID}_${pi.id}_dup`, pi, copies: 1 + Math.floor(rand() * 5) });
  }
}
shuffle(events, rand);
const plannedDeliveries = events.reduce((n, e) => n + e.copies, 0);

const deliveries = new Counter('g4_deliveries_sent');
const uniqueEvents = new Counter('g4_unique_events_sent');
const non200 = new Counter('g4_non_200_responses');
const toApp1 = new Counter('g4_deliveries_to_app1');
const toApp2 = new Counter('g4_deliveries_to_app2');
let first = true;

export const options = {
  scenarios: {
    deliver: { executor: 'shared-iterations', vus: Math.min(VUS, events.length), iterations: events.length, maxDuration: '20m' },
  },
  summaryTrendStats: TREND_STATS,
};

export default function () {
  if (first) { first = false; touch([deliveries, uniqueEvents, non200, toApp1, toApp2]); }
  const e = events[exec.scenario.iterationInTest];
  const body = JSON.stringify({
    id: e.evt,
    type: 'payment_intent.succeeded',
    created: Math.floor(Date.now() / 1000),
    data: { object: { id: e.pi.tx, amount: e.pi.amount, currency: 'usd' } },
  });
  const reqs = [];
  for (let c = 0; c < e.copies; c++) {
    const second = TWO && Math.random() < 0.5;
    (second ? toApp2 : toApp1).add(1);
    reqs.push({
      method: 'POST',
      url: `${second ? APP2_URL : APP_URL}/webhooks/stripe`,
      body,
      params: { headers: { 'Stripe-Signature': stripeSignature(body, SECRET), 'X-Real-IP': fakeIP() }, timeout: '30s' },
    });
  }
  for (const r of http.batch(reqs)) {
    recordStatus(r);
    deliveries.add(1);
    if (r.status !== 200) non200.add(1);
  }
  uniqueEvents.add(1);
}

export function handleSummary(data) {
  const plan = {
    intents: intents.length,
    unique_events: events.length,
    extra_event_ids: events.length - intents.length,
    planned_deliveries: plannedDeliveries,
    seed: SEED,
    two_instances: TWO,
  };
  const out = writeSummary(data, {});
  if (__ENV.PLAN_OUT) out[__ENV.PLAN_OUT] = JSON.stringify(plan, null, 2);
  return out;
}
