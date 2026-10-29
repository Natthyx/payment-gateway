-- G6 outbound deliveries, per endpoint mode (flaky / dead).
\pset pager off
\echo '== deliveries by endpoint and status'
SELECT split_part(e.url, '/hook/', 2) AS endpoint, d.status, count(*), min(d.attempts) AS min_attempts, max(d.attempts) AS max_attempts
FROM outbound_webhook_deliveries d JOIN merchant_webhook_endpoints e ON e.id = d.endpoint_id
GROUP BY 1, 2 ORDER BY 1, 2;

\echo '== attempt-count distribution (flaky endpoint)'
SELECT d.attempts, d.status, count(*)
FROM outbound_webhook_deliveries d JOIN merchant_webhook_endpoints e ON e.id = d.endpoint_id
WHERE e.url LIKE '%/hook/flaky' GROUP BY 1, 2 ORDER BY 1, 2;

\echo '== flaky drain time from DB clocks (enqueue of first delivery -> last delivered_at)'
SELECT count(*) AS deliveries,
       count(*) FILTER (WHERE d.status = 'delivered') AS delivered,
       round(EXTRACT(EPOCH FROM max(d.delivered_at) - min(d.created_at))::numeric, 1) AS first_enqueue_to_last_delivered_s,
       round(EXTRACT(EPOCH FROM percentile_cont(0.5) WITHIN GROUP (ORDER BY d.delivered_at - d.created_at))::numeric, 1) AS median_enqueue_to_delivered_s,
       round(EXTRACT(EPOCH FROM max(d.delivered_at - d.created_at))::numeric, 1) AS max_enqueue_to_delivered_s
FROM outbound_webhook_deliveries d JOIN merchant_webhook_endpoints e ON e.id = d.endpoint_id
WHERE e.url LIKE '%/hook/flaky';

\echo '== one delivery row per succeeded payment and endpoint (duplicates must be 0)'
SELECT count(*) AS payments_with_more_than_one_delivery FROM (
  SELECT d.endpoint_id, (d.payload->'data'->>'id') AS pi FROM outbound_webhook_deliveries d GROUP BY 1, 2 HAVING count(*) > 1) x;
