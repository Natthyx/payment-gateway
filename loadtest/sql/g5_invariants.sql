-- G5 invariants: total refunded must never exceed the payment; ledger balanced.
\pset pager off

\echo '== per payment: refunds by status, total succeeded vs payment amount'
SELECT pi.id AS payment_id, pi.amount, pi.status AS payment_status,
       count(r.*) FILTER (WHERE r.status = 'succeeded') AS refunds_succeeded,
       count(r.*) FILTER (WHERE r.status = 'pending')   AS refunds_pending,
       count(r.*) FILTER (WHERE r.status = 'failed')    AS refunds_failed,
       COALESCE(sum(r.amount) FILTER (WHERE r.status = 'succeeded'), 0) AS refunded_total,
       COALESCE(sum(r.amount) FILTER (WHERE r.status = 'succeeded'), 0) <= pi.amount AS ok_not_over_refunded
FROM payment_intents pi LEFT JOIN refunds r ON r.payment_intent_id = pi.id
GROUP BY pi.id ORDER BY pi.id;

\echo '== INVARIANT: payments where succeeded refunds exceed the amount (must be 0)'
SELECT count(*) AS over_refunded_payments FROM (
  SELECT pi.id FROM payment_intents pi JOIN refunds r ON r.payment_intent_id = pi.id AND r.status = 'succeeded'
  GROUP BY pi.id, pi.amount HAVING sum(r.amount) > pi.amount) x;

\echo '== INVARIANT: refund rows per idempotency key (must be 1 each)'
SELECT idempotency_key, count(*) FROM refunds WHERE idempotency_key LIKE 'g5c-%' GROUP BY 1;

\echo '== INVARIANT: ledger balanced overall and per payment'
SELECT COALESCE(sum(amount), 0) AS ledger_sum FROM ledger_entries;
SELECT count(*) AS unbalanced_payments FROM (
  SELECT payment_intent_id FROM ledger_entries GROUP BY 1 HAVING sum(amount) <> 0) x;

\echo '== ledger refund legs vs succeeded refunds, per payment'
SELECT pi.id AS payment_id,
       (SELECT count(*) FROM refunds r WHERE r.payment_intent_id = pi.id AND r.status = 'succeeded') AS refunds_succeeded,
       (SELECT count(*) FROM ledger_entries le WHERE le.payment_intent_id = pi.id AND le.account LIKE 'merchant:%:refund:%') AS refund_debit_legs,
       -(SELECT COALESCE(sum(amount), 0) FROM ledger_entries le WHERE le.payment_intent_id = pi.id AND le.account LIKE 'merchant:%:refund:%') AS ledger_refunded_total,
       (SELECT COALESCE(sum(amount), 0) FROM ledger_entries le WHERE le.payment_intent_id = pi.id AND le.account LIKE 'merchant:%') AS merchant_net_for_payment
FROM payment_intents pi ORDER BY pi.id;

\echo '== refund rows (all)'
SELECT r.id, r.payment_intent_id, r.idempotency_key, r.amount, r.status, left(COALESCE(r.failure_reason, ''), 60) AS failure_reason
FROM refunds r ORDER BY r.payment_intent_id, r.id;
