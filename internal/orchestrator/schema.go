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
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

const (
	agentCollection          = "_agents"
	taskCollection           = "_tasks"
	runCollection            = "_runs"
	configCollection         = "_orchConfigs"     // per-tenant config overrides (owner-keyed)
	proposedActionCollection = "_proposedActions" // write-tool side effects awaiting approval
)

// TaskCollection / AgentCollection are the system collection names, exported so
// feature packages (e.g. internal/support) can bind PocketBase hooks and query
// against them.
const (
	TaskCollection  = taskCollection
	AgentCollection = agentCollection
)

// SystemOwner is the tenant id for legacy/global rows that predate multi-tenancy
// (agents/tasks/runs created before an owner field existed). It is also the owner
// the boot-time global SeedTeam uses, so the platform's default team is just the
// "" tenant.
const SystemOwner = ""

// Task states. The lifecycle:
//
//	pending -> working -> needs_review --(approve)--> done (-> handoff to next role)
//	             ^                      |--(revise)--> pending  (rework, capped by ORCH_MAX_REVISIONS)
//	             |                      \--(reject)--> rejected (terminal)
//	             '--(revise re-queue)---'
//	working --(LLM error)--> failed
//	working --(revisions exhausted)--> failed
const (
	StatePending     = "pending"
	StateWorking     = "working"
	StateNeedsReview = "needs_review"
	StateRejected    = "rejected"
	StateDone        = "done"
	StateFailed      = "failed"
)

var taskStates = []string{
	StatePending, StateWorking, StateNeedsReview,
	StateRejected, StateDone, StateFailed,
}

// EnsureSchema creates the system collections in dependency order (_agents, then
// _tasks, then _runs — relations point backwards), plus the owner-keyed
// _orchConfigs. Idempotent: on an already-existing collection it additively
// backfills the multi-tenancy `owner` field (locked system collections accept new
// non-system fields), leaving existing rows untouched (owner defaults to "").
func EnsureSchema(app core.App) error {
	agents, err := ensureAgents(app)
	if err != nil {
		return err
	}
	tasks, err := ensureTasks(app, agents)
	if err != nil {
		return err
	}
	if err := ensureRuns(app, tasks, agents); err != nil {
		return err
	}
	if err := ensureProposedActions(app, tasks); err != nil {
		return err
	}
	return ensureOrchestratorConfigs(app)
}

// ensureProposedActions creates the _proposedActions system collection: the record
// of side effects a write-tool wants to perform, held until a human approves the
// task (then executed) or rejects/revises it (then discarded). This is what makes
// mutating tools safe — they propose here instead of acting mid-draft. Idempotent.
func ensureProposedActions(app core.App, tasks *core.Collection) error {
	if _, err := app.FindCollectionByNameOrId(proposedActionCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(proposedActionCollection)
	c.System = true
	c.Fields.Add(&core.RelationField{Name: "task", CollectionId: tasks.Id, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "actionKind", Required: true}) // selects the registered executor
	c.Fields.Add(&core.TextField{Name: "params"})                     // JSON args for the executor
	c.Fields.Add(&core.SelectField{Name: "status", Values: proposedStatuses, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "result"}) // executor output or error
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	return app.Save(c)
}

// ensureTextField additively adds a plain text field to an existing collection if
// it's missing. Safe on locked system collections (a new non-system field is
// allowed) and a no-op once present, so it's idempotent across restarts and
// harmless to pre-existing rows.
func ensureTextField(app core.App, c *core.Collection, name string) error {
	if c.Fields.GetByName(name) != nil {
		return nil
	}
	c.Fields.Add(&core.TextField{Name: name})
	return app.Save(c)
}

// ensureOwnerField additively adds the multi-tenancy `owner` text field to an
// existing collection if it's missing.
func ensureOwnerField(app core.App, c *core.Collection) error {
	return ensureTextField(app, c, "owner") // tenant id; "" = legacy/system
}

func ensureAgents(app core.App) (*core.Collection, error) {
	if c, err := app.FindCollectionByNameOrId(agentCollection); err == nil {
		return c, ensureOwnerField(app, c)
	}
	c := core.NewBaseCollection(agentCollection)
	c.System = true
	c.Fields.Add(&core.TextField{Name: "name", Required: true})
	c.Fields.Add(&core.TextField{Name: "role"})    // pipeline role, e.g. "engineer"
	c.Fields.Add(&core.TextField{Name: "persona"}) // the agent's system prompt
	c.Fields.Add(&core.TextField{Name: "provider"})
	c.Fields.Add(&core.TextField{Name: "model"})
	c.Fields.Add(&core.TextField{Name: "owner"}) // tenant id; "" = legacy/system
	c.Fields.Add(&core.SelectField{Name: "status", Values: []string{"active", "paused"}, MaxSelect: 1})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	if err := app.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

func ensureTasks(app core.App, agents *core.Collection) (*core.Collection, error) {
	if c, err := app.FindCollectionByNameOrId(taskCollection); err == nil {
		if err := ensureOwnerField(app, c); err != nil {
			return c, err
		}
		return c, ensureTextField(app, c, "kind") // task type for approve-action dispatch
	}
	c := core.NewBaseCollection(taskCollection)
	c.System = true
	c.Fields.Add(&core.TextField{Name: "title", Required: true})
	c.Fields.Add(&core.TextField{Name: "description"})
	c.Fields.Add(&core.RelationField{Name: "agent", CollectionId: agents.Id, MaxSelect: 1})
	c.Fields.Add(&core.SelectField{Name: "state", Values: taskStates, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "output"})     // the draft the agent produced
	c.Fields.Add(&core.TextField{Name: "kind"})       // task type; "" = software pipeline, else an approve-action kind
	c.Fields.Add(&core.TextField{Name: "parentTask"}) // lineage id (text to avoid self-relation ordering)
	c.Fields.Add(&core.TextField{Name: "errorMsg"})
	c.Fields.Add(&core.TextField{Name: "owner"}) // tenant id; "" = legacy/system
	c.Fields.Add(&core.TextField{Name: "createdBy"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	if err := app.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

func ensureRuns(app core.App, tasks, agents *core.Collection) error {
	if c, err := app.FindCollectionByNameOrId(runCollection); err == nil {
		return ensureOwnerField(app, c)
	}
	c := core.NewBaseCollection(runCollection)
	c.System = true
	c.Fields.Add(&core.RelationField{Name: "task", CollectionId: tasks.Id, MaxSelect: 1})
	c.Fields.Add(&core.RelationField{Name: "agent", CollectionId: agents.Id, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "owner"}) // tenant id (denormalized for per-tenant budget sums); "" = legacy/system
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

// ensureOrchestratorConfigs creates the owner-keyed _orchConfigs collection that
// holds per-tenant overrides of the env knobs. Every config field is zero-valued by
// default, which the tick treats as "fall through to the ORCH_* env default" — so a
// row's mere existence costs nothing until a field is set. Idempotent.
func ensureOrchestratorConfigs(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(configCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(configCollection)
	c.System = true
	c.Fields.Add(&core.TextField{Name: "owner", Required: true}) // tenant id (one row per tenant)
	c.Fields.Add(&core.BoolField{Name: "enabled"})
	c.Fields.Add(&core.BoolField{Name: "autopilot"})
	c.Fields.Add(&core.NumberField{Name: "intervalSeconds"})
	c.Fields.Add(&core.NumberField{Name: "maxTokens"})
	c.Fields.Add(&core.NumberField{Name: "maxRevisions"})
	c.Fields.Add(&core.NumberField{Name: "dailyTokenBudget"})
	c.Fields.Add(&core.TextField{Name: "provider"})
	c.Fields.Add(&core.TextField{Name: "model"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	// One config row per tenant.
	c.Indexes = []string{"CREATE UNIQUE INDEX `idx_orchConfigs_owner` ON `" + configCollection + "` (`owner`)"}
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
			"problems and don't invent nitpicks. " +
			"End your review with a single final line that is EXACTLY one of: " +
			"`VERDICT: APPROVE` if the draft is good to ship, or `VERDICT: CHANGES_REQUESTED` if it needs rework. " +
			"In autopilot this line decides whether the work ships or goes back to the author, so be deliberate.",
	},
}

// SeedAgent idempotently creates ONE active agent for (role, owner). Feature
// packages (e.g. internal/support) use it to register their own role's agent
// without importing the orchestrator's seeding internals. No-op if an agent with
// that role already exists for the owner, so it's safe to call on every boot.
func SeedAgent(app core.App, name, role, persona, owner string) error {
	n, _ := app.CountRecords(agentCollection,
		dbx.NewExp("role = {:r} AND owner = {:o}", dbx.Params{"r": role, "o": owner}))
	if n > 0 {
		return nil
	}
	col, err := app.FindCollectionByNameOrId(agentCollection)
	if err != nil {
		return err
	}
	cfg := configFromEnv()
	rec := core.NewRecord(col)
	rec.Set("name", name)
	rec.Set("role", role)
	rec.Set("persona", persona)
	rec.Set("provider", cfg.provider)
	rec.Set("model", cfg.model)
	rec.Set("owner", owner)
	rec.Set("status", "active")
	return app.Save(rec)
}

// SeedTeam seeds the default software team for the legacy/system tenant. It's the
// boot-time global seed; per-tenant seeding goes through SeedTeamForOwner.
func SeedTeam(app core.App) {
	SeedTeamForOwner(app, SystemOwner)
}

// SeedTeamForOwner creates the default software team owned by ownerID if that tenant
// has no agents yet. Idempotent per tenant — a second call for the same owner is a
// no-op, and seeding one tenant never touches another's agents.
func SeedTeamForOwner(app core.App, ownerID string) {
	if n, _ := app.CountRecords(agentCollection, dbx.NewExp("owner = {:o}", dbx.Params{"o": ownerID})); n > 0 {
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
		rec.Set("owner", ownerID)
		rec.Set("status", "active")
		if err := app.Save(rec); err != nil {
			app.Logger().Error("orchestrator: seed agent failed", "name", m.name, "owner", ownerID, "err", err)
		}
	}
}
