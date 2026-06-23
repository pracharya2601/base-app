# Decision log

Why things are the way they are — so we don't relitigate them.

## Use PocketBase as a Go framework, not the prebuilt binary
We need custom routes and a real API-key system. Framework mode (`pocketbase.New()`
+ our `main.go`) gives full control and one portable binary. Cost: we maintain a
small app instead of dropping in a release.

## SQLite + Litestream, not Postgres
PocketBase has **no Postgres support** and the maintainer has no plans for it.
Forks exist but you inherit the fork forever. PocketBase is single-server by
design (the hook bus doesn't sync across nodes), so SQLite isn't the bottleneck.
Litestream → S3 gives durability + point-in-time recovery (~1s RPO) without HA
complexity. If we ever truly need HA failover, reconsider the whole core.

## Litestream in the same container (not a separate sidecar)
Restore-on-boot must complete **before** PocketBase opens the DB file. A separate
sidecar container can't guarantee that ordering; a supervisor process in the same
container can (`litestream replicate -exec`).

## Superadmin via a custom `/provision` endpoint, not raw API only
PocketBase's `PATCH /api/collections` replaces the whole `fields` array (a
footgun). The provision endpoint is declarative and idempotent: callers say
"make these exist," server does fetch-merge-save. Safe to run on every deploy.

## API keys: built ourselves (PocketBase has none)
Native options were inadequate:
- JWT from password: short-lived, needs the password stored.
- `auth-refresh`: rolling, but lapses if the process is down past expiry.
- **impersonate**: custom-duration but non-refreshable, opaque, unnamed,
  not individually revocable (only by rotating the whole account).
So we built a real key system: named, scoped, listable, individually revocable,
stored as SHA-256 hashes. `impersonate` is still used for the long-lived
*operator* token (`mint-token.sh`); API keys are for *services*.

## `_apiKeys` as a SYSTEM collection
Protects the key store from accidental deletion/rename — even via our own
provision endpoint. Trade-off: the collection is locked, so schema changes go
through code (we add new *non-system* fields, which the guard allows — that's how
`scopes` was added after the fact).

## Scopes as space-delimited text, not a select field
A `select` field's allowed values are fixed at creation, and the collection is
locked. Space-delimited text + a `knownScopes` map in code means adding a scope
is a code edit, no schema migration. Scope granularity is per route-family
(coarse) — fine for now; per-collection scopes are a future extension.

## Token-first auth for the MCP server
Storing the superuser password is the weak point. `PB_TOKEN` (impersonate) is
preferred; on expiry the server fails loudly with a re-mint hint rather than
silently needing a password. Optionally add a scoped service-account password
for hands-off renewal — never the human superuser's.

## MCP server in Go (mark3labs/mcp-go), stdio
Keeps one language/toolchain with the rest of the repo, and produces a native
binary Claude Desktop launches directly over stdio. HTTP/SSE transport is
possible later if we need remote access.

## Frontend types generated, not hand-written
`pocketbase-typegen` derives types from the live schema, so they can't drift.
`select` → TS union enums, `relation` → record-id strings. Regenerate after
schema changes (wire into a `package.json` script / CI).

## User RBAC via native PocketBase rules, not custom middleware
First built role enforcement as a Go middleware mirroring the API-key one. Switched
to PocketBase's **native per-collection rules** because they also cover realtime
subscriptions and the admin UI, need no custom code, and are visible per
collection. Multi-role "just works": `@request.auth.roles...` traverses a
multi-relation and the `?=` ("any-of") operator unions across all of a caller's
roles. Trade-off: rules must be authored per collection — solved by auto-generating
them (below).

## `_permissions` as records + relations (exact match), not delimited text
Permissions could be space-delimited text matched with `?~` (LIKE), but that risks
substring collisions (`articles:read` matching `Xarticles:read`). Normalizing into
a `_permissions` system collection related from `_roles` lets rules use `?=`
(exact). Tokens are `"<collection>:<action>"`, with `"<collection>:*"` and `"*"`
wildcards honored by extra `||` clauses in the generated rule.

## Governance is automatic (hooks + startup backfill)
Collection-create/delete hooks create/delete a table's CRUD tokens AND apply the
RBAC rules; `backfillRBAC` does the same at startup for pre-existing collections.
So **every non-system table is RBAC-governed by default** — you can't forget to
secure one. Guard: auto-apply only fires when all five rules are nil, so a
collection with explicit rules (e.g. `users`, which needs public signup +
self-access) is never clobbered. To keep a collection superuser-only, give it an
explicit non-nil rule.

## API keys are roled service accounts (no superuser bypass on data)
Keys used to authenticate AS the superuser → they bypassed every collection rule
(god mode). To make a key "only do certain things," each key now links to a
`_serviceAccounts` record — a **system auth collection** carrying a `roles`
relation. The middleware acts AS that service account on record routes, so the
*same* native rules gate keys and humans; a key with no roles can't touch records.
Control-plane routes still need superuser, so the middleware keeps superuser auth
there and only swaps to the service account on `/records`. Verified that native
rules resolve `@request.auth.roles...` for a non-`users` auth collection (rules
aren't hardcoded to `users`). Existing keys were migrated to an `admin`-role
account for backward-compatible full access; downscope by editing their account.

## MinIO removed; secrets via `.env`, not committed
Dropped the local MinIO/createbucket compose services. Litestream's `LITESTREAM_*`
config (incl. S3 creds) now comes from a gitignored `.env` (template: `.env.example`);
in prod, inject the same vars from a secret manager. Empty `LITESTREAM_BUCKET` ⇒
boot without replication (local-dev convenience). The MCP server likewise moved
off the stored password to a scoped, revocable `PB_API_KEY` (mint with
`scripts/mint-apikey.sh`).

## Gotchas that cost a rebuild (don't repeat)
- PocketBase v0.39 needs **Go ≥ 1.25** (build failed on 1.23).
- Litestream **v0.5** rewrote config (`replica:` not `replicas:`); assets are
  `x86_64`/`arm64`.
- Dockerfile must `COPY *.go` (not just `main.go`) once we added `apikeys.go`.
- Provision **relation target must be listed before** the referencing collection.
- A relation field's **target is immutable**. To repoint, **drop the field + save,
  then add a fresh field + save** — a single save reuses the field by name and
  silently keeps the old target.
- Relations **can** target **system** collections (e.g. `users.roles → _roles`).
- RBAC **denial codes are native, not 403**: failed create → **400**,
  update/delete → **404** (row filtered out), list → **empty**.
- For an RBAC rule, **always use `?=` (any-of)**, never `=`, when traversing the
  `roles`/`permissions` multi-relations.
