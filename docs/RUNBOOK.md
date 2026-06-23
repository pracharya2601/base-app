# Runbook — common commands

## Lifecycle
```bash
docker compose up -d --build          # build + start everything
docker compose up -d --build pocketbase   # rebuild only the app after *.go edits
docker compose logs -f pocketbase     # tail logs (watch Litestream sync)
docker compose down                   # stop (data persists in ./pb_data)
```

## Auth
```bash
# password -> JWT
TOKEN=$(curl -s -X POST http://localhost:8090/api/collections/_superusers/auth-with-password \
  -H "Content-Type: application/json" \
  -d '{"identity":"admin@example.com","password":"SuperSecret123"}' | jq -r '.token')

# mint a long-lived token (store as PB_TOKEN)
TOKEN=$(./scripts/mint-token.sh 2>/dev/null)
# custom server / duration:
PB_URL=https://prod PB_ADMIN_EMAIL=.. PB_ADMIN_PASSWORD=.. DURATION_SECONDS=7776000 ./scripts/mint-token.sh
```

## Provision schema (idempotent)
```bash
curl -s -X POST http://localhost:8090/api/superadmin/provision \
  -H "Authorization: $TOKEN" -H "Content-Type: application/json" -d '{
    "collections":[
      {"name":"posts","fields":[
        {"name":"title","type":"text","required":true},
        {"name":"status","type":"select","values":["draft","live"]},
        {"name":"author","type":"relation","collection":"users","cascadeDelete":true}
      ]}
    ],
    "seed":{"posts":[{"title":"hello","status":"draft"}]},
    "appName":"MyApp"
  }'
```

## API keys
A key has two gates: **scopes** (control plane — schema/settings/keys) and
**roles** (data plane — records, via its service account). See `docs/RBAC.md`.
```bash
# mint a key. scopes => control-plane; roles => which records it can touch.
curl -s -X POST http://localhost:8090/api/superadmin/apikeys -H "Authorization: $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"ci","scopes":["schema:write"],"roles":["<role-id>"],"expiresInDays":90}'

# convenience: mint a scoped key for the MCP server (creates a service account too)
./scripts/mint-apikey.sh                 # prints pbk_...

# use it — control plane (scope) and data plane (roles) both via X-API-Key
curl -s http://localhost:8090/api/collections -H "X-API-Key: pbk_..."          # needs schema:read scope
curl -s http://localhost:8090/api/collections/articles/records -H "X-API-Key: pbk_..."  # needs a role granting articles:read

# list (shows each key's resolved role names) / revoke
curl -s http://localhost:8090/api/superadmin/apikeys -H "Authorization: $TOKEN" | jq
curl -s -X DELETE http://localhost:8090/api/superadmin/apikeys/<id> -H "Authorization: $TOKEN"
```

## RBAC (roles & permissions)
Permission tokens are auto-created/governed per collection; you mostly just grant
tokens to roles. See `docs/RBAC.md` for the full model.
```bash
# discover the grammar + list roles/permissions
curl -s http://localhost:8090/api/superadmin/permissions | jq
curl -s "http://localhost:8090/api/collections/_roles/records?expand=permissions" \
  -H "Authorization: $TOKEN" | jq -r '.items[] | "\(.name): \([.expand.permissions[]?.token]|sort)"'

# grant a token to a role (permissions is a relation: send permission-record ids)
PID=$(curl -s "http://localhost:8090/api/collections/_permissions/records?filter=(token='projects:read')" \
  -H "Authorization: $TOKEN" | jq -r '.items[0].id')
RID=$(curl -s "http://localhost:8090/api/collections/_roles/records?filter=(name='viewer')" \
  -H "Authorization: $TOKEN" | jq -r '.items[0].id')
CUR=$(curl -s "http://localhost:8090/api/collections/_roles/records/$RID" -H "Authorization: $TOKEN" | jq -c '.permissions')
curl -s -X PATCH "http://localhost:8090/api/collections/_roles/records/$RID" -H "Authorization: $TOKEN" \
  -H "Content-Type: application/json" -d "{\"permissions\": $(echo "$CUR" | jq -c ". + [\"$PID\"]")}"

# assign roles to a user / govern a collection on demand
# user:   PATCH /api/collections/users/records/<id>  {"roles":["<role-id>"]}
# govern: POST  /api/superadmin/provision            {"collections":[{"name":"x","rbac":true}]}
```

## Settings
```bash
curl -s http://localhost:8090/api/settings -H "Authorization: $TOKEN" | jq
curl -s -X PATCH http://localhost:8090/api/settings -H "Authorization: $TOKEN" \
  -H "Content-Type: application/json" -d '{"meta":{"appName":"Acme"}}'
```

## Frontend types
```bash
npx pocketbase-typegen --url http://localhost:8090 \
  --email admin@example.com --password SuperSecret123 --out ./pocketbase-types.ts
```

## MCP
```bash
cd mcp && go build -o pb-mcp .        # rebuild, then restart the MCP client
# smoke test over stdio:
( printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  sleep 1 ) | PB_URL=http://localhost:8090 ./pb-mcp 2>/dev/null | jq -c 'select(.id==2) | [.result.tools[].name]'
```

## Disaster-recovery test (Litestream)
```bash
# write data, then:
docker compose stop pocketbase && rm -rf ./pb_data && docker compose up -d pocketbase
docker compose logs pocketbase | grep -i restore     # "no local DB found — attempting restore"
```

## Inspect PocketBase source (for exact Go API)
```bash
docker run --rm -v "$PWD/go.mod:/src/go.mod" -w /src golang:1.25-alpine sh -c \
  'go mod download github.com/pocketbase/pocketbase 2>/dev/null;
   PB=$(go env GOMODCACHE)/github.com/pocketbase/pocketbase@v0.39.4; grep -rn "<symbol>" "$PB/core/"'
```
