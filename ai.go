package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"

	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/anthropic"
	"github.com/zendev-sh/goai/provider/azure"
	"github.com/zendev-sh/goai/provider/cerebras"
	"github.com/zendev-sh/goai/provider/cloudflare"
	"github.com/zendev-sh/goai/provider/cohere"
	"github.com/zendev-sh/goai/provider/compat"
	"github.com/zendev-sh/goai/provider/deepinfra"
	"github.com/zendev-sh/goai/provider/deepseek"
	"github.com/zendev-sh/goai/provider/fireworks"
	"github.com/zendev-sh/goai/provider/fptcloud"
	"github.com/zendev-sh/goai/provider/google"
	"github.com/zendev-sh/goai/provider/groq"
	"github.com/zendev-sh/goai/provider/llamacpp"
	"github.com/zendev-sh/goai/provider/minimax"
	"github.com/zendev-sh/goai/provider/mistral"
	"github.com/zendev-sh/goai/provider/nvidia"
	"github.com/zendev-sh/goai/provider/openai"
	"github.com/zendev-sh/goai/provider/openrouter"
	"github.com/zendev-sh/goai/provider/perplexity"
	"github.com/zendev-sh/goai/provider/requesty"
	"github.com/zendev-sh/goai/provider/together"
	"github.com/zendev-sh/goai/provider/vertex"
	"github.com/zendev-sh/goai/provider/vllm"
	"github.com/zendev-sh/goai/provider/xai"
)

// AI proxy / orchestrator (rungs 0-2 of docs/AI-PROXY.md):
//   - /api/ai/{provider}/generate  — single LLM call, returns text + usage
//   - /api/ai/{provider}/stream    — same, streamed over SSE
//   - provider keys live encrypted in the _aiProviders system collection
//   - every call writes a metering row to _aiUsage
//   - inference routes require a normal authenticated user (JWT)
//
// goai (github.com/zendev-sh/goai) is the unified backend; {provider} selects
// the goai provider. Add a provider = one entry in knownProviders + its import +
// an _aiProviders row. No schema migration needed.

const (
	aiProviderCollection = "_aiProviders"
	aiUsageCollection    = "_aiUsage"
)

// aiProvider is one entry in the provider allowlist. build() constructs a goai
// language model for a given model id, API key, and optional base URL.
type aiProvider struct {
	desc  string
	build func(model, apiKey, baseURL string) provider.LanguageModel
}

// buildKeyBase adapts any goai provider whose constructor follows the uniform
// shape — Chat(model, ...O) + WithAPIKey(string) O + WithBaseURL(string) O — into
// our builder. Generics let ONE helper serve every such provider: the Option type
// O is package-specific, but the shape is identical, so the compiler infers O
// from the passed functions. baseURL is applied only when non-empty (otherwise
// the provider's default endpoint is used).
func buildKeyBase[O any](
	chat func(string, ...O) provider.LanguageModel,
	withKey func(string) O,
	withBase func(string) O,
) func(model, apiKey, baseURL string) provider.LanguageModel {
	return func(model, apiKey, baseURL string) provider.LanguageModel {
		opts := []O{withKey(apiKey)}
		if baseURL != "" {
			opts = append(opts, withBase(baseURL))
		}
		return chat(model, opts...)
	}
}

// buildKeyOnly is for providers without a WithBaseURL option (e.g. azure).
func buildKeyOnly[O any](
	chat func(string, ...O) provider.LanguageModel,
	withKey func(string) O,
) func(model, apiKey, baseURL string) provider.LanguageModel {
	return func(model, apiKey, _ string) provider.LanguageModel {
		return chat(model, withKey(apiKey))
	}
}

// knownProviders is the closed allowlist (mirrors apikeys.go's knownScopes).
// {provider} in the URL must be one of these keys. Editing this map (code, not
// schema) is how you add/remove a provider — each needs its goai subpackage
// import above. All take a single API key (+ optional baseUrl); the cloud-IAM
// providers (bedrock, vertex-via-GCP, ollama-local) are intentionally omitted —
// they need non-key auth and can be added on request.
var knownProviders = map[string]aiProvider{
	"anthropic":  {"Anthropic Claude.", buildKeyBase(anthropic.Chat, anthropic.WithAPIKey, anthropic.WithBaseURL)},
	"openai":     {"OpenAI GPT.", buildKeyBase(openai.Chat, openai.WithAPIKey, openai.WithBaseURL)},
	"google":     {"Google Gemini.", buildKeyBase(google.Chat, google.WithAPIKey, google.WithBaseURL)},
	"groq":       {"Groq (fast inference).", buildKeyBase(groq.Chat, groq.WithAPIKey, groq.WithBaseURL)},
	"mistral":    {"Mistral AI.", buildKeyBase(mistral.Chat, mistral.WithAPIKey, mistral.WithBaseURL)},
	"cohere":     {"Cohere Command.", buildKeyBase(cohere.Chat, cohere.WithAPIKey, cohere.WithBaseURL)},
	"deepseek":   {"DeepSeek.", buildKeyBase(deepseek.Chat, deepseek.WithAPIKey, deepseek.WithBaseURL)},
	"xai":        {"xAI Grok.", buildKeyBase(xai.Chat, xai.WithAPIKey, xai.WithBaseURL)},
	"perplexity": {"Perplexity.", buildKeyBase(perplexity.Chat, perplexity.WithAPIKey, perplexity.WithBaseURL)},
	"together":   {"Together AI.", buildKeyBase(together.Chat, together.WithAPIKey, together.WithBaseURL)},
	"fireworks":  {"Fireworks AI.", buildKeyBase(fireworks.Chat, fireworks.WithAPIKey, fireworks.WithBaseURL)},
	"openrouter": {"OpenRouter (multi-model gateway).", buildKeyBase(openrouter.Chat, openrouter.WithAPIKey, openrouter.WithBaseURL)},
	"deepinfra":  {"DeepInfra.", buildKeyBase(deepinfra.Chat, deepinfra.WithAPIKey, deepinfra.WithBaseURL)},
	"cerebras":   {"Cerebras.", buildKeyBase(cerebras.Chat, cerebras.WithAPIKey, cerebras.WithBaseURL)},
	"nvidia":     {"NVIDIA NIM.", buildKeyBase(nvidia.Chat, nvidia.WithAPIKey, nvidia.WithBaseURL)},
	"cloudflare": {"Cloudflare Workers AI.", buildKeyBase(cloudflare.Chat, cloudflare.WithAPIKey, cloudflare.WithBaseURL)},
	"minimax":    {"MiniMax.", buildKeyBase(minimax.Chat, minimax.WithAPIKey, minimax.WithBaseURL)},
	"requesty":   {"Requesty.", buildKeyBase(requesty.Chat, requesty.WithAPIKey, requesty.WithBaseURL)},
	"fptcloud":   {"FPT Cloud.", buildKeyBase(fptcloud.Chat, fptcloud.WithAPIKey, fptcloud.WithBaseURL)},
	"vllm":       {"vLLM (self-hosted; set baseUrl).", buildKeyBase(vllm.Chat, vllm.WithAPIKey, vllm.WithBaseURL)},
	"llamacpp":   {"llama.cpp (self-hosted; set baseUrl).", buildKeyBase(llamacpp.Chat, llamacpp.WithAPIKey, llamacpp.WithBaseURL)},
	"vertex":     {"Google Vertex AI (API key).", buildKeyBase(vertex.Chat, vertex.WithAPIKey, vertex.WithBaseURL)},
	"compat":     {"Any OpenAI-compatible endpoint (set baseUrl).", buildKeyBase(compat.Chat, compat.WithAPIKey, compat.WithBaseURL)},
	"azure":      {"Azure OpenAI (set baseUrl-equivalent via deployment).", buildKeyOnly(azure.Chat, azure.WithAPIKey)},
}

// ---- key encryption (reuse PB_ENCRYPTION_KEY) -------------------------------
// --encryptionEnv only encrypts PocketBase's settings blob, NOT custom collection
// fields. So we encrypt the provider key ourselves with the same 32-char key via
// AES-256-GCM (tools/security). Local/dev without the key stores plaintext (clearly
// prefixed) so it still boots; production sets PB_ENCRYPTION_KEY and gets ciphertext.

func aiEncryptKey(plaintext string) (string, error) {
	key := os.Getenv("PB_ENCRYPTION_KEY")
	if len(key) == 32 {
		ct, err := security.Encrypt([]byte(plaintext), key)
		if err != nil {
			return "", err
		}
		return "enc:" + ct, nil
	}
	return "plain:" + plaintext, nil
}

func aiDecryptKey(stored string) (string, error) {
	switch {
	case strings.HasPrefix(stored, "enc:"):
		key := os.Getenv("PB_ENCRYPTION_KEY")
		if len(key) != 32 {
			return "", fmt.Errorf("key is encrypted but PB_ENCRYPTION_KEY is not a 32-char key")
		}
		b, err := security.Decrypt(strings.TrimPrefix(stored, "enc:"), key)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case strings.HasPrefix(stored, "plain:"):
		return strings.TrimPrefix(stored, "plain:"), nil
	default:
		return stored, nil // legacy/unprefixed — treat as plaintext
	}
}

// ---- system collections -----------------------------------------------------

// ensureAIProvidersCollection creates the locked _aiProviders system collection.
// One row per provider: encrypted key + config. apiKeyEnc is never returned over
// the API (reads expose hasKey only).
func ensureAIProvidersCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(aiProviderCollection); err == nil {
		return nil
	}
	col := core.NewBaseCollection(aiProviderCollection)
	col.System = true
	col.Fields.Add(&core.TextField{Name: "provider", Required: true})
	col.Fields.Add(&core.BoolField{Name: "enabled"})
	col.Fields.Add(&core.TextField{Name: "apiKeyEnc"})    // AES-GCM ciphertext ("enc:" / "plain:")
	col.Fields.Add(&core.TextField{Name: "baseUrl"})      // optional, for compatible endpoints
	col.Fields.Add(&core.TextField{Name: "defaultModel"}) // optional fallback model
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnUpdate: true, OnCreate: true})
	col.AddIndex("idx_aiProviders_provider", true, "provider", "")
	return app.Save(col)
}

// ensureAIUsageCollection creates the locked _aiUsage metering collection: one
// row per call. Prompt/response CONTENT is intentionally not stored (privacy).
func ensureAIUsageCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(aiUsageCollection); err == nil {
		return nil
	}
	col := core.NewBaseCollection(aiUsageCollection)
	col.System = true
	col.Fields.Add(&core.TextField{Name: "provider"})
	col.Fields.Add(&core.TextField{Name: "model"})
	col.Fields.Add(&core.TextField{Name: "userId"})
	col.Fields.Add(&core.NumberField{Name: "promptTokens"})
	col.Fields.Add(&core.NumberField{Name: "completionTokens"})
	col.Fields.Add(&core.NumberField{Name: "totalTokens"})
	col.Fields.Add(&core.NumberField{Name: "latencyMs"})
	col.Fields.Add(&core.TextField{Name: "status"}) // ok | error
	col.Fields.Add(&core.TextField{Name: "errorMsg"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(col)
}

// ---- provider config + usage helpers ----------------------------------------

type aiProviderConfig struct {
	apiKey       string
	baseURL      string
	defaultModel string
}

// loadAIProvider returns the runtime config for an enabled, keyed provider, or a
// human-readable error suitable for a 400.
func loadAIProvider(app core.App, name string) (*aiProviderConfig, error) {
	rec, err := app.FindFirstRecordByFilter(aiProviderCollection, "provider = {:p}", dbx.Params{"p": name})
	if err != nil {
		return nil, fmt.Errorf("provider %q is not configured", name)
	}
	if !rec.GetBool("enabled") {
		return nil, fmt.Errorf("provider %q is disabled", name)
	}
	key, err := aiDecryptKey(rec.GetString("apiKeyEnc"))
	if err != nil {
		return nil, fmt.Errorf("provider %q key could not be decrypted: %w", name, err)
	}
	if key == "" {
		return nil, fmt.Errorf("provider %q has no API key set", name)
	}
	return &aiProviderConfig{
		apiKey:       key,
		baseURL:      rec.GetString("baseUrl"),
		defaultModel: rec.GetString("defaultModel"),
	}, nil
}

func logAIUsage(app core.App, providerName, model string, auth *core.Record, in, out int, latencyMs int64, status, errMsg string) {
	col, err := app.FindCollectionByNameOrId(aiUsageCollection)
	if err != nil {
		return
	}
	rec := core.NewRecord(col)
	rec.Set("provider", providerName)
	rec.Set("model", model)
	if auth != nil {
		rec.Set("userId", auth.Id)
	}
	rec.Set("promptTokens", in)
	rec.Set("completionTokens", out)
	rec.Set("totalTokens", in+out)
	rec.Set("latencyMs", latencyMs)
	rec.Set("status", status)
	if errMsg != "" {
		rec.Set("errorMsg", errMsg)
	}
	_ = app.Save(rec)
}

// ---- request shape + option building ----------------------------------------

type aiGenerateRequest struct {
	Model       string   `json:"model"`
	System      string   `json:"system"`
	Prompt      string   `json:"prompt"`
	MaxTokens   int      `json:"maxTokens"`
	Temperature *float64 `json:"temperature"` // pointer so 0 differs from unset
}

func aiBuildOptions(req aiGenerateRequest) []goai.Option {
	opts := []goai.Option{goai.WithPrompt(req.Prompt)}
	if req.System != "" {
		opts = append(opts, goai.WithSystem(req.System))
	}
	if req.MaxTokens > 0 {
		opts = append(opts, goai.WithMaxOutputTokens(req.MaxTokens))
	}
	if req.Temperature != nil {
		opts = append(opts, goai.WithTemperature(*req.Temperature))
	}
	return opts
}

// resolveAICall validates the provider, body, and model for both handlers.
func resolveAICall(app core.App, e *core.RequestEvent) (aiProvider, *aiProviderConfig, aiGenerateRequest, string, error) {
	name := e.Request.PathValue("provider")
	p, ok := knownProviders[name]
	if !ok {
		return aiProvider{}, nil, aiGenerateRequest{}, "", e.BadRequestError("unknown provider: "+name, nil)
	}
	cfg, err := loadAIProvider(app, name)
	if err != nil {
		return aiProvider{}, nil, aiGenerateRequest{}, "", e.BadRequestError(err.Error(), nil)
	}
	var req aiGenerateRequest
	if err := e.BindBody(&req); err != nil {
		return aiProvider{}, nil, aiGenerateRequest{}, "", e.BadRequestError("invalid request body", err)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return aiProvider{}, nil, aiGenerateRequest{}, "", e.BadRequestError("'prompt' is required", nil)
	}
	model := req.Model
	if model == "" {
		model = cfg.defaultModel
	}
	if model == "" {
		return aiProvider{}, nil, aiGenerateRequest{}, "", e.BadRequestError("'model' is required (no defaultModel configured for this provider)", nil)
	}
	return p, cfg, req, model, nil
}

// ---- routes -----------------------------------------------------------------

// registerAIProviderHooks makes the DASHBOARD (or any direct write to
// _aiProviders) a safe way to manage keys: if apiKeyEnc is set to a raw value
// with no enc:/plain: prefix, encrypt it on save — exactly like the admin route.
// Already-prefixed values (our route, or an unchanged existing row) are left
// alone, so this never double-encrypts.
func registerAIProviderHooks(app core.App) {
	normalize := func(e *core.RecordEvent) error {
		raw := e.Record.GetString("apiKeyEnc")
		if raw != "" && !strings.HasPrefix(raw, "enc:") && !strings.HasPrefix(raw, "plain:") {
			enc, err := aiEncryptKey(raw)
			if err != nil {
				return err
			}
			e.Record.Set("apiKeyEnc", enc)
		}
		return e.Next()
	}
	app.OnRecordCreate(aiProviderCollection).BindFunc(normalize)
	app.OnRecordUpdate(aiProviderCollection).BindFunc(normalize)
}

func registerAIRoutes(se *core.ServeEvent, app core.App) {
	registerAIProviderHooks(app)
	aiActiveLimits = aiLimitsFromEnv() // rate limit + per-user token quota (env-configured)

	// --- limits (authed): the caller's current rate/quota usage vs the configured
	//     ceilings. Superusers are exempt, so they report 0 used. ---
	se.Router.GET("/api/ai/limits", func(e *core.RequestEvent) error {
		uid := ""
		exempt := true
		if e.Auth != nil && !e.Auth.IsSuperuser() {
			uid = e.Auth.Id
			exempt = false
		}
		used := 0
		if uid != "" && aiActiveLimits.tokensPerDay > 0 {
			used, _ = aiTokensUsedSince(app, uid, time.Now().Add(-24*time.Hour))
		}
		reqs := 0
		if uid != "" {
			reqs = aiRateWindow.count(uid, time.Minute)
		}
		return e.JSON(http.StatusOK, map[string]any{
			"exempt":           exempt, // superuser / service-key callers aren't limited
			"ratePerMin":       aiActiveLimits.ratePerMin,
			"requestsLastMin":  reqs,
			"tokensPerDay":     aiActiveLimits.tokensPerDay, // 0 = unlimited
			"tokensUsedToday":  used,
		})
	}).Bind(apis.RequireAuth())

	// --- catalog (authed): the full provider allowlist + descriptions, so UIs
	//     can populate provider pickers. Pure capability metadata, no secrets. ---
	se.Router.GET("/api/ai/catalog", func(e *core.RequestEvent) error {
		out := make([]map[string]any, 0, len(knownProviders))
		for name, p := range knownProviders {
			out = append(out, map[string]any{"provider": name, "description": p.desc})
		}
		return e.JSON(http.StatusOK, map[string]any{"providers": out})
	}).Bind(apis.RequireAuth())

	// --- discovery (authed clients): which providers are usable right now ---
	se.Router.GET("/api/ai/providers", func(e *core.RequestEvent) error {
		out := make([]map[string]any, 0)
		for name := range knownProviders {
			rec, err := app.FindFirstRecordByFilter(aiProviderCollection, "provider = {:p}", dbx.Params{"p": name})
			if err != nil || !rec.GetBool("enabled") || rec.GetString("apiKeyEnc") == "" {
				continue
			}
			out = append(out, map[string]any{"provider": name, "defaultModel": rec.GetString("defaultModel")})
		}
		return e.JSON(http.StatusOK, map[string]any{"providers": out})
	}).Bind(apis.RequireAuth())

	// --- inference: single call -> text + usage ---
	se.Router.POST("/api/ai/{provider}/generate", func(e *core.RequestEvent) error {
		p, cfg, req, model, err := resolveAICall(app, e)
		if err != nil {
			return err
		}
		if lerr := enforceAILimits(app, e, aiActiveLimits); lerr != nil {
			return lerr
		}
		lm := p.build(model, cfg.apiKey, cfg.baseURL)
		start := time.Now()
		result, gerr := goai.GenerateText(e.Request.Context(), lm, aiBuildOptions(req)...)
		latency := time.Since(start).Milliseconds()
		if gerr != nil {
			logAIUsage(app, e.Request.PathValue("provider"), model, e.Auth, 0, 0, latency, "error", gerr.Error())
			return e.JSON(http.StatusBadGateway, map[string]any{"error": "provider error", "detail": gerr.Error()})
		}
		in, out := result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens
		logAIUsage(app, e.Request.PathValue("provider"), model, e.Auth, in, out, latency, "ok", "")
		return e.JSON(http.StatusOK, map[string]any{
			"model": model,
			"text":  result.Text,
			"usage": map[string]int{"promptTokens": in, "completionTokens": out, "totalTokens": in + out},
		})
	}).Bind(apis.RequireAuth())

	// --- inference: streamed over SSE ---
	se.Router.POST("/api/ai/{provider}/stream", func(e *core.RequestEvent) error {
		p, cfg, req, model, err := resolveAICall(app, e)
		if err != nil {
			return err
		}
		if lerr := enforceAILimits(app, e, aiActiveLimits); lerr != nil {
			return lerr
		}
		lm := p.build(model, cfg.apiKey, cfg.baseURL)
		start := time.Now()
		stream, gerr := goai.StreamText(e.Request.Context(), lm, aiBuildOptions(req)...)
		if gerr != nil {
			logAIUsage(app, e.Request.PathValue("provider"), model, e.Auth, 0, 0, time.Since(start).Milliseconds(), "error", gerr.Error())
			return e.JSON(http.StatusBadGateway, map[string]any{"error": "provider error", "detail": gerr.Error()})
		}

		w := e.Response
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		send := func(payload map[string]any) {
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}

		for chunk := range stream.TextStream() {
			send(map[string]any{"delta": chunk})
		}

		providerName := e.Request.PathValue("provider")
		latency := time.Since(start).Milliseconds()
		if serr := stream.Err(); serr != nil {
			logAIUsage(app, providerName, model, e.Auth, 0, 0, latency, "error", serr.Error())
			send(map[string]any{"error": serr.Error()})
			return nil
		}
		res := stream.Result()
		in, out := res.TotalUsage.InputTokens, res.TotalUsage.OutputTokens
		logAIUsage(app, providerName, model, e.Auth, in, out, latency, "ok", "")
		send(map[string]any{"done": true, "usage": map[string]int{"promptTokens": in, "completionTokens": out, "totalTokens": in + out}})
		return nil
	}).Bind(apis.RequireAuth())
}
