#!/usr/bin/env python3
"""G6: compute retry gaps from the receiver's attempt log, and (for the
fast-forwarded dead-letter endpoint) the backoff the gateway scheduled.

usage: g6_backoff.py <receiver_full.json> [<deadletter_snapshots.csv>]

Flaky endpoint (real time): gap(n) = arrival(n+1) - arrival(n). Expected per
code: 30*2^(n-1) s plus up to one 5 s worker tick plus send time.
Dead endpoint (fast-forwarded): scheduled(n) = next_attempt_at after attempt n
minus arrival(n), read from DB snapshots taken before each fast-forward.
"""
import csv, json, statistics, sys
from datetime import datetime, timezone

def ts(s):
    # Go RFC3339Nano in UTC, e.g. 2026-10-28T17:20:01.123456789Z
    s = s.rstrip('Z')
    if '.' in s:
        a, b = s.split('.')
        s = a + '.' + b[:6]
    return datetime.fromisoformat(s).replace(tzinfo=timezone.utc).timestamp()

full = json.load(open(sys.argv[1]))
keys = full.get('keys') or []

gaps = {}
for k in keys:
    atts = [a for a in k['attempts'] if a['mode'] == 'flaky']
    for n in range(1, len(atts)):
        gaps.setdefault(n, []).append(ts(atts[n]['at']) - ts(atts[n - 1]['at']))
print('== flaky endpoint: real-time gap between attempt n and n+1 (seconds)')
print(f"{'n':>2} {'expected_backoff':>16} {'samples':>7} {'min':>8} {'median':>8} {'max':>8}")
for n in sorted(gaps):
    g = gaps[n]
    print(f"{n:>2} {30 * 2 ** (n - 1):>16} {len(g):>7} {min(g):>8.2f} {statistics.median(g):>8.2f} {max(g):>8.2f}")

if len(sys.argv) > 2:
    arrivals = {}
    for k in keys:
        atts = [a for a in k['attempts'] if a['mode'] == 'dead']
        if atts:
            arrivals[k['key']] = [ts(a['at']) for a in atts]
    sched = {}
    seen = set()
    with open(sys.argv[2]) as f:
        for row in csv.DictReader(f):
            did, n, status = row['id'], int(row['attempts']), row['status']
            if status != 'pending' or n == 0 or (did, n) in seen:
                continue
            seen.add((did, n))
            arr = arrivals.get(f'dlv_{did}')
            if not arr or len(arr) < n:
                continue
            sched.setdefault(n, []).append(float(row['next_attempt_epoch']) - arr[n - 1])
    print('\n== dead endpoint: backoff scheduled by the gateway after attempt n (next_attempt_at - arrival of attempt n, seconds)')
    print(f"{'n':>2} {'expected':>8} {'samples':>7} {'min':>8} {'median':>8} {'max':>8}")
    for n in sorted(sched):
        g = sched[n]
        print(f"{n:>2} {30 * 2 ** (n - 1):>8} {len(g):>7} {min(g):>8.2f} {statistics.median(g):>8.2f} {max(g):>8.2f}")
    per_key = {k: len(v) for k, v in arrivals.items()}
    print('\n== dead endpoint: attempts received per delivery:', json.dumps(sorted(set(per_key.values()))), 'over', len(per_key), 'deliveries')
