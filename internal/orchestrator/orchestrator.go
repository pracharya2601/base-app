package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/ai"
)

// Start launches the always-on orchestrator loop in a background goroutine and
// returns immediately. The loop ticks on the interval for the lifetime of the
// process; each tick advances AT MOST one task (a deliberate cost throttle) and
// re-reads the per-tenant config from the DB, so autopilot/budget/model edits take
// effect live (no restart). It only spends when there is pending work AND the
// daily budget has room. (Interval itself is fixed at boot; changing it needs a
// restart.)
func Start(app core.App) {
	cfg := loadOrchConfig(app, SystemOwner)
	if !cfg.enabled {
		app.Logger().Info("orchestrator: disabled (ORCH_ENABLED=false)")
		return
	}
	app.Logger().Info("orchestrator: started",
		"interval", cfg.interval.String(), "dailyTokenBudget", cfg.dailyTokenBudget,
		"autopilot", cfg.autoApprove)

	go func() {
		ticker := time.NewTicker(cfg.interval)
		defer ticker.Stop()
		for range ticker.C {
			tickSafe(app)
		}
	}()
}

// tickSafe wraps one tick in a panic recover so a single bad task can never kill
// the always-on loop.
func tickSafe(app core.App) {
	defer func() {
		if r := recover(); r != nil {
			app.Logger().Error("orchestrator: tick panicked", "err", r)
		}
	}()
	// Re-read config each tick so live edits (autopilot, budget, provider/model)
	// apply without a restart.
	tick(app, loadOrchConfig(app, SystemOwner))
}

func tick(app core.App, cfg config) {
	// Budget guard: stop dispatching once the day's token spend hits the cap.
	if cfg.dailyTokenBudget > 0 {
		if used := tokensUsedSince(app, "", time.Now().Add(-24*time.Hour)); used >= cfg.dailyTokenBudget {
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

	// Revision detection: a re-queued task (see the /revise route) carries its prior
	// draft in `output` and the reviewer feedback in `errorMsg`. A fresh task — or a
	// pipeline handoff — has an empty `output`, so this also tells the two apart.
	// Cap rework BEFORE claiming/spending so a task that can't be satisfied can never
	// loop on the budget.
	isRevision := task.GetString("output") != ""
	if isRevision && revisionsSoFar(app, task.Id) >= cfg.maxRevisions {
		failTask(app, task, fmt.Sprintf("max revisions (%d) exceeded", cfg.maxRevisions))
		return
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
	if isRevision {
		prompt = revisionPrompt(task.GetString("title"), task.GetString("description"),
			task.GetString("output"), task.GetString("errorMsg"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.callTimeout)
	defer cancel()
	start := time.Now()
	// If this task's kind has registered tools, run the agent with the auto tool
	// loop so it can ACT (e.g. look up an order) mid-draft. Otherwise it's a plain
	// single-shot generation.
	var res ai.GenerateResult
	var gerr error
	if tp, ok := toolsForKind(task.GetString("kind")); ok {
		res, gerr = ai.GenerateWithTools(ctx, app, provider, model, system, prompt, cfg.maxTokens, tp(app, task), cfg.maxToolSteps)
	} else {
		res, gerr = ai.Generate(ctx, app, provider, model, system, prompt, cfg.maxTokens)
	}
	logRun(app, task, agent, prompt, provider, res, time.Since(start).Milliseconds(), gerr)

	if gerr != nil {
		failTask(app, task, gerr.Error())
		return
	}
	task.Set("output", res.Text)
	task.Set("errorMsg", "")

	// Action-kind tasks (e.g. a support reply) always stop for human approval, even
	// under autopilot — approving them has a real side effect (emailing a customer),
	// so a human must sign off. Only the draft-only software pipeline auto-advances.
	if cfg.autoApprove && !HasApproveAction(task.GetString("kind")) {
		// Close the loop: if a reviewer asked for changes, send the work back to its
		// author to revise instead of shipping it. When the gate handles the task,
		// we're done for this tick.
		if autopilotReviewGate(app, cfg, task, agent, res.Text) {
			return
		}
		// Otherwise auto-approve and hand off to the next role (advanceTask saves the
		// task with its draft). Context stays bounded — the next agent receives only
		// this draft as input, not the whole chain.
		if _, _, _, aerr := advanceTask(app, task, "autopilot"); aerr != nil {
			// advanceTask marks the task done BEFORE the handoff, so a handoff failure
			// would otherwise leave a silently-stuck done task with no successor. Record
			// the reason on the task so it surfaces via the detail endpoint, not just logs.
			app.Logger().Error("orchestrator: autopilot advance failed", "task", task.Id, "err", aerr)
			task.Set("errorMsg", "handoff failed: "+aerr.Error())
			if serr := app.Save(task); serr != nil {
				app.Logger().Error("orchestrator: failed to record handoff error", "task", task.Id, "err", serr)
			}
		}
		return
	}
	task.Set("state", StateNeedsReview) // DRAFT-ONLY: a human must approve to advance
	if err := app.Save(task); err != nil {
		app.Logger().Error("orchestrator: failed to save draft", "task", task.Id, "err", err)
	}
}

// advanceTask marks a task done and, if its agent's role has a successor in the
// pipeline with an active agent, spawns the handoff task carrying this task's
// draft as the next agent's input. Returns (child, nextAgent, nextRole, err);
// child/nextAgent are nil when the pipeline ends or the next role has no active
// agent. Shared by manual approval (routes.go) and autopilot (above).
func advanceTask(app core.App, task *core.Record, createdBy string) (child, nextAgent *core.Record, next string, err error) {
	task.Set("state", StateDone)
	if err = app.Save(task); err != nil {
		return nil, nil, "", err
	}
	agent, ferr := app.FindRecordById(agentCollection, task.GetString("agent"))
	if ferr != nil {
		return nil, nil, "", nil // done, but no agent to derive a handoff from
	}
	next, ok := nextRole(agent.GetString("role"))
	if !ok {
		return nil, nil, "", nil // end of pipeline
	}
	na, aerr := activeAgentByRole(app, next)
	if aerr != nil {
		return nil, nil, next, nil // next role has no active agent — chain pauses here
	}
	c, cerr := createTask(app, next+": "+task.GetString("title"), task.GetString("output"), na.Id, task.Id, createdBy)
	if cerr != nil {
		return nil, nil, next, cerr
	}
	return c, na, next, nil
}

// Reviewer verdicts parsed from the trailing `VERDICT:` line (see the reviewer
// persona in schema.go). Anything without an explicit changes signal is treated as
// approve — autopilot never loops without being told to.
const (
	verdictApprove = "approve"
	verdictChanges = "changes"
)

// parseVerdict reads the reviewer's machine-readable verdict from its review text.
// It uses the LAST `VERDICT:` occurrence (the real verdict is the final line; an
// earlier mention of the format won't false-trigger). Returns "" when no verdict
// is present, which the gate treats as approve.
func parseVerdict(text string) string {
	up := strings.ToUpper(text)
	i := strings.LastIndex(up, "VERDICT:")
	if i < 0 {
		return ""
	}
	rest := up[i+len("VERDICT:"):]
	switch {
	case strings.Contains(rest, "CHANGES"), strings.Contains(rest, "REQUEST"), strings.Contains(rest, "REJECT"):
		return verdictChanges
	case strings.Contains(rest, "APPROVE"), strings.Contains(rest, "LGTM"), strings.Contains(rest, "PASS"):
		return verdictApprove
	}
	return ""
}

// autopilotReviewGate closes the loop in autopilot mode. If a reviewer asked for
// changes AND the upstream (authoring) task can still be revised, it re-queues that
// task with the review as feedback and marks this review done — so the author
// reworks and gets re-reviewed, bounded by ORCH_MAX_REVISIONS on the author task.
// Returns true when it handled the task (the caller stops); false means "advance
// normally" (approved, no clear changes verdict, no upstream task, or rework budget
// exhausted — let the chain finish rather than spin).
func autopilotReviewGate(app core.App, cfg config, reviewerTask, agent *core.Record, reviewText string) bool {
	if agent.GetString("role") != RoleReviewer || parseVerdict(reviewText) != verdictChanges {
		return false
	}
	parentID := reviewerTask.GetString("parentTask")
	if parentID == "" {
		return false
	}
	parent, err := app.FindRecordById(taskCollection, parentID)
	if err != nil || parent.GetString("output") == "" {
		return false // no upstream draft to rework
	}
	if revisionsSoFar(app, parent.Id) >= cfg.maxRevisions {
		return false // out of rework budget — ship as-is instead of looping forever
	}
	// Re-queue the author to rework against the review (same mechanism as /revise).
	parent.Set("errorMsg", reviewText)
	parent.Set("state", StatePending)
	if err := app.Save(parent); err != nil {
		app.Logger().Error("orchestrator: review-gate requeue failed", "task", parent.Id, "err", err)
		return false
	}
	reviewerTask.Set("state", StateDone) // the review did its job
	if err := app.Save(reviewerTask); err != nil {
		app.Logger().Error("orchestrator: review-gate close failed", "task", reviewerTask.Id, "err", err)
	}
	app.Logger().Info("orchestrator: review requested changes — re-queued upstream task",
		"review", reviewerTask.Id, "upstream", parent.Id)
	return true
}

// buildPrompt renders the task into the user message. The agent's persona is the
// system prompt; this is the concrete work item.
func buildPrompt(title, description string) string {
	if description == "" {
		return "Task: " + title
	}
	return fmt.Sprintf("Task: %s\n\n%s", title, description)
}

// revisionPrompt asks the agent to rework its previous draft against reviewer
// feedback. The prior draft and feedback are the rework context; the agent is
// asked for the COMPLETE revised draft so the result is self-contained (the next
// pipeline stage, or the human, never has to stitch a diff).
func revisionPrompt(title, description, priorDraft, feedback string) string {
	var b strings.Builder
	b.WriteString("Revise your previous draft to address the feedback below. ")
	b.WriteString("Output the COMPLETE revised draft, not just the changes.\n\n")
	b.WriteString("## Task\n")
	b.WriteString(title)
	if description != "" {
		b.WriteString("\n\n")
		b.WriteString(description)
	}
	b.WriteString("\n\n## Your previous draft\n")
	b.WriteString(priorDraft)
	b.WriteString("\n\n## Feedback to address\n")
	if feedback != "" {
		b.WriteString(feedback)
	} else {
		b.WriteString("(no specific feedback given — improve correctness, clarity, and completeness)")
	}
	return b.String()
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
	rec.Set("owner", agent.GetString("owner")) // denormalized so per-tenant budget sums stay a single-table scan
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

// revisionsSoFar is how many rework passes a task has already had: its total
// logged runs minus the original draft. Drives the ORCH_MAX_REVISIONS cap so a
// task that can't be satisfied can't loop forever on the budget.
func revisionsSoFar(app core.App, taskID string) int {
	n, err := app.CountRecords(runCollection, dbx.NewExp("task = {:t}", dbx.Params{"t": taskID}))
	if err != nil || n <= 1 {
		return 0
	}
	return int(n) - 1
}

// tokensUsedSince sums _runs.totalTokens since t — the spend the budget guard caps.
// ownerID scopes the sum to one tenant; pass "" to sum across ALL tenants (the
// current global guard, and the SystemOwner/legacy rows). Per-tenant isolation in
// the tick (Slice 2) passes a concrete owner.
func tokensUsedSince(app core.App, ownerID string, t time.Time) int {
	cutoff := t.UTC().Format("2006-01-02 15:04:05.000Z")
	query := "SELECT COALESCE(SUM(totalTokens),0) AS total FROM {{" + runCollection + "}} WHERE created >= {:t}"
	params := dbx.Params{"t": cutoff}
	if ownerID != "" {
		query += " AND owner = {:o}"
		params["o"] = ownerID
	}
	var res struct {
		Total int `db:"total"`
	}
	if err := app.DB().NewQuery(query).Bind(params).One(&res); err != nil {
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
