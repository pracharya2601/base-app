# base-app

**PocketBase, extended in Go into a programmable backend platform — one binary you can drive entirely from an API, a frontend, or an LLM.**

> ⚠️ **Status: experimental / pre-1.0.** This is an actively evolving exploration, not a finished product. The auth/RBAC core is tested and deployed; newer pieces (AI proxy, orchestration) are younger. Don't run it for anything critical without reading [Security & production](#security--production).

---

## What this is

[PocketBase](https://pocketbase.io) is a fantastic single-binary backend (SQLite + auth + REST + admin UI). But the prebuilt binary is a *product* — you configure it through its dashboard.

**base-app uses PocketBase as a Go *framework*** and compiles a custom binary, so the whole backend becomes *programmable*: schema, roles, API keys, settings, and AI calls are all driven through code and API instead of clicking around a UI. The goal is a backend you can **stand up, extend, and operate without ever touching the dashboard — and increasingly, without redeploying.**

It still ships as one container, with SQLite streamed to S3/R2 for durability.

## Why

A few beliefs drive this:

- **The dashboard shouldn't be the source of truth.** Everything an admin can do should be an API call — so it's scriptable, reviewable, and automatable.
- **LLMs and agents are first-class operators.** A built-in [MCP server](#mcp-server) lets an AI coding agent provision schema, manage RBAC, and run AI generations safely.
- **One binary, real durability.** No separate DB server; Litestream continuously replicates SQLite to object storage and restores on boot.
- **The long game: change behavior without redeploying.** Move logic into *data* (config, rules, workflows) so the running system can be reconfigured from the frontend. See the [Roadmap](#roadmap).

## What works today

| Area | Status | Notes |
|---|---|---|
| PocketBase in Docker, persistent data | ✅ | framework-mode custom `main.go` |
| **API-driven superadmin** | ✅ | provision collections/fields/rules/seed via one endpoint |
| **Production durability** | ✅ | Litestream → S3/R2, restore-on-boot |
| **Generated frontend types** | ✅ | `pocketbase-typegen` against the live instance |
| **MCP server** (LLM-drivable) | ✅ | 12 tools, wired into Claude Desktop / Claude Code |
| **Token & API-key auth** | ✅ | scoped, roled, revocable keys — stored as SHA-256 hashes |
| **RBAC** for users *and* keys | ✅ | native per-collection rules, auto-governed — [docs](docs/RBAC.md) |
| **AI proxy** over 24 providers | ✅ | text/stream/image, encrypted keys, usage metering, rate limits — [docs](docs/AI-PROXY.md) |
| **Automated tests + CI gate** | ✅ | unit + integration + HTTP-level; `go test -race` gates the image build |
| **Load-test harness** | ✅ | zero-dependency Go load generator — [`loadtest/`](loadtest/) |
| Orchestration / workflows | 🚧 | exploring — see [Roadmap](#roadmap) |
| User-deployable functions | 🔬 | researching (WASM sandbox) — see [Roadmap](#roadmap) |

## Quick start

```bash
cp .env.example .env            # S3/R2 creds for backups, or leave blank to run without replication
docker compose up -d --build    # start pocketbase (+litestream)

# Dashboard (optional):  http://localhost:8090/_/
# Dev superuser:         admin@example.com / SuperSecret123   ← CHANGE FOR PROD
```

Drive it from the API (no UI needed):

```bash
./test-superadmin-api.sh        # auth → create a collection → insert → read → set appName
```

Regenerate frontend types after any schema change:

```bash
npx pocketbase-typegen --url http://localhost:8090 \
  --email admin@example.com --password SuperSecret123 --out ./pocketbase-types.ts
```

## The custom API surface

On top of every standard PocketBase endpoint, base-app adds:

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/superadmin/provision` | one call: create collections / add fields / set rules / seed / appName (idempotent) |
| GET | `/api/superadmin/field-types` · `/scopes` · `/permissions` | self-describing catalogs (the shared contract for frontend + LLM) |
| GET/POST | `/api/superadmin/roles` · `/users/roles` | manage RBAC roles and user assignments |
| POST/GET/DELETE | `/api/superadmin/apikeys` | mint (plaintext shown once) / list / revoke scoped keys |
| POST | `/api/ai/{provider}/generate` · `/stream` · `/image` | AI proxy over 24 providers ([docs](docs/AI-PROXY.md)) |
| GET | `/admin` | unified console (AI providers + images + API keys) |

## Auth model (3 ways in)

1. **Password → JWT** (superuser login). Dev only.
2. **Long-lived impersonate token** (`scripts/mint-token.sh`) — preferred over storing a password.
3. **API keys** (`X-API-Key: pbk_…`) — scoped, roled, revocable. The middleware splits by plane:
   - **Control plane** (schema/settings/keys): enforces the key's **scope**, acts as a superuser.
   - **Data plane** (records): acts as the key's **service account**, so the *same RBAC rules that gate users gate the key* — no superuser bypass.

Full model and rule patterns in [`docs/RBAC.md`](docs/RBAC.md).

## MCP server

A stdio MCP server (`mcp/`) exposes 12 tools so an AI agent can operate the backend: `provision_schema`, `list_collections`, RBAC tools (`list_roles`, `manage_role`, `assign_user_roles`), and AI-proxy tools (`ai_generate_text`, `ai_generate_image`, …) — driving the proxy **without ever holding a provider key**. The write tools are prompted to treat access changes as policy decisions and *ask the user* first.

## Quality & testing

The auth/RBAC core — the most dangerous thing to get wrong — has real coverage:

- **Unit** — scope mapping, key hashing, the rate-limiter sliding window (incl. a concurrency test), RBAC helpers.
- **Integration** (`tests.NewTestApp`) — system-collection setup, idempotency, governance backfill.
- **HTTP-level** (`tests.ApiScenario`) — the API-key middleware (scope gate + data-plane RBAC, proving *scope ≠ data access*) and the provision endpoint end-to-end.
- **CI gate** — `go vet` + `go test -race` must pass before the image is built and published.

```bash
go test ./...          # fast
go test -race ./...     # what CI runs
./loadtest/run.sh       # traffic / load test (see loadtest/README.md)
```

## Roadmap

base-app is built in rungs — each one makes the backend more operable from the outside. Where it's going:

**Near-term — harden & cover**
- Finish the production-readiness checklist (see [Security & production](#security--production)).
- Extend HTTP-level tests to the AI-proxy handlers.
- Act on load-test findings (e.g. the per-request API-key write was throttled after load testing showed it ~halved keyed-read throughput).

**Mid-term — the orchestration engine (the big bet)**
The thesis: let admins compose backend automations **from the frontend, without redeploying**, by storing workflows as *data* (not code) and interpreting them with a fixed, safe node catalog (AI call, record CRUD, HTTP request, branch, transform…). Runs are persisted and observable; workflows run as a roled service account so RBAC bounds their blast radius. Declarative and bounded — robustness over raw power.

**Research — user-deployable functions**
A more powerful (and much harder) direction: let people deploy *custom controller code*. The hard part is safely sandboxing untrusted code — the standard answer is **WebAssembly** (Wasmtime / extism). We're evaluating whether to build this on PocketBase or adopt a Rust foundation that ships a WASM runtime natively (e.g. [TrailBase](https://github.com/trailbaseio/trailbase)). This decision likely settles the build-vs-adopt question for the whole platform.

> Want to shape the direction? Open an issue — the orchestration-vs-functions fork is genuinely open.

## Project layout

A **thin platform core** at the root plus **self-contained feature packages** under
`internal/` — the pattern to copy when you build on top:

```
main.go                 entry point: OnServe wiring that imports the feature packages
apikeys.go roles.go …   the platform core (package main): API keys, RBAC,
serviceaccounts.go      service accounts, provision — tightly coupled, one package
*_test.go               unit + integration + HTTP-level tests for the core
internal/
  ai/                   the AI proxy feature (package ai) — exports Register*/Ensure*
  adminui/              the /admin console (package adminui) — exports Register*
mcp/                    the MCP server (separate binary)
loadtest/               the load-test harness
docs/                   architecture, RBAC, AI proxy, runbook, decisions
```

**To add a feature:** create `internal/<feature>/`, expose a `Register*` entry
point, and call it from `main.go`'s `OnServe`. Keep it self-contained — the root
core is `package main` and can't be imported, so a feature shouldn't reach into
it (shared helpers belong in the feature package or a dedicated shared package).

## Architecture & docs

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — how the pieces fit
- [`docs/RBAC.md`](docs/RBAC.md) — the role/permission model and rule patterns
- [`docs/AI-PROXY.md`](docs/AI-PROXY.md) — providers, encryption, metering, limits
- [`docs/RUNBOOK.md`](docs/RUNBOOK.md) — operational commands
- [`docs/DECISIONS.md`](docs/DECISIONS.md) — the decision log
- [`CLAUDE.md`](CLAUDE.md) — the contributor source-of-truth (file map, gotchas, conventions)

## Stack

| Component | Version | Notes |
|---|---|---|
| PocketBase | v0.39.4 | as a Go framework; needs **Go ≥ 1.25** |
| Go | 1.25 (build) | pure-Go SQLite, `CGO_ENABLED=0` |
| Litestream | v0.5.12 | SQLite → S3/R2 streaming replication |
| MCP SDK | `mark3labs/mcp-go` | stdio transport |
| AI | `zendev-sh/goai` | 24 providers behind the proxy |

## Security & production

This is pre-1.0 — treat the defaults as **development** settings:

- **Change the dev superuser** (`admin@example.com` / `SuperSecret123`) before any real deployment.
- **Inject secrets from a vault**, not a shipped `.env`. `.env`, `.mcp.json`, and `pb_data/` are gitignored.
- Provider keys are AES-encrypted at rest; manage them via `/admin` or the records API, **not** the dashboard.
- Open production items (TLS termination, settings-blob encryption verification, scaling the in-memory rate limiter beyond one instance) are tracked in [`CLAUDE.md`](CLAUDE.md) and `docs/`.

## License

Not yet specified — a license will be added before this is recommended for reuse. Until then, treat it as source-available for evaluation. (If you'd like to use it, open an issue.)
