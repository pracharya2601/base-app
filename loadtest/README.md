# loadtest — base-app traffic / load harness

A zero-dependency load test for base-app. Spins up a test collection, then drives
three scenarios through a small Go load generator and reports throughput +
latency percentiles. **No k6/oha install** — just Go (already required to build
base-app) and a running instance.

## Run it

```bash
# 1. make sure the target is running (local):
docker compose up -d --build        # or: go build -o base-app . && ./base-app serve

# 2. one-shot: prepare + load test (local, defaults c=50 d=20s)
./loadtest/run.sh

# tune it:
C=100 D=30s ./loadtest/run.sh

# point at a deployed instance (READ THE CAVEAT BELOW):
BASE=https://your.app ./loadtest/run.sh
```

`run.sh` calls `setup.sh` (creates the `loadtest` collection, seeds 200 records,
mints an API key → `loadtest/.env`) then runs `go run ./loadtest`.

## The three scenarios — and why

| scenario | request | isolates |
|---|---|---|
| `read_public` | `GET /records` (no auth) | raw read path: SQLite read + JSON encode |
| `read_key` | `GET /records` + `X-API-Key` | read path **+ the API-key middleware** (key lookup + the per-request `lastUsedUnix` **write**) |
| `write` | `POST /records` + `X-API-Key` | the SQLite **single-writer** ceiling (+ Litestream if replication is on) |

The whole point of the split: **`read_key` − `read_public` = the cost of the
per-request write** the middleware does to stamp `lastUsedUnix`. The harness
prints that delta explicitly.

## Reading the output

```
scenario         reqs       rps     err%       p50       p95       p99       max
read_public     83839      5582     0.0%     2.9ms    48.0ms    69.0ms   142.6ms
read_key        44683      2976     0.0%    11.5ms    50.0ms    75.9ms   170.6ms
write           31680      2110     0.0%    19.7ms    56.6ms    79.9ms   162.1ms
```

- **rps** = throughput. **err%** should be ~0; if it climbs, you've found a limit.
- **p95/p99** = tail latency — what your slowest 5%/1% of users feel.
- `write` rps is your **write ceiling** and won't rise much with more concurrency
  (SQLite serializes writes). `read_public` scales with cores; `read_key` is
  effectively write-bound because of `lastUsedUnix`.

## ⚠️ Caveats — don't over-read the numbers

- **Co-located load gen.** Running `loadgen` and the server on the same laptop
  means at high concurrency they fight for CPU — `read_public` can *drop* at high
  `-c` purely from that. For clean ceilings, run the generator from a **separate
  machine**, or keep `-c` modest (≤ ~50 on a laptop).
- **Render free tier is a FLOOR, not a ceiling.** Free services throttle CPU/RAM
  and **spin down after ~15 min idle** (cold start adds seconds). Numbers there
  measure Render's throttle, not your app. Use local/paid runs for real capacity;
  use the free-tier run only to check cold-start + graceful behavior.
- **Litestream adds per-write overhead.** If `LITESTREAM_BUCKET` is set, every
  write ships WAL — the `write` ceiling will be lower than with replication off.

## Cleanup

The `loadtest` collection + seeded/written records stay in the DB. To remove:

```bash
TOKEN=$(curl -s -X POST "$BASE/api/collections/_superusers/auth-with-password" \
  -H 'Content-Type: application/json' \
  -d '{"identity":"admin@example.com","password":"SuperSecret123"}' | jq -r .token)
curl -s -X DELETE "$BASE/api/collections/loadtest" -H "Authorization: $TOKEN"
```

Revoke load-test API keys from `/admin#keys` (they're named `loadtest-<ts>`).

## Files

- `setup.sh` — provision collection + seed + mint key → `loadtest/.env`
- `loadgen.go` — the Go load generator (`go run ./loadtest -h` for flags)
- `run.sh` — health-check → setup → load test
