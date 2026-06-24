package orchestrator

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
func nowMinus24h() time.Time             { return time.Now().Add(-24 * time.Hour) }

// RegisterRoutes wires the human-operator endpoints. They're superuser-bound (an
// admin-scoped API key also satisfies this on the control plane), since approving
// agent work is a human policy decision.
//
//	POST /api/orchestrator/tasks              create + assign a task (kick off work)
//	POST /api/orchestrator/tasks/{id}/approve approve the draft -> hand off to next role
//	POST /api/orchestrator/tasks/{id}/reject  reject the draft (optional feedback)
//	GET  /api/orchestrator/status             live state: task counts, agents, budget
func RegisterRoutes(se *core.ServeEvent, app core.App) {
	// Create + assign a task. Assign by explicit agentId, or by role (first active
	// agent of that role). The orchestrator loop will pick it up on the next tick.
	se.Router.POST("/api/orchestrator/tasks", func(e *core.RequestEvent) error {
		var body struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Role        string `json:"role"`
			AgentID     string `json:"agentId"`
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		if body.Title == "" {
			return e.BadRequestError("'title' is required", nil)
		}
		agent, err := resolveAgent(app, body.AgentID, body.Role)
		if err != nil {
			return e.BadRequestError(err.Error(), nil)
		}
		task, err := createTask(app, body.Title, body.Description, agent.Id, "", actorID(e))
		if err != nil {
			return e.InternalServerError("failed to create task", err)
		}
		return e.JSON(http.StatusCreated, taskView(task, agent))
	}).Bind(apis.RequireSuperuserAuth())

	// Approve a draft: mark the task done and hand off to the next pipeline role
	// (if any), seeding a new pending task with this draft as its input.
	se.Router.POST("/api/orchestrator/tasks/{id}/approve", func(e *core.RequestEvent) error {
		task, err := app.FindRecordById(taskCollection, e.Request.PathValue("id"))
		if err != nil {
			return e.NotFoundError("task not found", err)
		}
		if task.GetString("state") != StateNeedsReview {
			return e.BadRequestError("task is not awaiting review (state: "+task.GetString("state")+")", nil)
		}
		// Write-tool guard: execute any side effects the agent PROPOSED while
		// drafting (e.g. a refund). Approving the task is the authorization. On
		// failure the task stays needs_review so the operator can fix or reject.
		if err := executeProposedActions(app, task); err != nil {
			return e.InternalServerError("a proposed action failed; task left for review", err)
		}
		// Action-kind task (e.g. a support reply): run its registered action — the
		// real-world effect of approval — instead of a linear pipeline handoff. The
		// action runs first; on failure the task stays needs_review so it can retry.
		if action, ok := approveActionFor(task.GetString("kind")); ok {
			if err := action(app, task); err != nil {
				return e.InternalServerError("approve action failed", err)
			}
			task.Set("state", StateDone)
			if err := app.Save(task); err != nil {
				return e.InternalServerError("failed to finalize task", err)
			}
			return e.JSON(http.StatusOK, map[string]any{
				"id": task.Id, "state": StateDone, "action": task.GetString("kind"),
			})
		}
		child, na, next, err := advanceTask(app, task, actorID(e))
		if err != nil {
			return e.InternalServerError("failed to approve task", err)
		}
		out := map[string]any{"id": task.Id, "state": StateDone}
		if child != nil {
			out["handoff"] = map[string]any{"role": next, "agent": na.GetString("name"), "taskId": child.Id}
		}
		return e.JSON(http.StatusOK, out)
	}).Bind(apis.RequireSuperuserAuth())

	// Reject a draft (terminal). Optional feedback is stored on the task.
	se.Router.POST("/api/orchestrator/tasks/{id}/reject", func(e *core.RequestEvent) error {
		task, err := app.FindRecordById(taskCollection, e.Request.PathValue("id"))
		if err != nil {
			return e.NotFoundError("task not found", err)
		}
		if task.GetString("state") != StateNeedsReview {
			return e.BadRequestError("task is not awaiting review (state: "+task.GetString("state")+")", nil)
		}
		var body struct {
			Feedback string `json:"feedback"`
		}
		_ = e.BindBody(&body)
		task.Set("state", StateRejected)
		if body.Feedback != "" {
			task.Set("errorMsg", body.Feedback)
		}
		if err := app.Save(task); err != nil {
			return e.InternalServerError("failed to reject task", err)
		}
		discardProposedActions(app, task.Id) // rejected work's side effects never run
		return e.JSON(http.StatusOK, map[string]any{"id": task.Id, "state": StateRejected})
	}).Bind(apis.RequireSuperuserAuth())

	// Send a draft back for rework: re-queue the SAME task (state -> pending) with
	// the reviewer's feedback. The loop re-runs the original agent against its prior
	// draft + this feedback (see revisionPrompt), bounded by ORCH_MAX_REVISIONS so a
	// stuck task can't loop on the budget. This is what makes the team iterate rather
	// than dead-end at reject.
	se.Router.POST("/api/orchestrator/tasks/{id}/revise", func(e *core.RequestEvent) error {
		task, err := app.FindRecordById(taskCollection, e.Request.PathValue("id"))
		if err != nil {
			return e.NotFoundError("task not found", err)
		}
		if task.GetString("state") != StateNeedsReview {
			return e.BadRequestError("task is not awaiting review (state: "+task.GetString("state")+")", nil)
		}
		if task.GetString("output") == "" {
			return e.BadRequestError("task has no draft to revise", nil)
		}
		var body struct {
			Feedback string `json:"feedback"`
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		if strings.TrimSpace(body.Feedback) == "" {
			return e.BadRequestError("'feedback' is required to request a revision", nil)
		}
		cfg := configFromEnv()
		// This check is advisory (a fast-fail for the caller); processTask re-checks the
		// cap authoritatively on the tick, so a task can never exceed it even if the
		// limit or run count shifts between here and dispatch.
		done := revisionsSoFar(app, task.Id)
		if done >= cfg.maxRevisions {
			return e.BadRequestError(
				fmt.Sprintf("max revisions (%d) already reached for this task", cfg.maxRevisions), nil)
		}
		// Keep `output` (the prior draft) as rework context; stash feedback in errorMsg
		// (processTask reads both, then clears errorMsg on the next successful draft).
		task.Set("errorMsg", body.Feedback)
		task.Set("state", StatePending)
		if err := app.Save(task); err != nil {
			return e.InternalServerError("failed to requeue task", err)
		}
		// The agent re-drafts from scratch and will re-propose, so drop the stale
		// proposals from the draft being revised.
		discardProposedActions(app, task.Id)
		return e.JSON(http.StatusOK, map[string]any{
			"id":             task.Id,
			"state":          StatePending,
			"revisionsSoFar": done,
			"maxRevisions":   cfg.maxRevisions,
		})
	}).Bind(apis.RequireSuperuserAuth())

	// Toggle autopilot live (no restart): when on, the loop auto-approves drafts
	// and chains the pipeline autonomously. Spends without a human gate — the daily
	// token budget remains the ceiling.
	se.Router.POST("/api/orchestrator/autopilot", func(e *core.RequestEvent) error {
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		autopilot.Store(body.Enabled)
		return e.JSON(http.StatusOK, map[string]any{"autopilot": body.Enabled})
	}).Bind(apis.RequireSuperuserAuth())

	// Live status: task counts by state, agent roster, and budget usage.
	se.Router.GET("/api/orchestrator/status", func(e *core.RequestEvent) error {
		cfg := configFromEnv()
		counts := map[string]int{}
		for _, s := range taskStates {
			n, _ := app.CountRecords(taskCollection, dbx.NewExp("state = {:s}", dbx.Params{"s": s}))
			counts[s] = int(n)
		}
		agents, _ := app.FindAllRecords(agentCollection)
		roster := make([]map[string]any, 0, len(agents))
		for _, a := range agents {
			roster = append(roster, map[string]any{
				"name": a.GetString("name"), "role": a.GetString("role"), "status": a.GetString("status"),
			})
		}
		used := tokensUsedSince(app, "", nowMinus24h())
		return e.JSON(http.StatusOK, map[string]any{
			"enabled":          cfg.enabled,
			"autopilot":        autopilot.Load(),
			"intervalSeconds":  int(cfg.interval.Seconds()),
			"tasks":            counts,
			"agents":           roster,
			"tokensUsedToday":  used,
			"dailyTokenBudget": cfg.dailyTokenBudget,
			"maxRevisions":     cfg.maxRevisions,
			"pipeline":         pipeline,
		})
	}).Bind(apis.RequireSuperuserAuth())

	// List tasks (newest first), optionally filtered by state, agent id, or role.
	// Paginated via ?page (1-based) and ?limit (default 50, max 200). This is how an
	// operator sees WHAT the company is doing, not just how many tasks are in each
	// bucket (that's /status).
	se.Router.GET("/api/orchestrator/tasks", func(e *core.RequestEvent) error {
		q := e.Request.URL.Query()
		limit := clampInt(atoiOr(q.Get("limit"), 50), 1, 200)
		page := atoiOr(q.Get("page"), 1)
		if page < 1 {
			page = 1
		}

		filter := "1=1"
		params := dbx.Params{}
		if s := q.Get("state"); s != "" {
			filter += " && state = {:state}"
			params["state"] = s
		}
		if a := q.Get("agent"); a != "" {
			filter += " && agent = {:agent}"
			params["agent"] = a
		}
		if role := q.Get("role"); role != "" {
			ids, _ := agentIDsByRole(app, role)
			if len(ids) == 0 {
				return e.JSON(http.StatusOK, map[string]any{"page": page, "limit": limit, "items": []any{}})
			}
			ors := make([]string, len(ids))
			for i, id := range ids {
				key := fmt.Sprintf("ra%d", i)
				ors[i] = "agent = {:" + key + "}"
				params[key] = id
			}
			filter += " && (" + strings.Join(ors, " || ") + ")"
		}

		recs, err := app.FindRecordsByFilter(taskCollection, filter, "-created", limit, (page-1)*limit, params)
		if err != nil {
			return e.InternalServerError("failed to list tasks", err)
		}
		agentsByID := agentMap(app)
		items := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			items = append(items, listTaskView(r, agentsByID))
		}
		return e.JSON(http.StatusOK, map[string]any{"page": page, "limit": limit, "items": items})
	}).Bind(apis.RequireSuperuserAuth())

	// Task detail: the full record (incl. the draft output), its lineage chain
	// (ancestor tasks oldest-first, walked up via parentTask), and every _run logged
	// for it (oldest-first). The one call an operator needs to inspect a single piece
	// of work end to end.
	se.Router.GET("/api/orchestrator/tasks/{id}", func(e *core.RequestEvent) error {
		task, err := app.FindRecordById(taskCollection, e.Request.PathValue("id"))
		if err != nil {
			return e.NotFoundError("task not found", err)
		}
		// Walk the parentTask chain upward; guard against cycles with a seen-set.
		lineage := []map[string]any{}
		seen := map[string]bool{task.Id: true}
		for cur := task; ; {
			pid := cur.GetString("parentTask")
			if pid == "" || seen[pid] {
				break
			}
			seen[pid] = true
			p, perr := app.FindRecordById(taskCollection, pid)
			if perr != nil {
				break
			}
			lineage = append(lineage, detailTaskView(app, p))
			cur = p
		}
		// Reverse to oldest-first (root ancestor leads).
		for i, j := 0, len(lineage)-1; i < j; i, j = i+1, j-1 {
			lineage[i], lineage[j] = lineage[j], lineage[i]
		}

		runRecs, _ := app.FindRecordsByFilter(runCollection, "task = {:t}", "created", 200, 0, dbx.Params{"t": task.Id})
		runs := make([]map[string]any, 0, len(runRecs))
		for _, r := range runRecs {
			runs = append(runs, runView(r))
		}

		return e.JSON(http.StatusOK, map[string]any{
			"task":            detailTaskView(app, task),
			"lineage":         lineage,
			"runs":            runs,
			"proposedActions": proposalViews(app, task.Id),
		})
	}).Bind(apis.RequireSuperuserAuth())
}

// ---- helpers ----

func resolveAgent(app core.App, agentID, role string) (*core.Record, error) {
	if agentID != "" {
		a, err := app.FindRecordById(agentCollection, agentID)
		if err != nil {
			return nil, errf("unknown agentId: %s", agentID)
		}
		return a, nil
	}
	if role != "" {
		return activeAgentByRole(app, role)
	}
	return nil, errf("provide 'agentId' or 'role'")
}

func activeAgentByRole(app core.App, role string) (*core.Record, error) {
	a, err := app.FindFirstRecordByFilter(agentCollection, "role = {:r} && status = 'active'", dbx.Params{"r": role})
	if err != nil {
		return nil, errf("no active agent for role %q", role)
	}
	return a, nil
}

// createTask inserts a pending task. kind is an optional trailing arg (default
// "" = software pipeline); the variadic keeps the many existing call sites stable
// while letting EnqueueTask tag a task with an approve-action kind.
func createTask(app core.App, title, description, agentID, parentID, createdBy string, kind ...string) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId(taskCollection)
	if err != nil {
		return nil, err
	}
	k := ""
	if len(kind) > 0 {
		k = kind[0]
	}
	rec := core.NewRecord(col)
	rec.Set("title", title)
	rec.Set("description", description)
	rec.Set("agent", agentID)
	rec.Set("state", StatePending)
	rec.Set("kind", k)
	rec.Set("parentTask", parentID)
	rec.Set("createdBy", createdBy)
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func taskView(task, agent *core.Record) map[string]any {
	return map[string]any{
		"id": task.Id, "title": task.GetString("title"), "state": task.GetString("state"),
		"agent": agent.GetString("name"), "role": agent.GetString("role"),
	}
}

func actorID(e *core.RequestEvent) string {
	if e.Auth != nil {
		return e.Auth.Id
	}
	return ""
}

// agentIDsByRole returns the ids of every agent with the given role (a role may
// have more than one agent). Used to filter tasks by role on the list endpoint.
func agentIDsByRole(app core.App, role string) ([]string, error) {
	recs, err := app.FindRecordsByFilter(agentCollection, "role = {:r}", "", 500, 0, dbx.Params{"r": role})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.Id
	}
	return ids, nil
}

// agentMap loads all agents into an id->record map so a list of tasks can be
// rendered with agent name/role without an N+1 lookup per task.
func agentMap(app core.App) map[string]*core.Record {
	m := map[string]*core.Record{}
	agents, err := app.FindAllRecords(agentCollection)
	if err != nil {
		return m
	}
	for _, a := range agents {
		m[a.Id] = a
	}
	return m
}

// listTaskView is the compact per-task shape for the list endpoint (no draft body
// — just whether one exists, via hasOutput).
func listTaskView(t *core.Record, agentsByID map[string]*core.Record) map[string]any {
	v := map[string]any{
		"id":         t.Id,
		"title":      t.GetString("title"),
		"state":      t.GetString("state"),
		"parentTask": t.GetString("parentTask"),
		"hasOutput":  t.GetString("output") != "",
		"created":    t.GetDateTime("created").String(),
		"updated":    t.GetDateTime("updated").String(),
	}
	if a := agentsByID[t.GetString("agent")]; a != nil {
		v["agent"] = a.GetString("name")
		v["role"] = a.GetString("role")
	}
	return v
}

// detailTaskView is the full per-task shape (includes the draft output) for the
// detail endpoint and lineage entries.
func detailTaskView(app core.App, t *core.Record) map[string]any {
	v := map[string]any{
		"id":          t.Id,
		"title":       t.GetString("title"),
		"description": t.GetString("description"),
		"state":       t.GetString("state"),
		"output":      t.GetString("output"),
		"errorMsg":    t.GetString("errorMsg"),
		"parentTask":  t.GetString("parentTask"),
		"createdBy":   t.GetString("createdBy"),
		"created":     t.GetDateTime("created").String(),
		"updated":     t.GetDateTime("updated").String(),
	}
	if a, err := app.FindRecordById(agentCollection, t.GetString("agent")); err == nil {
		v["agent"] = a.GetString("name")
		v["role"] = a.GetString("role")
	}
	return v
}

// runView is the per-run shape (an agent action's outcome + token/latency cost).
func runView(r *core.Record) map[string]any {
	return map[string]any{
		"id":               r.Id,
		"status":           r.GetString("status"),
		"provider":         r.GetString("provider"),
		"model":            r.GetString("model"),
		"promptTokens":     r.GetInt("promptTokens"),
		"completionTokens": r.GetInt("completionTokens"),
		"totalTokens":      r.GetInt("totalTokens"),
		"durationMs":       r.GetInt("durationMs"),
		"errorMsg":         r.GetString("errorMsg"),
		"created":          r.GetDateTime("created").String(),
	}
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
