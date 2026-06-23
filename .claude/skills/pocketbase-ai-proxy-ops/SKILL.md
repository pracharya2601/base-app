---
name: pocketbase-ai-proxy-ops
description: >
  Operate base-app's AI proxy — configure providers, generate text/images
  through /api/ai/*, manage rate limits + per-user token quotas, and read usage.
  Use when wiring an app/agent to the proxy, adding a provider key, generating
  text or images, debugging 4xx/429/502 from the AI routes, or enabling S3 image
  storage. Procedural playbook; full reference in docs/AI-PROXY.md.
---

# Operating base-app's AI proxy

The proxy fronts LLM/image providers so callers never hold a provider key — the
server holds the (encrypted) key and you call it by provider name. Built on
`github.com/zendev-sh/goai`. Code: `ai.go` (text), `aiimage.go` (image),
`airatelimit.go` (limits). Full detail: **docs/AI-PROXY.md**.

## Two ways to drive it
- **MCP** — `pocketbase-superadmin` server: `ai_list_providers`,
  `ai_generate_text`, `ai_generate_image`. Needs a key with the **`ai:use`** scope.
- **Raw HTTP** — base `http://localhost:8090` (prod: `https://pocketbase.easypezy.com`),
  any authenticated user (JWT) or an `ai:use` API key.

## Routes
- `POST /api/ai/{provider}/generate` → `{text, usage}` ; `/stream` → SSE
  (`data:{delta}` … `data:{done,usage}`).
- `POST /api/ai/{provider}/image` → `{images:[{id,url,mediaType}]}`; `url` is a
  `/api/files/_aiImages/...` preview link.
- `GET /api/ai/catalog` (all text providers), `/api/ai/image-catalog` (openai,
  google, vertex, azure), `/api/ai/providers` (usable now), `/api/ai/limits`
  (caller's quota usage).
- Request body: `provider` (path), `prompt` (req), `model` (opt if defaultModel
  set), `system`, `maxTokens`, `temperature` (text); `size`, `count` (image).

## Mental model — the 6 things to know
1. **Keys live in `_aiProviders`**, AES-encrypted at rest by a save-hook (reuses
   `PB_ENCRYPTION_KEY`). One row per provider: `provider`, `enabled`, `apiKeyEnc`
   (write the RAW key — the hook encrypts), `baseUrl`, `defaultModel`. It's a
   **system collection → NOT editable in the PB dashboard** ("Missing collection
   context"); manage via the `/admin` console or the records API.
2. **24 text providers, 4 image providers.** Add one = a line in `knownProviders`
   (ai.go) + its goai subpackage import. `compat` = any OpenAI-compatible endpoint
   (set `baseUrl`).
3. **Auth = any JWT.** API keys reach the routes via the `ai:use` scope (acts as
   superuser there). Mint with `scripts/mint-apikey.sh` (includes `ai:use`).
4. **Limits** (`airatelimit.go`, before the upstream call, 429 over limit):
   `AI_RATE_LIMIT_PER_MIN` (default 60) + `AI_TOKEN_QUOTA_PER_DAY` (default 0=off,
   set to cap spend). **Superusers and API keys are EXEMPT** — limits target
   end-user JWTs. `_aiUsage` meters every call.
5. **Errors:** unknown/disabled/keyless provider → **400**; over a limit → **429**;
   the upstream provider rejected it (bad model id, bad key) → **502** with the
   detail. Image model on the text `/generate` route → 502 (wrong endpoint).
6. **Images need S3 for durability.** They store in the `_aiImages` file field →
   wherever PocketBase Files storage points: **local disk (ephemeral on Render!)**
   until you enable **Settings → Files storage → S3 (R2)**. No code change — the
   file field auto-routes; the preview URL is served by PocketBase either way.

## Playbooks

### Configure a provider key — ⚠️ confirm with the user
Adding/replacing a provider API key spends real money on that account. Don't
invent it. Use the `/admin#providers` console (paste raw key) or:
`POST /api/collections/_aiProviders/records` (superuser) with `apiKeyEnc` = raw
key. Set `defaultModel`. Verify with `GET /api/ai/providers`.

### Generate text / image
Pick a usable provider from `/api/ai/providers` (or `ai_list_providers`). Then
`/generate` (or `/stream`) / `/image`. **OpenAI images: use `gpt-image-1`, NOT
`dall-e-3`** — goai sends a `response_format` param dall-e-3's endpoint rejects
(502). Image `count`>1 → one `_aiImages` record + preview URL per image.

### Enable S3 image storage (R2)
Settings → Files storage → S3: endpoint `https://<acct>.r2.cloudflarestorage.com`,
region `auto`, force path style ON, R2 creds. After that, new images land on R2;
verify the new file is NOT under `pb_data/storage` and the preview URL still 200s.

### Set a spend cap
Set `AI_TOKEN_QUOTA_PER_DAY` (env, per-user/24h) on the deployment. Check a
caller's standing via `GET /api/ai/limits`.

### Debug an AI 4xx/5xx
400 → provider not in allowlist / disabled / no key / missing prompt|model.
429 → rate or token-quota exceeded (see `/api/ai/limits`; superusers exempt).
502 → upstream error; read `detail` (bad key, bad model, wrong endpoint).

## Hard rules
- **Never add/replace a provider key on your own initiative** — it spends money;
  confirm with the user.
- Write the RAW key to `apiKeyEnc`; never pre-encrypt (the hook does it).
- Don't point users at the PB dashboard for `_aiProviders`/`_aiImages`/`_aiUsage`
  (system collections) — use `/admin` or the records API.
- For OpenAI images default to `gpt-image-1`.

## Reference
- Full design, routes, storage, limits, gotchas: **docs/AI-PROXY.md**
- Overall stack + endpoints: `CLAUDE.md`
