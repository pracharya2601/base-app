package ai

import (
	"context"
	"fmt"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/zendev-sh/goai"
)

// GenerateResult is the outcome of an in-process LLM call.
type GenerateResult struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// TotalTokens is prompt + completion.
func (r GenerateResult) TotalTokens() int { return r.PromptTokens + r.CompletionTokens }

// Tool is a function the model can call mid-generation. It re-exports goai.Tool so
// feature packages define tools through this package and never import goai directly.
type Tool = goai.Tool

// NewTool builds a Tool from a typed input struct and an execute function. The
// JSON schema is generated from In; the model's JSON args are unmarshaled into In
// before execute runs. Use struct{} for a no-arg tool. The execute return string
// (or error string) is fed back to the model as the tool result — do NOT put
// secrets in it, it's sent to the provider.
//
//	ai.NewTool("get_order", "Look up an order by number",
//	    func(ctx context.Context, in struct {
//	        Number string `json:"number" jsonschema:"description=order number"`
//	    }) (string, error) { return lookup(in.Number), nil })
func NewTool[In any](name, description string, execute func(ctx context.Context, in In) (string, error)) Tool {
	return goai.NewTool(name, description, execute)
}

// Generate runs a single LLM call IN-PROCESS (no HTTP round-trip), for trusted
// internal callers like the orchestrator. It resolves the provider's stored +
// decrypted key/config, falls back to the provider's defaultModel when model is
// "", records a metering row in _aiUsage (with no userId — it's a system call),
// and returns the text + token usage. maxTokens <= 0 uses the provider default.
//
// Unlike the HTTP routes, Generate does NOT enforce per-user rate/quota limits —
// the caller is the system, so spend governance is the caller's responsibility.
func Generate(ctx context.Context, app core.App, providerName, model, system, prompt string, maxTokens int) (GenerateResult, error) {
	return GenerateWithTools(ctx, app, providerName, model, system, prompt, maxTokens, nil, 0)
}

// GenerateWithTools is Generate with an optional auto tool loop. When tools are
// supplied, goai runs generate → execute tool(s) → re-generate, up to maxSteps
// turns (clamped to >=2 so at least one tool round-trip can happen; 0 tools makes
// it identical to Generate). The metered token usage (result.TotalUsage) already
// sums every step, so the returned usage covers the whole loop. Same governance
// note as Generate: no per-user rate/quota — the caller is the system.
func GenerateWithTools(ctx context.Context, app core.App, providerName, model, system, prompt string, maxTokens int, tools []Tool, maxSteps int) (GenerateResult, error) {
	p, ok := knownProviders[providerName]
	if !ok {
		return GenerateResult{}, fmt.Errorf("unknown provider: %s", providerName)
	}
	cfg, err := loadAIProvider(app, providerName)
	if err != nil {
		return GenerateResult{}, err
	}
	if model == "" {
		model = cfg.defaultModel
	}
	if model == "" {
		return GenerateResult{}, fmt.Errorf("no model given and provider %q has no defaultModel configured", providerName)
	}

	req := aiGenerateRequest{Prompt: prompt, System: system, Model: model, MaxTokens: maxTokens}
	opts := aiBuildOptions(req)
	if len(tools) > 0 {
		if maxSteps < 2 {
			maxSteps = 4 // need >1 step for the tool loop (generate -> tool -> generate)
		}
		opts = append(opts, goai.WithTools(tools...), goai.WithMaxSteps(maxSteps))
	}

	lm := p.build(model, cfg.apiKey, cfg.baseURL)
	start := time.Now()
	result, gerr := goai.GenerateText(ctx, lm, opts...)
	latency := time.Since(start).Milliseconds()
	if gerr != nil {
		logAIUsage(app, providerName, model, nil, 0, 0, latency, "error", gerr.Error())
		return GenerateResult{}, gerr
	}
	in, out := result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens
	logAIUsage(app, providerName, model, nil, in, out, latency, "ok", "")
	return GenerateResult{Text: result.Text, Model: model, PromptTokens: in, CompletionTokens: out}, nil
}
