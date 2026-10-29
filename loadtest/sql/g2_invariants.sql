-- G2 invariants. psql variables: expected_keys (K).
\pset pager off
\echo '== intents created by G2 (expected = K)'
SELECT count(*) AS g2_intents, :expected_keys AS expected,
       count(*) = :expected_keys AS ok
FROM payment_intents WHERE idempotency_key LIKE 'g2-%';

\echo '== duplicate intents per (merchant, idempotency_key) — must be 0'
SELECT count(*) AS duplicate_groups FROM (
  SELECT merchant_id, idempotency_key FROM payment_intents
  WHERE idempotency_key LIKE 'g2-%' GROUP BY 1, 2 HAVING count(*) > 1) d;

\echo '== intents whose amount came from the different-body request — must be 0'
\echo '   (each key i was first sent with amount 1000+i; the probe used 1001+i)'
SELECT count(*) AS intents_with_probe_amount
FROM payment_intents
WHERE idempotency_key LIKE 'g2-%'
  AND amount <> 1000 + split_part(idempotency_key, '-', 3)::bigint;

\echo '== status distribution'
SELECT status, count(*) FROM payment_intents WHERE idempotency_key LIKE 'g2-%' GROUP BY 1 ORDER BY 1;

\echo '== provider charge calls vs intents (mock counter is read separately from /metrics)'
