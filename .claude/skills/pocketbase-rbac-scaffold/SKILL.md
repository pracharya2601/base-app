---
name: pocketbase-rbac-scaffold
description: >
  Scaffold a NEW backend that uses PocketBase in Go framework-mode with the
  base-app RBAC stack: a custom superadmin/provision API, native-rule RBAC
  (_roles + _permissions, auto-governed collections), API keys that act as roled
  service accounts, plus Litestream backups and an optional MCP server. Use when
  starting a new project that needs this exact auth/RBAC platform, or porting it
  into an existing repo. To OPERATE a running instance instead, use
  pocketbase-rbac-ops.
---

# Scaffold the PocketBase + RBAC stack

This generator materializes a self-contained backend from the bundled
`templates/` (verbatim snapshots of the base-app reference). The hard part —
native-rule RBAC, service-account keys, auto-govern hooks — is already written;
you copy, rename, set the module + secrets, build, and verify.

## What you get
- `main.go` — framework entry; `/api/superadmin/provision` (idempotent schema),
  field-type catalog.
- `apikeys.go` — `_apiKeys` system collection, scopes, dual-plane key middleware.
- `roles.go` — `_roles` + `_permissions`, native-rule RBAC, auto-govern hooks
  (create/delete + startup backfill), role-admin endpoints.
- `serviceaccounts.go` — `_serviceAccounts` auth collection; the roled identity a
  key acts as on record routes.
- `Dockerfile`, `docker-compose.yml`, `entrypoint.sh`, `litestream.yml`,
  `.env.example` — container + Litestream → S3, secrets via `.env`.
- `scripts/mint-token.sh`, `scripts/mint-apikey.sh` — operator token + scoped,
  roled API key.
- `mcp/` (optional) — Go MCP server exposing the superadmin + RBAC tools.

## Procedure

### 1. Materialize the files
Set `TEMPLATES` to this skill's `templates/` dir and `DEST` to the new project,
then copy + strip the `.tmpl` suffix (and restore dotfiles):
```bash
TEMPLATES=".../pocketbase-rbac-scaffold/templates"   # this skill's templates dir
DEST="./my-backend"
mkdir -p "$DEST/scripts" "$DEST/mcp"
cp "$TEMPLATES/main.go.tmpl"             "$DEST/main.go"
cp "$TEMPLATES/apikeys.go.tmpl"          "$DEST/apikeys.go"
cp "$TEMPLATES/roles.go.tmpl"            "$DEST/roles.go"
cp "$TEMPLATES/serviceaccounts.go.tmpl"  "$DEST/serviceaccounts.go"
cp "$TEMPLATES/go.mod.tmpl"              "$DEST/go.mod"
cp "$TEMPLATES/Dockerfile.tmpl"          "$DEST/Dockerfile"
cp "$TEMPLATES/docker-compose.yml.tmpl"  "$DEST/docker-compose.yml"
cp "$TEMPLATES/entrypoint.sh.tmpl"       "$DEST/entrypoint.sh"
cp "$TEMPLATES/litestream.yml.tmpl"      "$DEST/litestream.yml"
cp "$TEMPLATES/env.example.tmpl"         "$DEST/.env.example"
cp "$TEMPLATES/gitignore.tmpl"           "$DEST/.gitignore"
cp "$TEMPLATES/dockerignore.tmpl"        "$DEST/.dockerignore"
cp "$TEMPLATES/scripts/mint-token.sh.tmpl"  "$DEST/scripts/mint-token.sh"
cp "$TEMPLATES/scripts/mint-apikey.sh.tmpl" "$DEST/scripts/mint-apikey.sh"
cp "$TEMPLATES/mcp/main.go.tmpl"         "$DEST/mcp/main.go"   # optional
cp "$TEMPLATES/mcp/go.mod.tmpl"          "$DEST/mcp/go.mod"    # optional
cp "$TEMPLATES/mcp.json.tmpl"            "$DEST/.mcp.json"     # optional
chmod +x "$DEST/entrypoint.sh" "$DEST/scripts/"*.sh
```

### 2. Fill in the placeholders
Templates carry **placeholders**, not dev values — nothing secret is baked in.
Substitute them across the materialized files:

| Placeholder | Files | Set to |
|---|---|---|
| `__MODULE_NAME__` | `go.mod` | your Go module name (only place it matters — all `.go` are `package main`) |
| `__ADMIN_EMAIL__` | mint scripts, `mcp/main.go` fallback | superuser email |
| `__ADMIN_PASSWORD__` | mint scripts, `mcp/main.go` fallback | superuser password |
| `__MCP_BIN_PATH__` | `.mcp.json` | absolute path to the built `mcp/pb-mcp` |
| `__PB_API_KEY__` | `.mcp.json` | a minted `pbk_…` key — fill AFTER step 5 |

```bash
cd "$DEST"
grep -rl '__MODULE_NAME__\|__ADMIN_EMAIL__\|__ADMIN_PASSWORD__' . 2>/dev/null | xargs sed -i '' \
  -e 's/__MODULE_NAME__/my-backend/g' \
  -e 's/__ADMIN_EMAIL__/you@example.com/g' \
  -e 's/__ADMIN_PASSWORD__/CHANGE_ME/g'
```
The mint scripts also accept `PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` via env —
prefer that over baking a password into files. `.mcp.json` placeholders are set
in step 6 (after the binary is built and a key is minted).

### 3. Secrets
```bash
cp "$DEST/.env.example" "$DEST/.env"     # fill LITESTREAM_* (empty bucket = no replication)
```
In prod, inject `LITESTREAM_*` from a secret manager, not a committed `.env`.
(`ADMIN` placeholders were handled in step 2; below, `<email>`/`<pw>` are the
values you chose there.)

### 4. Build, run, bootstrap the superuser
```bash
cd "$DEST" && docker compose up -d --build
# create the first superuser (no UI step needed):
docker compose exec pocketbase /pb/pocketbase superuser upsert <email> <pw>
```
On boot the app auto-creates `_apiKeys`, `_roles`, `_permissions`,
`_serviceAccounts` and seeds `viewer`/`editor`/`admin` roles.

### 5. Verify (must all pass)
```bash
curl -s localhost:8090/api/health                                  # 200
TOKEN=$(curl -s -X POST localhost:8090/api/collections/_superusers/auth-with-password \
  -H 'Content-Type: application/json' -d '{"identity":"<email>","password":"<pw>"}' | jq -r .token)
curl -s localhost:8090/api/collections -H "Authorization: $TOKEN" | jq -r '[.items[].name]' | grep -E '_roles|_permissions|_serviceAccounts'
curl -s localhost:8090/api/superadmin/roles -H "Authorization: $TOKEN" | jq -r '.roles[].name'   # viewer editor admin
# provision a collection -> it should auto-govern (RBAC rules + tokens appear)
curl -s -X POST localhost:8090/api/superadmin/provision -H "Authorization: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"collections":[{"name":"notes","fields":[{"name":"title","type":"text"}]}]}'
curl -s localhost:8090/api/collections/notes -H "Authorization: $TOKEN" | jq -r '.createRule'   # references roles.permissions.token
```
Then mint a key (`./scripts/mint-apikey.sh`) and confirm a no-role key sees zero
records — see pocketbase-rbac-ops for the runtime playbook.

### 6. Wire the MCP (optional)
```bash
cd "$DEST/mcp" && go build -o pb-mcp .
KEY=$(cd "$DEST" && ./scripts/mint-apikey.sh)   # prints a pbk_… (mints scoped+roled)
cd "$DEST" && sed -i '' \
  -e "s#__MCP_BIN_PATH__#$PWD/mcp/pb-mcp#" \
  -e "s/__PB_API_KEY__/$KEY/" .mcp.json
```
Restart the MCP client. Treat the filled `.mcp.json` as a secret (it holds a key).

## Adapt / extend
- Different model? Edit `rbacRules` / `knownActions` in `roles.go`, or add scopes
  in `apikeys.go` — both are code, no migration.
- Don't want a collection auto-governed (e.g. public signup)? Give it an explicit
  non-nil rule; the auto-govern guard skips collections that already have rules.
- Drop the MCP if unused: delete `mcp/` and `.mcp.json`.

## Pinned versions (don't drift)
PocketBase **v0.39.4** (needs **Go ≥ 1.25**); Litestream **v0.5.12**
(single `replica:`, `x86_64`/`arm64` assets); MCP SDK `mark3labs/mcp-go` v0.42.

## Keeping templates in sync (maintenance)
The `templates/*.tmpl` are snapshots of the base-app reference, **re-parameterized**
(dev module name + creds → placeholders). They are NOT auto-synced. When the
reference changes, re-copy AND re-apply the parameterization (a blind `cp` would
re-bake `base-app` / dev creds):
```bash
# run from the base-app repo root
SK=.claude/skills/pocketbase-rbac-scaffold/templates
for f in main apikeys roles serviceaccounts; do cp $f.go $SK/$f.go.tmpl; done
cp go.mod $SK/go.mod.tmpl; cp Dockerfile $SK/Dockerfile.tmpl
cp docker-compose.yml $SK/docker-compose.yml.tmpl; cp entrypoint.sh $SK/entrypoint.sh.tmpl
cp litestream.yml $SK/litestream.yml.tmpl; cp .env.example $SK/env.example.tmpl
cp .gitignore $SK/gitignore.tmpl; cp .dockerignore $SK/dockerignore.tmpl
cp scripts/mint-token.sh $SK/scripts/mint-token.sh.tmpl
cp scripts/mint-apikey.sh $SK/scripts/mint-apikey.sh.tmpl
cp mcp/main.go $SK/mcp/main.go.tmpl; cp mcp/go.mod $SK/mcp/go.mod.tmpl
# re-parameterize (idempotent):
sed -i '' 's/^module base-app/module __MODULE_NAME__/' $SK/go.mod.tmpl
for f in $SK/mcp/main.go.tmpl $SK/scripts/mint-token.sh.tmpl $SK/scripts/mint-apikey.sh.tmpl; do
  sed -i '' -e 's/admin@example\.com/__ADMIN_EMAIL__/g' -e 's/SuperSecret123/__ADMIN_PASSWORD__/g' "$f"
done
# verify no live values leaked:
grep -rn 'base-app\|admin@example.com\|SuperSecret123\|pbk_[0-9a-f]\{48\}' $SK && echo "LEAK — fix before commit" || echo "clean"
```
`mcp.json.tmpl` is hand-maintained (pure placeholders); don't overwrite it from a
live `.mcp.json` (that one holds a real key + machine path).
The model/usage these files implement: `docs/RBAC.md`, `CLAUDE.md`.
