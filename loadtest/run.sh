#!/usr/bin/env bash
# One-shot: verify the target is up, prepare it (setup.sh), then run the Go load
# generator across all three scenarios.
#
# Usage:
#   ./loadtest/run.sh                       # local, defaults (c=50, d=20s)
#   C=100 D=30s ./loadtest/run.sh           # tune concurrency / duration
#   BASE=https://your.app ./loadtest/run.sh # remote target (see README caveats)
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${BASE:-http://localhost:8090}"
C="${C:-50}"
D="${D:-20s}"

echo "==> health check $BASE"
curl -sf -m 5 "$BASE/api/health" >/dev/null || {
  echo "target not reachable at $BASE — start it first (e.g. docker compose up -d --build)" >&2
  exit 1
}

BASE="$BASE" ./loadtest/setup.sh
# shellcheck disable=SC1091
set -a; . loadtest/.env; set +a

echo
echo "==> load test (concurrency=$C duration=$D)"
go run ./loadtest -base "$BASE_URL" -key "$API_KEY" -scenario all -c "$C" -d "$D"
