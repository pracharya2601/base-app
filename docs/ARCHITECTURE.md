# Architecture

```
                ┌─────────────────────────────────────────────────────┐
   Claude  ─────┤ MCP server (mcp/pb-mcp, Go, stdio) — 9 tools         │
   Desktop      │   schema:  list_field_types, list_collections,       │
                │            provision_schema, create_record           │
                │   settings: get_settings, update_settings            │
                │   RBAC:    list_roles, manage_role, assign_user_roles │
                │            (ask-the-user guardrails in descriptions)  │
                └───────────────┬─────────────────────────────────────┘
                                │ HTTP + (PB_API_KEY | PB_TOKEN | password)
   Frontend ────────────────────┼──── GET /api/superadmin/field-types | scopes | permissions
     │ typegen / fetch catalog   │
     ▼                           ▼
   pocketbase-types.ts    ┌──────────────────────────────────────────────┐
                          │ PocketBase (framework mode, our main.go)      │
                          │  custom:   /api/superadmin/*                  │
                          │  apikeys.go:  X-API-Key middleware (2 planes) │
                          │  roles.go:    RBAC — _roles + _permissions    │
                          │  serviceaccounts.go: key→roled identity       │
                          │  standard: /api/collections, /records,        │
                          │            /settings                          │
                          │  storage: SQLite (WAL) in pb_data/            │
                          └───────────────┬──────────────────────────────┘
                                          │ Litestream (in same container)
                                          ▼
                              S3 target  (env-driven via .env; continuous WAL
                                          replication, restore-on-boot)
```

## Two control surfaces

- **Control plane** — schema, settings, key management (`/api/superadmin/*`,
  `/api/collections`, `/api/settings`). Gated by superuser auth + (for keys)
  coarse **scopes**. This is the operator/admin surface.
- **Data plane** — record CRUD (`/api/collections/{c}/records`). Gated by
  **RBAC**: native per-collection rules that read the caller's roles →
  permissions. Both human users and API keys (via their service account) flow
  through the *same* rules. This is the privacy/authorization surface.

## Components

### 1. PocketBase framework binary (`main.go`)
- `pocketbase.New()` + `OnServe()` hook registers everything.
- **Provision endpoint** (`/api/superadmin/provision`): declarative + idempotent.
  Creates collections, adds missing fields to existing ones, seeds records, sets
  appName. Supports field types: text, number, bool, email, select, relation.
- **Field-type catalog** (`fieldTypeCatalog()`): one self-describing contract
  consumed by the LLM (MCP), the frontend, and backend validation.

### 2. API-key system (`apikeys.go`)
- `_apiKeys` **system** collection (SHA-256 hashes only, never plaintext); each
  key carries `scopes` (control plane), `superuserId`, and `serviceAccountId`.
- `apiKeyAuthMiddleware` runs after the JWT loader, validates `X-API-Key` + expiry,
  then **splits by plane**:
  - **Record routes** → authenticates AS the key's **service account** (roled,
    non-superuser); native RBAC rules decide. No scope check here.
  - **Everything else** → checks the key's **scope** and authenticates as the
    minting superuser. `requiredScope(method, path)` maps a request to its scope.
- Mint accepts `scopes` + `roles`; the list endpoint resolves each key's role names.

### 3. RBAC (`roles.go`) — the data-plane authorization model
- `_permissions` **system** collection: one record per token (`token` field) —
  `"<collection>:<action>"`, a collection wildcard `"<collection>:*"`, or `"*"`.
- `_roles` **system** collection: `name`, `description`, and `permissions`
  (multi-relation → `_permissions`).
- `users.roles` / `_serviceAccounts.roles`: multi-relation → `_roles`. A caller's
  effective permissions = the **union** across all their roles.
- Enforcement is **native PocketBase per-collection rules** (no middleware):
  `@request.auth.roles.permissions.token ?= "<c>:<a>" || ... "<c>:*" || ... "*"`.
- **Auto-managed**: collection-create/delete hooks keep tokens + rules in sync
  (`registerPermissionSyncHooks`); `backfillRBAC` does the same at startup for
  pre-existing collections. New tables are governed by default. Full usage:
  `docs/RBAC.md`.

### 4. Service accounts (`serviceaccounts.go`)
- `_serviceAccounts` **system auth** collection with a `roles` relation matching
  `users.roles`. It is the roled identity an API key acts AS on the data plane,
  so the same rules gate keys and humans uniformly.
- One service account per key (random, non-loginable credentials — the API key is
  the real credential). Existing keys were migrated to an `admin`-role account.

### 5. Litestream (`entrypoint.sh`, `litestream.yml`)
- Runs as the container's supervisor: restore-if-missing → `replicate -exec`.
- Streams `pb_data/data.db` WAL to S3 every ~1s. RPO ≈ 1s. Recovery = restart.

### 6. MCP server (`mcp/main.go`)
- Thin `pbClient`. Sends `X-API-Key` when `PB_API_KEY` is set, else an
  `Authorization` token; auto re-auth on 401 if a password is set; static
  token/API-key fails loudly with a re-mint hint.
- Each tool = name + description + typed params so an LLM understands it.
- **Agentic RBAC**: `list_roles` / `manage_role` / `assign_user_roles` let an
  agent drive authorization, with descriptions that make it **ask the user**
  before granting permissions or assigning powerful roles — the human owns policy.

## Auth precedence (MCP client)
`PB_API_KEY` (scoped, **preferred**) → `X-API-Key`, fail-loud on revoke/expiry.
`PB_TOKEN` only → static impersonate token (fail-loud on expiry).
`PB_EMAIL`/`PB_PASSWORD` → auto-auth + auto-renew.
nothing → dev fallback to the demo superuser.

## Request → scope mapping (API keys, CONTROL plane)
Scopes gate the control plane only. On **record routes** the key is its service
account and RBAC rules decide — scopes are not consulted there.

| Path / method | Required scope |
|---|---|
| `/api/superadmin/apikeys*` | `keys:manage` |
| `/api/superadmin/provision` | `schema:write` |
| `/api/settings` GET / write | `settings:read` / `settings:write` |
| `/api/collections*` GET / write | `schema:read` / `schema:write` |
| anything else | `admin` |
`admin` satisfies everything. (`records:read`/`records:write` scopes remain for
legacy keys with no service account; roled keys use RBAC instead.)
