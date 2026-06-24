package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/ai"
)

// Start launches the always-on orchestrator loop in a background goroutine and
// returns immediately. The loop ticks on cfg.interval for the lifetime of the
// process; each tick advances AT MOST one task (a deliberate cost throttle). It
// only spends when there is pending work AND the daily budget has room.
func Start(app core.App) {
	cfg := configFromEnv()
	if !cfg.enabled {
		app.Logger().Info("orchestrator: disabled (ORCH_ENABLED=false)")
		return
	}
	app.Logger().Info("orchestrator: started",
		"interval", cfg.interval.String(), "dailyTokenBudget", cfg.dailyTokenBudget)

	go func() {
		ticker := time.NewTicker(cfg.interval)
		defer ticker.Stop()
		for range ticker.C {
			tickSafe(app, cfg)
		}
	}()
}

// tickSafe wraps one tick in a panic recover so a single bad task can never kill
// the always-on loop.
func tickSafe(app core.App, cfg config) {
	defer func() {
		if r := recover(); r != nil {
			app.Logger().Error("orchestrator: tick panicked", "err", r)
		}
	}()
	tick(app, cfg)
}

func tick(app core.App, cfg config) {
	// Budget guard: stop dispatching once the day's token spend hits the cap.
	if cfg.dailyTokenBudget > 0 {
		if used := tokensUsedSince(app, time.Now().Add(-24*time.Hour)); used >= cfg.dailyTokenBudget {
			app.Logger().Warn("orchestrator: daily token budget reached — pausing dispatch",
				"used", used, "budget", cfg.dailyTokenBudget)
			return
		}
	}

	// Pick the oldest pending task that has an assigned agent.
	recs, err := app.FindRecordsByFilter(taskCollection, "state = {:s} && agent != ''", "created", 1, 0,
		dbx.Params{"s": StatePending})
	if err != nil || len(recs) == 0 {
		return // nothing to do
	}
	processTask(app, cfg, recs[0])
}

// processTask runs one agent against one task and leaves a DRAFT for review.
func processTask(app core.App, cfg config, task *core.Record) {
	agent, err := app.FindRecordById(agentCollection, task.GetString("agent"))
	if err != nil {
		failTask(app, task, "assigned agent no longer exists")
		return
	}
	if agent.GetString("status") == "paused" {
		return // leave pending; a paused member doesn't pick up work (no spend)
	}

	// Claim the task so a (future) second worker can't double-run it.
	task.Set("state", StateWorking)
	if err := app.Save(task); err != nil {
		return
	}

	provider := orElse(agent.GetString("provider"), cfg.provider)
	model := agent.GetString("model") // "" => provider default, handled by ai.Generate
	system := agent.GetString("persona")
	prompt := buildPrompt(task.GetString("title"), task.GetString("description"))

	ctx, cancel := context.WithTimeout(context.Background(), cfg.callTimeout)
	defer cancel()
	start := time.Now()
	res, gerr := ai.Generate(ctx, app, provider, model, system, prompt, cfg.maxTokens)
	logRun(app, task, agent, prompt, provider, res, time.Since(start).Milliseconds(), gerr)

	if gerr != nil {
		failTask(app, task, gerr.Error())
		return
	}
	task.Set("output", res.Text)
	task.Set("errorMsg", "")
	task.Set("state", StateNeedsReview) // DRAFT-ONLY: a human must approve to advance
	if err := app.Save(task); err != nil {
		app.Logger().Error("orchestrator: failed to save draft", "task", task.Id, "err", err)
	}
}

// buildPrompt renders the task into the user message. The agent's persona is the
// system prompt; this is the concrete work item.
func buildPrompt(title, description string) string {
	if description == "" {
		return "Task: " + title
	}
	return fmt.Sprintf("Task: %s\n\n%s", title, description)
}

// failTask moves a task to a terminal failed state with the reason. Terminal (not
// back to pending) so a poison task can't be retried forever and drain budget.
func failTask(app core.App, task *core.Record, reason string) {
	task.Set("state", StateFailed)
	task.Set("errorMsg", reason)
	if err := app.Save(task); err != nil {
		app.Logger().Error("orchestrator: failed to mark task failed", "task", task.Id, "err", err)
	}
}

// logRun appends an observability record for one agent action (success or error).
func logRun(app core.App, task, agent *core.Record, prompt, provider string, res ai.GenerateResult, durationMs int64, gerr error) {
	col, err := app.FindCollectionByNameOrId(runCollection)
	if err != nil {
		return
	}
	rec := core.NewRecord(col)
	rec.Set("task", task.Id)
	rec.Set("agent", agent.Id)
	rec.Set("prompt", prompt)
	rec.Set("provider", provider)
	rec.Set("model", res.Model)
	rec.Set("durationMs", durationMs)
	if gerr != nil {
		rec.Set("status", "error")
		rec.Set("errorMsg", gerr.Error())
	} else {
		rec.Set("status", "ok")
		rec.Set("output", res.Text)
		rec.Set("promptTokens", res.PromptTokens)
		rec.Set("completionTokens", res.CompletionTokens)
		rec.Set("totalTokens", res.TotalTokens())
	}
	_ = app.Save(rec)
}

// tokensUsedSince sums _runs.totalTokens since t — the spend the budget guard caps.
func tokensUsedSince(app core.App, t time.Time) int {
	cutoff := t.UTC().Format("2006-01-02 15:04:05.000Z")
	var res struct {
		Total int `db:"total"`
	}
	err := app.DB().
		NewQuery("SELECT COALESCE(SUM(totalTokens),0) AS total FROM {{" + runCollection + "}} WHERE created >= {:t}").
		Bind(dbx.Params{"t": cutoff}).
		One(&res)
	if err != nil {
		return 0
	}
	return res.Total
}

func orElse(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
