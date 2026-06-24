#!/usr/bin/env bash
# Prepare a target instance for load testing:
#   1. create a `loadtest` collection (PUBLIC read, authed write) if missing
#   2. seed it with SEED_COUNT records so reads return data
#   3. mint a records:read+write API key (acts as a service account)
#   4. write loadtest/.env with BASE_URL + API_KEY for the load generator
#
# Idempotent: re-running reuses the collection and tops up the seed. It mints a
# FRESH key each run (cheap; revoke old ones from /admin#keys if you care).
#
# Usage:
#   BASE=http://localhost:8090 ./loadtest/setup.sh
#   BASE=https://pocketbase.easypezy.com ./loadtest/setup.sh   # remote (see README caveats)
set -euo pipefail
cd "$(dirname "$0")/.."

BASE="${BASE:-http://localhost:8090}"
EMAIL="${EMAIL:-admin@example.com}"
PASS="${PASS:-SuperSecret123}"
SEED_COUNT="${SEED_COUNT:-200}"
COLL="loadtest"

echo "==> auth as superuser @ $BASE"
TOKEN=$(curl -s -X POST "$BASE/api/collections/_superusers/auth-with-password" \
  -H "Content-Type: application/json" \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jq -r '.token')
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || { echo "auth failed" >&2; exit 1; }

echo "==> ensure '$COLL' collection (public read, authed write)"
if curl -s -o /dev/null -w '%{http_code}' "$BASE/api/collections/$COLL" -H "Authorization: $TOKEN" | grep -q '^200$'; then
  echo "    exists, reusing"
else
  curl -s -X POST "$BASE/api/collections" -H "Authorization: $TOKEN" \
    -H "Content-Type: application/json" -d '{
      "name": "loadtest",
      "type": "base",
      "fields": [
        { "name": "title", "type": "text" },
        { "name": "n",     "type": "number" }
      ],
      "listRule": "",
      "viewRule": "",
      "createRule": "@request.auth.id != \"\"",
      "updateRule": "@request.auth.id != \"\"",
      "deleteRule": "@request.auth.id != \"\""
    }' | jq -r 'if .id then "    created (\(.id))" else "    create failed: \(.message // tostring)" end'
fi

echo "==> seed up to $SEED_COUNT records"
HAVE=$(curl -s "$BASE/api/collections/$COLL/records?perPage=1" -H "Authorization: $TOKEN" | jq -r '.totalItems // 0')
echo "    currently $HAVE"
i="$HAVE"
while [ "$i" -lt "$SEED_COUNT" ]; do
  curl -s -o /dev/null -X POST "$BASE/api/collections/$COLL/records" -H "Authorization: $TOKEN" \
    -H "Content-Type: application/json" -d "{\"title\":\"seed-$i\",\"n\":$i}"
  i=$((i+1))
done
echo "    seeded to $i"

echo "==> mint a records:read+write API key"
ROLE_ID=$(curl -s -G "$BASE/api/superadmin/roles" -H "Authorization: $TOKEN" \
  | jq -r '.roles[] | select(.name=="admin") | .id')
[ -n "$ROLE_ID" ] && [ "$ROLE_ID" != "null" ] || { echo "admin role not found" >&2; exit 1; }
KEY=$(curl -s -X POST "$BASE/api/superadmin/apikeys" -H "Authorization: $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"loadtest-$(date +%s)\",\"scopes\":[\"records:read\",\"records:write\"],\"roles\":[\"$ROLE_ID\"]}" \
  | jq -r '.key')
[ -n "$KEY" ] && [ "$KEY" != "null" ] || { echo "mint failed" >&2; exit 1; }

cat > loadtest/.env <<EOF
BASE_URL=$BASE
API_KEY=$KEY
EOF
echo "==> wrote loadtest/.env (BASE_URL + API_KEY)"
echo "    done."
