#!/usr/bin/env bash
# Mint a long-lived PocketBase access token via the impersonate API.
# The password is used ONCE here to mint; the resulting token is what you store
# (e.g. as PB_TOKEN for the MCP server) — no password kept in app config.
#
# Usage:
#   PB_URL=http://localhost:8090 \
#   PB_ADMIN_EMAIL=admin@example.com PB_ADMIN_PASSWORD=SuperSecret123 \
#   DURATION_SECONDS=31536000 ./scripts/mint-token.sh
set -euo pipefail

PB_URL="${PB_URL:-http://localhost:8090}"
EMAIL="${PB_ADMIN_EMAIL:-admin@example.com}"
PASS="${PB_ADMIN_PASSWORD:-SuperSecret123}"
DURATION="${DURATION_SECONDS:-31536000}"  # default 1 year

# 1. Authenticate (password used only for this minting step).
RESP=$(curl -s -X POST "$PB_URL/api/collections/_superusers/auth-with-password" \
  -H "Content-Type: application/json" \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}")
ADMIN_TOKEN=$(echo "$RESP" | jq -r '.token')
ADMIN_ID=$(echo "$RESP" | jq -r '.record.id')

if [ "$ADMIN_TOKEN" = "null" ] || [ -z "$ADMIN_TOKEN" ]; then
  echo "auth failed: $RESP" >&2
  exit 1
fi

# 2. Mint a long-lived, non-refreshable token for this identity.
MINT=$(curl -s -X POST "$PB_URL/api/collections/_superusers/impersonate/$ADMIN_ID" \
  -H "Authorization: $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d "{\"duration\": $DURATION}")
LONG_TOKEN=$(echo "$MINT" | jq -r '.token')

if [ "$LONG_TOKEN" = "null" ] || [ -z "$LONG_TOKEN" ]; then
  echo "mint failed: $MINT" >&2
  exit 1
fi

# 3. Show expiry and emit the token.
EXP=$(echo "$LONG_TOKEN" | cut -d. -f2 | tr '_-' '/+' | (cat; echo "==") | base64 -d 2>/dev/null | jq -r '.exp | todate' 2>/dev/null || echo "?")
echo "# token expires: $EXP" >&2
echo "$LONG_TOKEN"
