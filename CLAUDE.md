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
Layout: a `package main` CORE at the root (the tightly-coupled auth/RBAC/
provision platform — they share package-private types, so they stay one package),
plus self-contained feature packages under `internal/`. main.go is the thin
wiring that imports the feature packages. New features should follow this pattern:
a focused `internal/<feature>` package exporting a `Register*` entry point that
main.go calls. (Note: the root core is `package main` and so CANNOT be imported —
shared helpers a feature needs must live in the feature pkg or a shared package.)

ROOT (package main) — the platform core:
main.go              framework entry: OnServe wiring; blockDashboardMiddleware (BLOCK_PB_DASHBOARD=1 → redirect /_/ to /admin so operators only use the unified console; /api/* incl. auth stay open); provision endpoint is now a thin wrapper over internal/provision.Apply (maps spec errors→400, save errors→500); field-type catalog (registerProvisionRoutes)

internal/provision/ (package provision) — the importable core of superadmin provisioning, extracted out of package main so BOTH the HTTP endpoint AND in-process callers (the agentic ops executor) share one implementation. Exports Spec/CollectionSpec/FieldSpec/Rules (with jsonschema tags for tool use), Apply(app, Spec)->Result, ApplyRules, RBACRules, and InvalidSpecError (→400 vs 500). roles.go + apikeys_http_test.go now call provision.ApplyRules/RBACRules.

internal/ops/ (package ops) — the agentic ADMIN function: state intent in /admin → an "ops" agent (Ozzy) proposes a platform change behind the approval guard → human approves to apply. Tools (all PROPOSE; executor runs on approve only): propose_schema (over provision.CollectionSpec → provision.Apply), manage_role (create/upsert an RBAC role + its permission tokens), assign_user_roles (set a user's roles by email/id; roles must exist), and seed_records (insert records into an existing collection — reuses provision.Apply's Seed). The RBAC ones are minimal self-contained _roles/_permissions/users upserts; the canonical RBAC machinery stays in roles.go. Registered via the orchestrator seams (RegisterTaskTools/RegisterApproveAction/RegisterActionExecutor), zero engine edits. This is the path to "run everything from /admin via the agentic system": new admin capabilities = new tools the ops agent proposes.
apikeys.go           API-key system: _apiKeys system collection, scopes, auth middleware, throttled last-used stamp
roles.go             user RBAC: _roles + _permissions system collections, native-rule enforcement
serviceaccounts.go   _serviceAccounts auth collection; the roled identity an API key acts as on data routes
*_test.go            unit + integration + HTTP-level (tests.ApiScenario) tests for the core

internal/ai/ (package ai) — the AI proxy feature, self-contained:
  ai.go              24 goai providers, /api/ai/{provider}/generate + /stream, _aiProviders (encrypted keys) + _aiUsage (metering); exports EnsureProvidersCollection/EnsureUsageCollection/RegisterRoutes
  aiimage.go         image gen -> _aiImages file field + preview URLs; exports EnsureImagesCollection/RegisterImageRoutes
  airatelimit.go     AI rate limit (req/min, in-memory) + per-user token quota; env-configured
  airatelimit_test.go  rate-limiter + env unit tests

internal/adminui/ (package adminui) — the admin console, self-contained:
  adminui.go         serves the /admin SPA: GET /admin -> spa/index.html, /admin/assets/{path...} -> embedded JS/CSS (apis.Static), /admin/classic -> the old single-file console (fallback). Embeds spa/ (committed build output) + admin_ui.html.
  spa/               COMMITTED Vite+Svelte build output (from ../frontend). Committed so `go build` works without npm; the Dockerfile rebuilds it fresh (node stage).
  admin_ui.html      the CLASSIC single-file console (now at /admin/classic) — still hosts AI Providers / Images / API Keys tabs not yet ported to the SPA; hash-routed (#providers/#keys/…).
  keysui.go / aiui.go  /admin/apikeys, /admin/ai -> redirect to /admin/classic#keys / #providers (where those tabs live until ported)

frontend/ (Vite + Svelte SOURCE for the /admin SPA — NOT package main) — replaces the hand-maintained HTML string. `cd frontend && npm install && npm run build` outputs to internal/adminui/spa (vite base=/admin/). Native look via theme.css (PocketBase design tokens, copied from admin_ui.html). Views ported so far: Orchestrator (status + autopilot toggle + Ops command box + task queue + detail with proposed-actions + approve/revise/reject) and Data (read-only collection/records browse). AI Providers/Images/API Keys/Settings still live in the classic console (linked from the SPA nav). lib/api.js = fetch wrapper + superuser login (token in sessionStorage). After editing frontend/, rebuild + commit spa/.

internal/orchestrator/ (package orchestrator) — the "AI agent company":
  schema.go          _agents/_tasks/_runs system collections (+ owner field for multi-tenancy; _tasks.kind for action dispatch) + _orchConfigs (per-tenant config overrides, owner-keyed) + _proposedActions (write-tool side effects awaiting approval) + SeedTeam/SeedTeamForOwner (PM/engineer/reviewer; per-tenant idempotent) + SeedAgent. Multi-tenancy slice 1 = data model only; tick still global (owner="" = legacy/system tenant)
  orchestrator.go    always-on tick loop: claim a pending task, run its agent via ai.Generate, draft -> needs_review, log a _run; daily token-budget guard; self-correcting rework loop (re-queued task carries prior draft + feedback, capped by ORCH_MAX_REVISIONS); in autopilot a reviewer "VERDICT: CHANGES_REQUESTED" auto-sends work back to the author (autopilotReviewGate/parseVerdict) instead of shipping it; terminal failure (no retry-drain)
  config.go          env knobs (ORCH_ENABLED/INTERVAL/MAX_TOKENS/MAX_TOOL_STEPS/MAX_REVISIONS/DAILY_TOKEN_BUDGET/PROVIDER/MODEL) + the role pipeline (nextRole) + DB-DRIVEN CONFIG: loadOrchConfig(app, owner) overlays the per-tenant _orchConfigs row on the env defaults (tick re-reads every tick → live edits, no restart), upsertOrchConfig writes it. autopilot now lives in _orchConfigs (not an in-memory global); numbers/strings overlay only when set, enabled stays env-only. Lookup uses FindFirstRecordByData (matches owner="" — the PB filter-string parser doesn't).
  routes.go          superuser routes: POST tasks, tasks/{id}/approve (advance + hand off to next role), /revise (rework with feedback), /reject; POST autopilot (persists to _orchConfigs); GET+POST config (read/partial-update the DB-driven config); GET status, GET tasks (list, filter by state/agent/role, paginated), GET tasks/{id} (detail: draft + parent lineage + runs + proposedActions)
  actions.go         THE GENERALIZATION SEAM that turns the engine from a fixed software pipeline into a generic "company function" runner. A `kind` on each task selects behavior: kind="" = the software pipeline (linear handoff); a registered kind runs its ApproveAction on approve INSTEAD of a handoff and is never auto-advanced under autopilot (human must approve real side effects). THREE registries: RegisterApproveAction(kind, fn) (what approve DOES) + RegisterTaskTools(kind, provider) (tools the agent may CALL while drafting — goai auto tool loop via ai.GenerateWithTools, gated by ORCH_MAX_TOOL_STEPS) + RegisterActionExecutor(actionKind, fn) (runs a PROPOSED side effect on approval). WRITE-TOOL GUARD: a mutating tool calls ProposeAction(...) to record an intended side effect in _proposedActions instead of acting mid-draft; on APPROVE the engine runs every pending proposal (executeProposedActions) before the ApproveAction; on REJECT/REVISE they're discarded. So mutations stay behind the human gate. Also exports EnqueueTask, HasApproveAction, HasTaskTools, SeedAgent. Engine never imports the feature pkg — it calls back through registered funcs.
  *_test.go          unit (pipeline/config/action-registry) + integration (schema/seed + approve-handoff via ApiScenario)

internal/support/ (package support) — the FIRST real "company function" on the engine: autonomous customer support (NOT coding). Self-contained, plugs in via the actions.go seam, does not modify the engine:
  support.go         support_tickets collection (public-create contact form; budget guard backstops spend) + seeded "support" agent. TRIGGER: new ticket -> EnqueueTask(kind=support_reply) with the customer msg + their recent-ticket history as context. STATE MIRROR: a _tasks update hook reflects task state onto the ticket (drafting/awaiting_approval/resolved). APPROVE-ACTION (sendReply): approving the drafted reply emails it (PB SMTP if configured, else records it) + resolves the ticket. The agent drafts; a human approves; the action makes it real. This is the template future functions (orders/bookings) copy.
  support_orders.go  the agent's abilities: an `orders` collection + a READ tool lookup_orders (queries orders by customerEmail) AND a WRITE tool issue_refund — registered for support_reply via RegisterTaskTools. issue_refund does NOT mutate: it PROPOSES a refund (orchestrator.ProposeAction) so it only runs when a human approves the reply; executeRefund (RegisterActionExecutor) does the real mutation (order->refunded) on approval. The draft runs goai's auto tool loop. Demo orders seeded only when SUPPORT_SEED_DEMO is set (prod DB stays clean).
  *_test.go          integration: ticket trigger -> task, state mirror, approve-action resolves ticket; lookup_orders query + tool Execute + registry

internal/bookings/ (package bookings) — the SECOND "company function", built to prove the template generalizes: an appointment/booking desk. Uses ONLY the engine's exported seams with ZERO changes to internal/orchestrator (a new function = a new package + ~4 lines in main.go). booking_requests + bookings collections, seeded "scheduler" agent. TRIGGER: new request -> EnqueueTask(kind=booking_reply). Tools: check_availability (READ, capacity vs existing bookings) + confirm_booking (WRITE — proposes via ProposeAction, refuses full dates). APPROVE-ACTION sends the reply; the proposed booking's executor CREATES a bookings record on approval only (vs support's write-tool which UPDATES). State mirror like support.

internal/ai/generate.go  ai.Generate / ai.GenerateWithTools(...) — in-process LLM call (no HTTP) the orchestrator uses; the WithTools variant runs goai's auto tool loop. Re-exports ai.Tool + ai.NewTool[In] so feature pkgs define tools without importing goai.

INFRA & TOOLING:
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
scripts/test-superadmin-api.sh  raw-curl walkthrough of the superuser API
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
| GET | `/admin` | unified console (AI Providers + Images + API Keys + Orchestrator) |

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
- [x] ~~Enable PocketBase `--encryptionEnv` so the settings blob (SMTP/S3 creds) isn't plaintext in the DB.~~ — `entrypoint.sh` passes `--encryptionEnv=PB_ENCRYPTION_KEY` when the key is set (settings blob) and the AI proxy encrypts provider keys with the same key; a boot-time guard in `main.go` (`encryptionKeyStatus`) logs a loud WARNING if the key is missing/malformed so prod can't silently store secrets in plaintext. **Set a 32-char `PB_ENCRYPTION_KEY` in prod.**
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
  (sync snippet is in that SKILL.md). NOTE: the feature packages `internal/ai/`
  and `internal/adminui/` are **not yet templated** — the scaffold recreates the
  RBAC platform (root `package main`) only.
