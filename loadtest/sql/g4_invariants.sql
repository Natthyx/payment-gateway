-- G4 / G8 invariants after the inbox has drained.
-- psql variables: expected_intents, expected_events (unique event ids sent).
\pset pager off

\echo '== I1 ledger balanced overall (sum of all entries = 0)'
SELECT COALESCE(sum(amount), 0) AS ledger_sum, COALESCE(sum(amount), 0) = 0 AS ok FROM ledger_entries;

\echo '== I2 ledger balanced per payment intent (groups with sum <> 0 must be 0)'
SELECT count(*) AS unbalanced_intents FROM (
  SELECT payment_intent_id FROM ledger_entries GROUP BY 1 HAVING sum(amount) <> 0) x;

\echo '== I3 exactly one set of ledger entries (3 legs) per succeeded intent'
SELECT count(*) FILTER (WHERE n IS NULL) AS succeeded_without_ledger,
       count(*) FILTER (WHERE n <> 3)     AS succeeded_with_wrong_leg_count,
       count(*)                           AS succeeded_intents
FROM payment_intents pi
LEFT JOIN (SELECT payment_intent_id, count(*) AS n FROM ledger_entries GROUP BY 1) l ON l.payment_intent_id = pi.id
WHERE pi.status = 'succeeded';

\echo '== I4 no ledger entries for intents that are not succeeded'
SELECT count(DISTINCT le.payment_intent_id) AS non_succeeded_with_ledger
FROM ledger_entries le JOIN payment_intents pi ON pi.id = le.payment_intent_id
WHERE pi.status <> 'succeeded';

\echo '== I5 intent statuses consistent with webhooks (every seeded intent got a success event;'
\echo '   warm-up intents, key g3-warm-*, got none and must stay processing)'
SELECT CASE WHEN idempotency_key LIKE 'seed-%' THEN 'seeded' ELSE 'warm-up' END AS kind, status, count(*)
FROM payment_intents GROUP BY 1, 2 ORDER BY 1, 2;
SELECT count(*) FILTER (WHERE status = 'succeeded') AS seeded_succeeded, :expected_intents AS expected,
       count(*) FILTER (WHERE status = 'succeeded') = :expected_intents AS ok
FROM payment_intents WHERE idempotency_key LIKE 'seed-%';

\echo '== I6 each provider event stored once; none processed twice'
SELECT count(*) AS webhook_rows, count(DISTINCT (provider, provider_event_id)) AS distinct_events,
       :expected_events AS expected_events,
       count(*) = :expected_events AND count(DISTINCT (provider, provider_event_id)) = :expected_events AS ok
FROM webhooks;
SELECT status, count(*), max(attempts) AS max_attempts FROM webhooks GROUP BY 1 ORDER BY 1;

\echo '== I7 ledger leg amounts match the intent (provider leg = -amount)'
SELECT count(*) AS provider_leg_mismatch
FROM ledger_entries le JOIN payment_intents pi ON pi.id = le.payment_intent_id
WHERE le.account LIKE 'provider:%' AND le.account NOT LIKE '%:refund:%' AND le.amount <> -pi.amount;

\echo '== Drain timing from DB clocks'
SELECT count(*) FILTER (WHERE status = 'processed') AS processed,
       round(EXTRACT(EPOCH FROM max(created_at) - min(created_at))::numeric, 3) AS ingest_span_s,
       round(EXTRACT(EPOCH FROM max(processed_at) - max(created_at))::numeric, 3) AS drain_after_last_ingest_s,
       round(EXTRACT(EPOCH FROM max(processed_at) - min(created_at))::numeric, 3) AS first_ingest_to_last_processed_s,
       round(EXTRACT(EPOCH FROM max(processed_at) - min(processed_at))::numeric, 3) AS processing_span_s,
       round((count(*) FILTER (WHERE status = 'processed')
              / NULLIF(EXTRACT(EPOCH FROM max(processed_at) - min(processed_at)), 0))::numeric, 1) AS processed_per_s
FROM webhooks;
