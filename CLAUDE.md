# base-app — PocketBase extended as a backend platform

A single PocketBase **framework-mode** Go binary, extended with a superadmin
API, a real API-key system, an MCP server, Litestream backups, and generated
frontend types. This file is the source of truth — read it first.

## What this is

PocketBase used **as a Go framework** (not the prebuilt binary): we compile our
own `main.go` so we can add custom routes, middleware, and a real API-key system
that PocketBase lacks natively. It runs in Docker with Litestream streaming the
SQLite DB to an S3 target (any S3 or S3-compatible store; credentials via `.env`).

## Goals achieved (in build order)

1. Run PocketBase in Docker with persistent data ✅
2. Drive all superadmin tasks from API, not the UI ✅ (`/api/superadmin/provision`)
3. Production-shaped durability ✅ (Litestream → S3, restore-on-boot)
4. Generated, always-in-sync frontend TypeScript types ✅ (`pocketbase-typegen`)
5. LLM-drivable via an MCP server ✅ (`mcp/pb-mcp`, wired to Claude Desktop)
6. Token-based auth instead of stored passwords ✅ (impersonate, `mint-token.sh`)
7. A real, scoped, revocable **API-key** system ✅ (`_apiKeys` system collection)
8. Settings management over MCP ✅ (`get_settings` / `update_settings`)
9. **RBAC** for users AND API keys ✅ (`_roles`/`_permissions`, native rules, auto-governed) — see `docs/RBAC.md`
10. **AI proxy** over goai ✅ (24 providers; `/api/ai/*` text/stream/image; encrypted provider keys; usage metering; per-user rate limit + token quota; unified `/admin` console) — see `docs/AI-PROXY.md`

## Stack & pinned versions

| Component | Version | Notes |
|---|---|---|
| PocketBase | **v0.39.4** | `github.com/pocketbase/pocketbase`; needs **Go ≥ 1.25** |
| Go (build) | 1.25 (Docker) / 1.26 (local) | pure-Go SQLite, `CGO_ENABLED=0` |
| Litestream | **v0.5.12** | v0.5 uses single `replica:` (not `replicas:`) |
| MCP SDK | `github.com/mark3labs/mcp-go` v0.42.0 | Go, stdio transport |
| Frontend types | `pocketbase-typegen` | run against the live instance |

## File map

```
main.go              framework entry: routes, provision endpoint, field-type catalog
apikeys.go           API-key system: _apiKeys system collection, scopes, auth middleware
roles.go             user RBAC: _roles + _permissions system collections, native-rule enforcement
serviceaccounts.go   _serviceAccounts auth collection; the roled identity an API key acts as on data routes
ai.go                AI proxy: 24 goai providers, /api/ai/{provider}/generate + /stream, _aiProviders (encrypted keys) + _aiUsage (metering), catalog/limits routes
aiimage.go           AI image gen: /api/ai/{provider}/image -> _aiImages file field (S3/R2 when Files storage on) + preview URLs (openai/google/vertex/azure)
airatelimit.go       AI rate limit (req/min, in-memory) + per-user token quota (tokens/day from _aiUsage); superusers/keys exempt; env-configured
adminui.go           unified /admin console (embeds admin_ui.html): sidebar over AI Providers, Images, API Keys
admin_ui.html        the /admin console page (PocketBase-native palette, auto-login via dashboard session)
keysui.go / aiui.go  thin redirects: /admin/apikeys -> /admin#keys, /admin/ai -> /admin#providers (folded into the unified console)
Dockerfile           multi-stage: Go build (golang:1.25) -> alpine + Litestream
docker-compose.yml   pocketbase(+litestream) service; secrets via .env (env_file)
.env.example         secrets template -> copy to .env (gitignored); inject from a vault in prod
entrypoint.sh        restore-on-boot, then `litestream replicate -exec pocketbase serve`
litestream.yml       replica config (env-driven, force-path-style for S3-compat)
scripts/mint-token.sh   mint a long-lived impersonate token (the "access token")
scripts/mint-apikey.sh  mint a scoped, revocable API key for a service client (e.g. MCP)
mcp/main.go          MCP server: 9 tools (incl. agentic RBAC: list_roles, manage_role, assign_user_roles)
.mcp.json            project-scoped MCP config (Claude Code / Desktop)
pocketbase-types.ts  generated frontend types (regenerate after schema changes)
test-superadmin-api.sh  raw-curl walkthrough of the superuser API
pb_hooks/main.pb.js  (unused in framework mode — JSVM plugin not registered)
```

## Run it

```bash
cp .env.example .env                  # fill in S3 creds, or leave blank for no replication
docker compose up -d --build          # start pocketbase(+litestream)
# Dashboard: http://localhost:8090/_/   (admin@example.com / SuperSecret123)
docker compose logs -f pocketbase     # watch Litestream sync (~1s)
```

Secrets live in `.env` (gitignored), loaded via `env_file`. Leave
`LITESTREAM_BUCKET` empty to boot without replication. In **production**, do not
ship `.env` — inject the same `LITESTREAM_*` vars from a secret manager.

Rebuild after editing `*.go`: `docker compose up -d --build pocketbase`.
Rebuild MCP after editing `mcp/`: `cd mcp && go build -o pb-mcp .` then restart the MCP client.

## Custom endpoints (all under our binary)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/superadmin/field-types` | public | self-describing field-type catalog |
| GET | `/api/superadmin/scopes` | public | grantable API-key scopes |
| GET | `/api/superadmin/permissions` | public | user-role permission grammar (`collection:action`) |
| GET | `/api/superadmin/roles` | `roles:read` | list roles + their permission tokens |
| POST | `/api/superadmin/roles` | `roles:write` | upsert a role by name, set its permission tokens (idempotent) |
| POST | `/api/superadmin/users/roles` | `roles:write` | set a user's roles (by name; identify by email/userId) |
| POST | `/api/superadmin/provision` | superuser | create collections / add fields / set rules (or `rbac:true`) / seed / appName (idempotent) |
| POST | `/api/superadmin/apikeys` | superuser | mint a key with `scopes` + `roles` (plaintext returned once) |
| GET | `/api/superadmin/apikeys` | superuser | list keys (metadata only) |
| DELETE | `/api/superadmin/apikeys/{id}` | superuser | revoke a key |

**AI proxy** (auth: any JWT, or an `ai:use` API key — see `docs/AI-PROXY.md`):

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/ai/{provider}/generate` | text generation (24 goai providers) → text + usage |
| POST | `/api/ai/{provider}/stream` | streamed text (SSE) |
| POST | `/api/ai/{provider}/image` | image gen (openai/google/vertex/azure) → stored file + preview URL |
| GET | `/api/ai/catalog` · `/api/ai/image-catalog` · `/api/ai/providers` | provider allowlist · image-capable · usable-now |
| GET | `/api/ai/limits` | caller's rate/quota usage vs ceilings |
| GET | `/admin` | unified console (AI Providers + Images + API Keys) |

Provider keys live in the `_aiProviders` system collection, AES-encrypted at rest
by a save-hook (reuses `PB_ENCRYPTION_KEY`); manage via `/admin` or the records
API, **not** the PB dashboard. Limits via env: `AI_RATE_LIMIT_PER_MIN` (default
60), `AI_TOKEN_QUOTA_PER_DAY` (default 0 = off). OpenAI images: use `gpt-image-1`.

Plus all standard PocketBase endpoints (`/api/collections`, `/api/collections/{c}/records`, `/api/settings`, …).

## Auth model (3 ways in)

1. **Password** → JWT (`/api/collections/_superusers/auth-with-password`). Dev creds: `admin@example.com` / `SuperSecret123`.
2. **Long-lived token** (impersonate): `./scripts/mint-token.sh` → store as `PB_TOKEN`. Non-refreshable; re-mint on expiry. **Preferred over storing a password.**
3. **API keys** (`X-API-Key: pbk_...`): named, **scoped**, **roled**, revocable. Stored as SHA-256 hash in the `_apiKeys` system collection. The middleware (priority `DefaultLoadAuthTokenMiddlewarePriority + 1`) validates the key, then splits by plane:
   - **Control plane** (schema/settings/key-mgmt routes): enforces the key's **scope** and acts as the minting superuser (`superuserId`).
   - **Data plane** (`/api/collections/{c}/records`): acts AS the key's **service account** (`serviceAccountId` → a `_serviceAccounts` auth record carrying `roles`), so the **same native RBAC rules that gate users gate the key** — no superuser bypass. A key with no roles can't touch records. See "API keys are roled too" in `docs/RBAC.md`.

### API-key scopes
`admin` (all), `schema:read`, `schema:write`, `records:read`, `records:write`,
`settings:read`, `settings:write`, `keys:manage`, `roles:read`, `roles:write`,
`ai:use` (the AI proxy — `/api/ai/*`). Edit the `knownScopes` map +
`requiredScope()` in `apikeys.go` to add more (no schema migration needed).

API-key scopes are a **machine-client** gate (coarse, per-route-family). They are
NOT the same as user RBAC — see below.

## User roles & RBAC (`roles.go`)

User authorization (human JWTs) is enforced by PocketBase's **native
per-collection rules** — there is **no custom middleware**. The model is
normalized for exact (`?=`) matching:

- **`_permissions`** — system collection; one record per access token in its
  `token` field: `"articles:create"`, a collection wildcard `"articles:*"`, or
  the global `"*"`.
- **`_roles`** — system collection (locked, like `_apiKeys`); `name`,
  `description`, and **`permissions`** = a multi-relation → `_permissions`.
- **`users.roles`** — multi-relation (maxSelect 20) → `_roles`. A user's
  effective permissions are the **union** across all their roles. (Relations can
  target system collections; targets are immutable, so the startup migration
  drops+re-adds to repoint.)
- **Enforcement** — a collection marked `rbac:true` in provision gets rules like:
  ```
  create: @request.auth.roles.permissions.token ?= "articles:create"
       || @request.auth.roles.permissions.token ?= "articles:*"
       || @request.auth.roles.permissions.token ?= "*"
  ```
  The triple traversal `users → roles → permissions → token` flattens tokens
  across **all** the user's roles; `?=` is exact (no substring collisions). Denial
  uses native codes: create→**400**, update/delete→**404** (row filtered),
  list→empty. Superusers (incl. API-key auth) bypass; guests fall to the rules.
- Seeded defaults: `viewer` (`articles:read`, `users:read`), `editor`
  (`articles:read/create/update`), `admin` (`*`).
- **Auto-synced on create/delete**: `registerPermissionSyncHooks` binds
  `OnCollectionAfterCreateSuccess` / `OnCollectionAfterDeleteSuccess`. Creating any
  non-system collection (1) auto-creates its four `<name>:read/create/update/delete`
  tokens and (2) **auto-applies the `rbacRules`** — but only if the creator left
  all five rules at the default nil (`applyRbacRulesIfUnset`); set any rule
  explicitly and it's left alone. Dropping a collection deletes its tokens
  (PocketBase strips the freed ids from `_roles.permissions` automatically). Fires
  for both provision and dashboard/API creates. `backfillRBAC` does the same at
  startup for pre-existing/ungoverned collections — tokens AND rules — so
  governance is universal without anyone opting a collection in. System
  collections are skipped.
  **Consequence**: every non-system table is RBAC-governed by default (new ones
  via the hook, existing ones via startup backfill), reachable only by superusers,
  holders of `*` (the `admin` role), or roles granted its tokens. Collections with
  explicit rules (e.g. `users` — public signup + self-access) are never touched.
  To keep a collection superuser-only, give it an explicit non-nil rule.

**Auto-generated rules**: pass `"rbac": true` on a collection in the provision
spec and it writes the five rules from the collection name (the `rbac` flag is
applied first, then any explicit `rules` object overrides). A collection NOT
marked `rbac` keeps its own rules; left at nil it stays superuser-only.

`GET /api/superadmin/permissions` returns the token grammar. Manage role and
permission records via the standard superuser record API on `_roles` /
`_permissions`. This is app-wide RBAC; for per-resource grants (editor of project
5 only) add a `grants` join collection and reference it with `@collection.*`.

**How multiple roles combine + how to write rules against them:** see
[`docs/RBAC.md`](docs/RBAC.md) — the union semantics, the `?=` "any-of" operator,
and rule patterns (permission-based, role-name, per-record ownership).

## MCP server (`mcp/pb-mcp`)

12 tools: `list_field_types`, `list_collections`, `provision_schema`,
`create_record`, `get_settings`, `update_settings`, the **agentic RBAC** tools
`list_roles`, `manage_role`, `assign_user_roles`, and the **AI proxy** tools
`ai_list_providers`, `ai_generate_text`, `ai_generate_image` (so an AI coding
agent can discover + drive the proxy without ever holding a provider key — see
`docs/AI-PROXY.md`). The RBAC write tools'
descriptions tell the agent this is a **policy decision** and to **ASK THE USER**
before granting permissions or assigning powerful roles — the MCP doesn't invent
access policy. Auth via env, in precedence order — `PB_API_KEY` (**preferred**:
scoped + revocable, mint with `scripts/mint-apikey.sh`), then `PB_TOKEN`, then
`PB_EMAIL`/`PB_PASSWORD` (auto-renew on 401). The MCP key needs `roles:read
roles:write ai:use` (+ `schema:*`, `settings:*`, `records:write`) — not
`admin`/`keys:manage`. (`ai:use` gates `/api/ai/*`; a key minted before this scope
existed must be re-minted to use the AI tools.) Wired into Claude
Desktop at `~/Library/Application Support/Claude/claude_desktop_config.json`
(server name `pocketbase-superadmin`, alongside an existing `sentinel` server).
Restart Claude Desktop after rebuilding the binary.

## Frontend types

```bash
npx pocketbase-typegen --url http://localhost:8090 \
  --email admin@example.com --password SuperSecret123 --out ./pocketbase-types.ts
```
Regenerate after any schema change. `select` → TS union enums, `relation` → `RecordIdString`.

## Conventions & gotchas (learned the hard way)

- **PocketBase needs Go ≥ 1.25.** Older toolchains fail `go mod tidy`.
- **Litestream v0.5 ≠ v0.3**: single `replica:` field; binary assets use `x86_64`/`arm64`.
- **`PATCH /api/collections` replaces the WHOLE `fields` array** — the provision endpoint does fetch-merge-save so callers don't have to.
- **Provision relation ordering**: list a relation's target collection BEFORE the collection referencing it (loop saves in array order).
- **System collections are locked**: can't rename/delete/flip System after creation. You CAN add a *new non-system field* (that's how `scopes` was migrated onto `_apiKeys`).
- **Litestream lives IN the pocketbase container**, not a separate sidecar — restore must finish before PocketBase opens the DB file.
- **`pb_hooks/` JS is inert** in framework mode (JSVM plugin isn't registered).

## Security TODO before production

- [x] ~~Swap MinIO for real S3~~ — MinIO removed; `LITESTREAM_*` now come from `.env`. Point them at real S3 in prod.
- [x] ~~Move the MCP password out of `.mcp.json`~~ — now uses a scoped `PB_API_KEY` (mint with `scripts/mint-apikey.sh`); treat the filled-in key as a secret, don't commit it.
- [ ] Enable PocketBase `--encryptionEnv` so the settings blob (SMTP/S3 creds) isn't plaintext in the DB.
- [ ] Put PocketBase behind TLS.
- [x] ~~Per-collection API-key access~~ — done via RBAC: keys are roled service accounts gated by native per-collection rules (`docs/RBAC.md`). Scopes remain the *control-plane* gate only.
- [ ] Decide whether `viewer`/`editor` should auto-cover NEW collections (`*:<action>` wildcard tokens) or stay explicit per-collection.

## Dev credentials (CHANGE FOR PROD)

- Superuser: `admin@example.com` / `SuperSecret123`
- S3/Litestream creds: set in `.env` (see `.env.example`)
- Backups bucket: set `LITESTREAM_BUCKET` in `.env`

## Known leftover dev cruft

Demo collections from experimentation still in the DB: `articles`, `projects`,
`clients`, `tasks`, `mcp_demo`, `invoices`, `apikey_made_this`. Safe to delete.

See `docs/` for deeper architecture, a command runbook, the decision log, and the
RBAC / multi-role rules guide (`docs/RBAC.md`).

## Skills

- `.claude/skills/pocketbase-rbac-ops/` — playbook for any agent **operating** a
  live instance (provision schema, manage RBAC, mint roled keys; encodes the
  ask-the-user guardrails). Thin wrapper over the MCP tools + `docs/`.
- `.claude/skills/pocketbase-ai-proxy-ops/` — playbook for **operating the AI
  proxy** (configure providers, generate text/images, manage rate limits +
  quotas, enable S3 images; encodes the gpt-image-1 + don't-spend-without-asking
  guardrails). Thin wrapper over the `ai_*` MCP tools + `docs/AI-PROXY.md`.
- `.claude/skills/pocketbase-rbac-scaffold/` — generator that **recreates** this
  stack in a new project. Bundles verbatim `.tmpl` snapshots of the `*.go` + infra
  files; SKILL.md is the copy → set module → secrets → build → verify procedure.
  Templates are NOT auto-synced — re-copy them when the reference `*.go` change
  (sync snippet is in that SKILL.md). NOTE: the AI proxy files (`ai.go`,
  `aiimage.go`, `airatelimit.go`, `adminui.go`, `admin_ui.html`) are **not yet
  templated** — the scaffold recreates the RBAC platform only.
