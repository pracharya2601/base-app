// Package orchestrator runs an always-on "AI company": a team of role-based
// agents (stored in _agents) that pick up tasks (_tasks), do the work by calling
// the AI proxy, and leave a DRAFT for a human to approve. Every agent action is
// logged to _runs for observability. Spend is capped by a daily token budget.
//
// v1 is a software team that SHIPS CODE in DRAFT-ONLY mode: agents produce specs,
// code drafts, and reviews — they never execute anything. Work only advances when
// a human approves, and approval hands off to the next role in the pipeline:
//
//	product-manager  ->  engineer  ->  reviewer  ->  done
//
// (each arrow is a human approval). The orchestrator never sleeps — it ticks on an
// interval — but agents are event-driven workers, so it only spends when there is
// pending work and budget remaining.
package orchestrator

import (
	"github.com/pocketbase/pocketbase/core"
)

const (
	agentCollection = "_agents"
	taskCollection  = "_tasks"
	runCollection   = "_runs"
)

// Task states. The lifecycle:
//
//	pending -> working -> needs_review --(human)--> approved -> done
//	                                   \--(human)--> rejected
//	working --(LLM error)--> failed
const (
	StatePending     = "pending"
	StateWorking     = "working"
	StateNeedsReview = "needs_review"
	StateApproved    = "approved"
	StateRejected    = "rejected"
	StateDone        = "done"
	StateFailed      = "failed"
)

var taskStates = []string{
	StatePending, StateWorking, StateNeedsReview,
	StateApproved, StateRejected, StateDone, StateFailed,
}

// EnsureSchema creates the three system collections in dependency order
// (_agents, then _tasks, then _runs — relations point backwards). Idempotent.
func EnsureSchema(app core.App) error {
	agents, err := ensureAgents(app)
	if err != nil {
		return err
	}
	tasks, err := ensureTasks(app, agents)
	if err != nil {
		return err
	}
	return ensureRuns(app, tasks, agents)
}

func ensureAgents(app core.App) (*core.Collection, error) {
	if c, err := app.FindCollectionByNameOrId(agentCollection); err == nil {
		return c, nil
	}
	c := core.NewBaseCollection(agentCollection)
	c.System = true
	c.Fields.Add(&core.TextField{Name: "name", Required: true})
	c.Fields.Add(&core.TextField{Name: "role"})    // pipeline role, e.g. "engineer"
	c.Fields.Add(&core.TextField{Name: "persona"}) // the agent's system prompt
	c.Fields.Add(&core.TextField{Name: "provider"})
	c.Fields.Add(&core.TextField{Name: "model"})
	c.Fields.Add(&core.SelectField{Name: "status", Values: []string{"active", "paused"}, MaxSelect: 1})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	if err := app.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

func ensureTasks(app core.App, agents *core.Collection) (*core.Collection, error) {
	if c, err := app.FindCollectionByNameOrId(taskCollection); err == nil {
		return c, nil
	}
	c := core.NewBaseCollection(taskCollection)
	c.System = true
	c.Fields.Add(&core.TextField{Name: "title", Required: true})
	c.Fields.Add(&core.TextField{Name: "description"})
	c.Fields.Add(&core.RelationField{Name: "agent", CollectionId: agents.Id, MaxSelect: 1})
	c.Fields.Add(&core.SelectField{Name: "state", Values: taskStates, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "output"})     // the draft the agent produced
	c.Fields.Add(&core.TextField{Name: "parentTask"}) // lineage id (text to avoid self-relation ordering)
	c.Fields.Add(&core.TextField{Name: "errorMsg"})
	c.Fields.Add(&core.TextField{Name: "createdBy"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	if err := app.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

func ensureRuns(app core.App, tasks, agents *core.Collection) error {
	if _, err := app.FindCollectionByNameOrId(runCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(runCollection)
	c.System = true
	c.Fields.Add(&core.RelationField{Name: "task", CollectionId: tasks.Id, MaxSelect: 1})
	c.Fields.Add(&core.RelationField{Name: "agent", CollectionId: agents.Id, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "prompt"})
	c.Fields.Add(&core.TextField{Name: "output"})
	c.Fields.Add(&core.TextField{Name: "provider"})
	c.Fields.Add(&core.TextField{Name: "model"})
	c.Fields.Add(&core.NumberField{Name: "promptTokens"})
	c.Fields.Add(&core.NumberField{Name: "completionTokens"})
	c.Fields.Add(&core.NumberField{Name: "totalTokens"})
	c.Fields.Add(&core.NumberField{Name: "durationMs"})
	c.Fields.Add(&core.TextField{Name: "status"})
	c.Fields.Add(&core.TextField{Name: "errorMsg"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(c)
}

// teamMember is a seeded default agent.
type teamMember struct {
	name, role, persona string
}

// defaultTeam is the v1 software team. Personas are written for DRAFT-ONLY work:
// they explicitly tell the agent its output is a draft for human review, never
// executed.
var defaultTeam = []teamMember{
	{
		name: "Pam (PM)",
		role: RolePM,
		persona: "You are a sharp product manager on a software team. Given a feature request, " +
			"write a concise, actionable spec in markdown: Problem, User Story, Acceptance Criteria " +
			"(a checklist), and a short Task Breakdown. Be specific and avoid fluff. Your output is a " +
			"draft a human will review before any work proceeds.",
	},
	{
		name: "Ed (Engineer)",
		role: RoleEngineer,
		persona: "You are a senior software engineer. Given a spec or task, produce a concrete " +
			"implementation DRAFT in markdown: the code (with explicit file paths in fenced code blocks) " +
			"plus a brief explanation of the approach and any trade-offs. This is a DRAFT for human review " +
			"— it will NOT be executed or committed automatically, so do not run commands or assume side effects.",
	},
	{
		name: "Rey (Reviewer)",
		role: RoleReviewer,
		persona: "You are a meticulous senior code reviewer. Given a code draft, review it for " +
			"correctness, security, edge cases, and clarity. Output markdown: a short summary verdict, then " +
			"a list of concrete issues (each with file/line context and a suggested fix). Be direct about real " +
			"problems and don't invent nitpicks. Your review is advisory for a human.",
	},
}

// SeedTeam creates the default software team if no agents exist yet. Idempotent.
func SeedTeam(app core.App) {
	if n, _ := app.CountRecords(agentCollection); n > 0 {
		return
	}
	col, err := app.FindCollectionByNameOrId(agentCollection)
	if err != nil {
		return
	}
	cfg := configFromEnv()
	for _, m := range defaultTeam {
		rec := core.NewRecord(col)
		rec.Set("name", m.name)
		rec.Set("role", m.role)
		rec.Set("persona", m.persona)
		rec.Set("provider", cfg.provider)
		rec.Set("model", cfg.model)
		rec.Set("status", "active")
		if err := app.Save(rec); err != nil {
			app.Logger().Error("orchestrator: seed agent failed", "name", m.name, "err", err)
		}
	}
}
