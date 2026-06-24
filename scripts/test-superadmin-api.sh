#!/usr/bin/env bash
# Exercise the PocketBase SUPERADMIN API end-to-end — everything the admin UI
# does is just these REST calls. Requires: curl + jq.
set -euo pipefail

BASE="${BASE:-http://localhost:8090}"
EMAIL="${EMAIL:-admin@example.com}"
PASS="${PASS:-SuperSecret123}"

echo "==> 1. Authenticate as a superuser (_superusers collection)"
TOKEN=$(curl -s -X POST "$BASE/api/collections/_superusers/auth-with-password" \
  -H "Content-Type: application/json" \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jq -r '.token')
echo "    token: ${TOKEN:0:24}...(truncated)"

AUTH=(-H "Authorization: $TOKEN")

echo "==> 2. Create a new collection 'articles' (schema management via API)"
curl -s -X POST "$BASE/api/collections" "${AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "articles",
    "type": "base",
    "fields": [
      { "name": "title", "type": "text", "required": true },
      { "name": "body",  "type": "text" }
    ],
    "listRule": "",
    "viewRule": ""
  }' | jq '{id, name, type}'

echo "==> 3. Insert a record into it"
curl -s -X POST "$BASE/api/collections/articles/records" "${AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d '{ "title": "Hello from the API", "body": "no UI was used" }' | jq '{id, title, body}'

echo "==> 4. Read it back"
curl -s "$BASE/api/collections/articles/records" "${AUTH[@]}" | jq '.items[] | {id, title}'

echo "==> 5. Update an application setting (also superuser-only)"
curl -s -X PATCH "$BASE/api/settings" "${AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d '{ "meta": { "appName": "ApiDrivenPB" } }' | jq '.meta.appName'

echo "==> done. Open http://localhost:8090/_/ to see the 'articles' collection in the UI."
