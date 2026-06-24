package orchestrator

import (
	"fmt"
	"net/http"
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
		agent, _ := app.FindRecordById(agentCollection, task.GetString("agent"))

		task.Set("state", StateDone)
		if err := app.Save(task); err != nil {
			return e.InternalServerError("failed to approve task", err)
		}

		out := map[string]any{"id": task.Id, "state": StateDone}
		// Hand off to the next role, if there is one and an active agent for it.
		if agent != nil {
			if next, ok := nextRole(agent.GetString("role")); ok {
				if na, err := activeAgentByRole(app, next); err == nil {
					child, cerr := createTask(app,
						next+": "+task.GetString("title"),
						task.GetString("output"), // the approved draft becomes the next agent's input
						na.Id, task.Id, actorID(e))
					if cerr == nil {
						out["handoff"] = map[string]any{"role": next, "agent": na.GetString("name"), "taskId": child.Id}
					}
				}
			}
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
		return e.JSON(http.StatusOK, map[string]any{"id": task.Id, "state": StateRejected})
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
		used := tokensUsedSince(app, nowMinus24h())
		return e.JSON(http.StatusOK, map[string]any{
			"enabled":          cfg.enabled,
			"intervalSeconds":  int(cfg.interval.Seconds()),
			"tasks":            counts,
			"agents":           roster,
			"tokensUsedToday":  used,
			"dailyTokenBudget": cfg.dailyTokenBudget,
			"pipeline":         pipeline,
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

func createTask(app core.App, title, description, agentID, parentID, createdBy string) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId(taskCollection)
	if err != nil {
		return nil, err
	}
	rec := core.NewRecord(col)
	rec.Set("title", title)
	rec.Set("description", description)
	rec.Set("agent", agentID)
	rec.Set("state", StatePending)
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
