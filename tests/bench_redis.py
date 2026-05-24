#!/usr/bin/env python3
"""
Benchmark: Redis cache impact on gatekeeper latency.

Strategy: flush Redis between runs instead of stopping it, so gatekeeper's
connection stays healthy. Three conditions measured back-to-back:

  1. Warm cache   — all 200 requests hit Redis
  2. Cold cache   — FLUSHALL first; request 1 is a DB miss, 2+ are cache hits
  3. List users   — endpoint whose results are never cached (no ID-based key),
                    gives a clean "always DB" baseline on the same host

Run:
  python3 tests/bench_redis.py [--url http://localhost:8080]
"""

import argparse
import statistics
import subprocess
import sys
import time
import uuid

import requests

TIMEOUT = 10   # seconds per HTTP request
N = 200        # requests per condition
REDIS_CONTAINER = "local-redis-1"


# ── helpers ──────────────────────────────────────────────────────────────────

def signup_and_login(base_url):
    uid = uuid.uuid4().hex[:8]
    email = f"bench_{uid}@example.com"
    password = "benchpass123"
    r = requests.post(f"{base_url}/signup", json={
        "email": email, "username": f"bench_{uid}", "password": password,
    }, timeout=TIMEOUT)
    assert r.status_code == 201, f"signup failed: {r.text}"
    user_id = r.json()["user_id"]
    r = requests.post(f"{base_url}/login",
                      json={"email": email, "password": password}, timeout=TIMEOUT)
    assert r.status_code == 200, f"login failed: {r.text}"
    return user_id, r.json()["token"]


def measure_get_user(base_url, user_id, token, n=N):
    url = f"{base_url}/users/{user_id}"
    headers = {"Authorization": f"Bearer {token}"}
    latencies = []
    for _ in range(n):
        t0 = time.perf_counter()
        r = requests.get(url, headers=headers, timeout=TIMEOUT)
        latencies.append((time.perf_counter() - t0) * 1000)
        assert r.status_code == 200, f"unexpected {r.status_code}: {r.text}"
    return latencies



def flush_redis():
    r = subprocess.run(
        ["docker", "exec", REDIS_CONTAINER, "redis-cli", "FLUSHALL"],
        capture_output=True, text=True, timeout=5,
    )
    return r.returncode == 0


def redis_info():
    r = subprocess.run(
        ["docker", "exec", REDIS_CONTAINER, "redis-cli", "INFO", "stats"],
        capture_output=True, text=True, timeout=5,
    )
    hits = misses = 0
    for line in r.stdout.splitlines():
        if line.startswith("keyspace_hits:"):
            hits = int(line.split(":")[1])
        if line.startswith("keyspace_misses:"):
            misses = int(line.split(":")[1])
    return hits, misses


def stats(label, latencies):
    s = sorted(latencies)
    n = len(s)
    print(f"  {label}")
    print(f"    n={n}  mean={statistics.mean(s):.1f}ms  "
          f"p50={s[n//2]:.1f}ms  "
          f"p95={s[int(n*0.95)]:.1f}ms  "
          f"p99={s[int(n*0.99)]:.1f}ms  "
          f"min={s[0]:.1f}ms  max={s[-1]:.1f}ms")
    return s[n // 2]   # return p50


# ── main ─────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://localhost:8080")
    args = parser.parse_args()
    base_url = args.url

    # Wait for gatekeeper to be reachable
    print(f"Waiting for {base_url} ...")
    for i in range(30):
        try:
            requests.get(f"{base_url}/users", timeout=2)
            break
        except Exception:
            if i == 29:
                sys.exit("gatekeeper not reachable after 30s")
            time.sleep(1)

    print(f"\nCreating benchmark user ...")
    user_id, token = signup_and_login(base_url)
    print(f"  user_id={user_id}")

    p50s = {}

    # ── 1. Warm the cache with one request, then benchmark ───────────────────
    flush_redis()
    requests.get(f"{base_url}/users/{user_id}",
                 headers={"Authorization": f"Bearer {token}"},
                 timeout=TIMEOUT)               # populate session + user cache
    time.sleep(0.05)

    print(f"\n[1/3] WARM CACHE  ({N} requests, all cache hits expected)")
    h0, m0 = redis_info()
    lats = measure_get_user(base_url, user_id, token)
    h1, m1 = redis_info()
    p50s["warm"] = stats("GET /users/{id}", lats)
    print(f"    redis hits={h1-h0}  misses={m1-m0}")

    # ── 2. Cold cache: flush, then measure (request 1 misses, rest hit) ──────
    flush_redis()
    time.sleep(0.05)

    print(f"\n[2/3] COLD CACHE  ({N} requests; req 1 is a DB miss, rest hit cache)")
    h0, m0 = redis_info()
    lats = measure_get_user(base_url, user_id, token)
    h1, m1 = redis_info()
    p50s["cold_all"] = stats("all requests (incl. cold miss)", lats)
    p50s["cold_warm"] = stats("warm tail only (req 2+)", lats[1:])
    print(f"    redis hits={h1-h0}  misses={m1-m0}")
    print(f"    cold miss latency: {lats[0]:.1f}ms")

    # ── Summary ───────────────────────────────────────────────────────────────
    # The cold miss (lats[0] from run 2) is the cleanest "no cache" datapoint:
    # it's the same endpoint, same user, guaranteed to hit Postgres.
    db_lat = lats[0]

    print(f"\n{'='*60}")
    print(f"p50 summary:")
    print(f"  warm cache hits : {p50s['warm']:.1f}ms")
    print(f"  cold cache tail : {p50s['cold_warm']:.1f}ms")
    print(f"  single DB miss  : {db_lat:.1f}ms  (req 1 of cold run)")
    if p50s["warm"] > 0 and db_lat > 0:
        speedup = db_lat / p50s["warm"]
        saved = db_lat - p50s["warm"]
        print(f"\n  cache hit vs DB miss: {speedup:.1f}x faster  ({saved:.1f}ms saved per request)")


if __name__ == "__main__":
    main()
