# AI proxy / orchestrator

> **Status: v1 BUILT (rungs 0–2) — 2026-06-23.** `/api/ai/{provider}/generate`
> and `/stream` are live in `ai.go`, wired in `main.go`. Verified end-to-end
> (auth → encrypted key → goai → real provider round-trip → `_aiUsage` metering).
> Tool-use and RAG remain **out of scope** (see "Deferred"). The sections below
> are the as-built reference; "Capability ladder" tracks what's next.

## Managing provider keys (hook-only shape)

There is **no custom provider-admin route**. Keys are written straight to the
`_aiProviders` records collection (superuser-only), and a **save-hook**
(`registerAIProviderHooks`, `OnRecordCreate`/`OnRecordUpdate`) encrypts the
`apiKeyEnc` field on every write path — route, dashboard-style record write, MCP,
script. Encryption lives in ONE place and can't be bypassed. Two ways to manage:

- **GUI:** `GET /admin/ai` — embedded page (`aiui.go` + `ai_ui.html`, mirrors
  `/admin/apikeys`). Superuser logs in client-side, uploads keys, and has a
  built-in Generate/Stream test box. It talks to the `_aiProviders` records API.
- **Raw:** `POST`/`PATCH /api/collections/_aiProviders/records` with a superuser
  token; set `apiKeyEnc` to the **raw** key (the hook encrypts it).

> The PocketBase dashboard CANNOT manage `_aiProviders` — it's `System=true`
> (keeps it out of RBAC backfill + protects the keys table), and the dashboard
> only renders its built-in system collections ("Missing collection context"
> otherwise). That's why `/admin/ai` exists.

## Quick start (as built)

```bash
# 1. Configure a key — raw key into apiKeyEnc; the save-hook encrypts it.
#    (Easiest: just use the /admin/ai page instead of this curl.)
curl -X POST $PB/api/collections/_aiProviders/records -H "Authorization: Bearer $SU_TOKEN" \
  -d '{"provider":"anthropic","apiKeyEnc":"sk-ant-...","defaultModel":"claude-sonnet-4-6","enabled":true}'

# 2. Any authenticated user calls the model (key never leaves the server):
curl -X POST $PB/api/ai/anthropic/generate -H "Authorization: Bearer $USER_JWT" \
  -d '{"prompt":"Say hi","maxTokens":64}'
#  -> { "model":"...", "text":"...", "usage":{promptTokens,completionTokens,totalTokens} }

# 3. Stream the same call (SSE):
curl -N -X POST $PB/api/ai/anthropic/stream -H "Authorization: Bearer $USER_JWT" \
  -d '{"prompt":"Write a haiku"}'
#  -> data: {"delta":"..."}  ...  data: {"done":true,"usage":{...}}

# Discovery:
GET /api/ai/providers   # authed: which providers are enabled + keyed right now
```

**24 providers wired** (`knownProviders` in ai.go) via a generic helper
`buildKeyBase[O](Chat, WithAPIKey, WithBaseURL)` — one helper covers every goai
provider with the uniform key+baseUrl shape, the Option type `O` inferred per
package: anthropic, openai, google, groq, mistral, cohere, deepseek, xai,
perplexity, together, fireworks, openrouter, deepinfra, cerebras, nvidia,
cloudflare, minimax, requesty, fptcloud, vllm, llamacpp, vertex, **compat** (any
OpenAI-compatible endpoint — set `baseUrl`), and **azure** (key-only). Omitted:
`bedrock` (AWS IAM), `ollama` (local/no key), `runpod` (needs endpointID). Add a
provider = its subpackage import + one map line; `GET /api/ai/catalog` (authed)
returns the full allowlist for UI pickers.

v1 request fields: `model` (optional if `defaultModel` set), `prompt` (required),
`system`, `maxTokens`, `temperature`. Multi-turn `messages` is a follow-up.

## Goal

Add an authenticated **AI gateway** to the existing PocketBase binary so that all
LLM traffic runs *through* base-app instead of clients calling providers
directly. The binary already is a gateway (custom routes behind an API-key /
RBAC middleware); this adds an `/api/ai/...` surface that reuses that edge.

The headline route:

```
POST /api/ai/:provider/generate
POST /api/ai/:provider/stream     (SSE)
```

`:provider` selects the backend (anthropic, openai, gemini, groq, ollama, …).

## Why in the binary (not a separate proxy)

base-app already owns auth, RBAC, the DB, and the encrypted-settings mechanism.
An AI route group inherits all of it for free, deploys as one image, and can read
provider keys + write usage records in-process. A standalone proxy would
re-implement the auth edge and add a hop. **Placement is still officially open**
(we said "decide later"), but this doc assumes in-binary; nothing here blocks
splitting it out later, since a separate service would just call these same
routes with an API key.

## Library: `goai` (`github.com/zendev-sh/goai`)

A Vercel-AI-SDK-style unified Go library. One API across 25+ providers, provider
subpackages, streaming via channels, structured output via generics, tool-use
loops, embeddings, prompt caching (Anthropic/OpenAI), MCP support, minimal deps.

Maps cleanly onto `:provider`:

```go
// :provider == "anthropic"  -> anthropic.Messages(model)
// :provider == "openai"     -> openai.Chat(model)
result, _ := goai.GenerateText(ctx, providerModel,
    goai.WithSystem(system),
    goai.WithPrompt(prompt),
)
// result.Text, result.Usage (prompt/completion tokens)
```

`go get github.com/zendev-sh/goai` adds it to `go.mod`. Provider subpackages are
imported per provider we enable.

---

## Architecture

```
client (browser / service)
   │  Authorization: Bearer <user JWT>            ← v1 auth = any authenticated user
   ▼
POST /api/ai/:provider/generate
   │
   ├─ apis.RequireAuth()                          reuse PB's JWT auth
   ├─ provider allowlist check (knownProviders)   unknown -> 400
   ├─ load provider config from _aiProviders      decrypt apiKey with PB_ENCRYPTION_KEY
   ├─ goai.GenerateText / StreamText              upstream call (key never leaves server)
   ├─ write _aiUsage record                        provider, model, user, tokens, latency
   └─ return { text, usage, model }  /  SSE stream
```

Two new system collections, one route group, one provider registry in code.

---

## Storage

### `_aiProviders` — provider credentials & config (system collection)

Why a collection (per decision): keys are editable via dashboard/API and rotate
without a redeploy. **Caveat:** `--encryptionEnv` only encrypts PocketBase's own
*settings* blob — it does **not** encrypt custom collection fields. So we encrypt
the key value ourselves with `pocketbase/tools/security` (AES-GCM) using the
**same** `PB_ENCRYPTION_KEY` (already a 32-char Render secret). One key, one
mechanism, write-only over the API (plaintext is never returned on read).

| field          | type   | notes                                                       |
|----------------|--------|-------------------------------------------------------------|
| `provider`     | text   | unique; e.g. `anthropic`, `openai`, `groq` — matches `:provider` |
| `enabled`      | bool   | off = route returns 400 for that provider                   |
| `apiKeyEnc`    | text   | AES-GCM ciphertext of the provider key (encrypt on write, decrypt in-process) |
| `baseUrl`      | text   | optional — for OpenAI-compatible / Ollama / self-hosted     |
| `defaultModel` | text   | optional fallback when request omits `model`                |
| `created`      | autodate |                                                           |

Locked system collection (like `_apiKeys`). Managed by superuser / a key with
`settings:write` via a small admin route; **`apiKeyEnc` is never returned** —
reads show `hasKey: true/false` only.

### `_aiUsage` — metering / audit (system collection)

Every call writes one row. This is the billing/quota/audit substrate.

| field             | type     | notes                              |
|-------------------|----------|------------------------------------|
| `provider`        | text     |                                    |
| `model`           | text     |                                    |
| `userId`          | text     | from the JWT (`@request.auth.id`)  |
| `promptTokens`    | number   |                                    |
| `completionTokens`| number   |                                    |
| `totalTokens`     | number   |                                    |
| `latencyMs`       | number   |                                    |
| `status`          | text     | `ok` / `error`                     |
| `errorMsg`        | text     | populated on failure               |
| `created`         | autodate |                                    |

(Prompt/response **content** is deliberately NOT stored by default — privacy.
Add an opt-in `logContent` flag later if needed.)

---

## Auth model (v1)

**Any authenticated user (JWT).** Routes bind `apis.RequireAuth()`. Guests get
401. This is the simplest gate and matches the decision. Note the existing
API-key middleware still runs first, so a valid `X-API-Key` also satisfies
`RequireAuth` (it authenticates as a service account / superuser) — that's fine.

**Not in v1 but easy later:** an `ai:use` permission token (RBAC) or an `ai:use`
API-key scope to gate *which* users/keys may spend tokens, plus per-user quota
enforced against `_aiUsage` before the upstream call. Hooks are already in place
to add this without restructuring.

---

## Route contracts (v1)

### `POST /api/ai/:provider/generate`
Request:
```json
{
  "model": "claude-sonnet-4-6",
  "system": "optional system prompt",
  "prompt": "single-turn text",
  "messages": [{ "role": "user", "content": "..." }],
  "temperature": 0.7,
  "maxTokens": 1024
}
```
- `prompt` OR `messages` (messages wins if both). `model` optional if the
  provider has a `defaultModel`.
- Response:
```json
{
  "model": "claude-sonnet-4-6",
  "text": "…",
  "usage": { "promptTokens": 42, "completionTokens": 88, "totalTokens": 130 }
}
```
- Errors: `400` unknown/disabled provider or bad body; `401` no auth;
  `502` upstream provider error (logged to `_aiUsage` with `status:error`).

### `POST /api/ai/:provider/stream`
Same body. Responds `text/event-stream` (SSE):
```
data: {"delta":"Hello"}
data: {"delta":" world"}
data: {"done":true,"usage":{...}}
```
Backed by goai `StreamText` (channel) flushed per chunk. Usage row written when
the stream closes.

### Provider registry (code, mirrors `knownScopes`)
```go
var knownProviders = map[string]func(model string) goai.Model{
    "anthropic": func(m string) goai.Model { return anthropic.Messages(m) },
    "openai":    func(m string) goai.Model { return openai.Chat(m) },
    // add a provider = one line + its subpackage import + an _aiProviders row
}
```
Adding a provider: add the line, import the subpackage, insert an `_aiProviders`
row with its key. No schema migration.

---

## Capability ladder

What this setup can extract, cheap+safe → complex. Each rung ships independently.

| Rung | Route | goai call | Value | v1? |
|------|-------|-----------|-------|-----|
| 0 Skeleton | `/api/ai/:provider/*` | — | auth, allowlist, key-hiding, `_aiUsage`, (rate-limit) | ✅ |
| 1 Generate | `…/generate` | `GenerateText` | prompt → text + tokens | ✅ |
| 2 Stream | `…/stream` | `StreamText` | token-by-token SSE | ✅ |
| 3 Structured | `…/extract` | `GenerateObject[T]` | typed JSON out (classify/extract) — extraction WITHOUT tool-use risk | next |
| 4 Embeddings | `…/embed` | embeddings | vectors into a collection — RAG foundation | next |
| 5 Tools / RAG | `…/agent` | tool loop / retrieval | the complex stuff | **deferred** |

**v1 = rungs 0–2.** That delivers the whole "everything runs through this proxy"
goal with the simplest LLM surface. Rung 3 (structured extraction) is the natural
follow-up and is where "what can we extract" gets concrete.

## Deferred (design separately before building)

- **Tool-use agent** (rung 5): a model calling tools that read/write PB
  collections through RBAC. Powerful but needs a security review (what can the
  model touch? whose auth does a tool run as?).
- **RAG** (rungs 4→5): embeddings + retrieval over collections. Needs a vector
  storage decision (SQLite + brute force vs an external vector store) and a
  chunking/ingest pipeline.
- Per-user **quotas** and content logging.

---

## What v1 touches (when approved)

- `go.mod` — add `github.com/zendev-sh/goai` (+ provider subpackages).
- New `ai.go` — `ensureAIProvidersCollection`, `ensureAIUsageCollection`,
  `knownProviders`, `registerAIRoutes`, encrypt/decrypt helpers.
- `main.go` `OnServe` — call the two `ensure*` funcs and `registerAIRoutes(se, app)`
  alongside the existing wiring.
- Provider rows seeded via the dashboard or a tiny `settings:write` admin route.
- Docs: this file + a line in `RUNBOOK.md`.

No changes to the existing API-key / RBAC / Litestream / encryption machinery.

## Open decisions (carried)

1. **Placement** — in-binary (assumed here) vs separate service. Revisit if
   streaming load competes with PB's connection pool.
2. **Orchestrator depth** — stop at structured extraction (rung 3) or go to
   tools/RAG (rung 5). Decide after v1 is in hand.
