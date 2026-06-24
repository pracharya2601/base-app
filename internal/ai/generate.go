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

// Generate runs a single LLM call IN-PROCESS (no HTTP round-trip), for trusted
// internal callers like the orchestrator. It resolves the provider's stored +
// decrypted key/config, falls back to the provider's defaultModel when model is
// "", records a metering row in _aiUsage (with no userId — it's a system call),
// and returns the text + token usage. maxTokens <= 0 uses the provider default.
//
// Unlike the HTTP routes, Generate does NOT enforce per-user rate/quota limits —
// the caller is the system, so spend governance is the caller's responsibility.
func Generate(ctx context.Context, app core.App, providerName, model, system, prompt string, maxTokens int) (GenerateResult, error) {
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
	lm := p.build(model, cfg.apiKey, cfg.baseURL)
	start := time.Now()
	result, gerr := goai.GenerateText(ctx, lm, aiBuildOptions(req)...)
	latency := time.Since(start).Milliseconds()
	if gerr != nil {
		logAIUsage(app, providerName, model, nil, 0, 0, latency, "error", gerr.Error())
		return GenerateResult{}, gerr
	}
	in, out := result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens
	logAIUsage(app, providerName, model, nil, in, out, latency, "ok", "")
	return GenerateResult{Text: result.Text, Model: model, PromptTokens: in, CompletionTokens: out}, nil
}
