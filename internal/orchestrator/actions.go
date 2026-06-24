package orchestrator

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/ai"
)

// This file is the seam that turns the orchestrator from a fixed software-delivery
// pipeline into a generic engine that can run ANY "company function". A feature
// package (e.g. internal/support) plugs in through two calls:
//
//   - EnqueueTask        — put work on the queue (the engine drafts it like any task)
//   - RegisterApproveAction — say what "approve" DOES for that kind of task
//
// The engine never imports the feature package; it only calls back through the
// registered func. A task's `kind` selects the action. kind == "" is the built-in
// software pipeline (linear role handoff); a registered kind runs its action on
// approve INSTEAD of a handoff, and is never auto-advanced by autopilot (a human
// must approve, because the action has real side effects — e.g. emailing a
// customer). This is the foundation the configurable multi-function company is
// built on.

// ApproveAction is what runs when an operator approves a task of a given kind.
// It performs the real-world effect of the work (send the reply, provision the
// schema, …). It runs BEFORE the task is marked done; returning an error leaves
// the task in needs_review so the operator can retry.
type ApproveAction func(app core.App, task *core.Record) error

var (
	approveActionsMu sync.RWMutex
	approveActions   = map[string]ApproveAction{}
)

// RegisterApproveAction binds an action to a task kind. Call it once at startup
// (during OnServe, before requests are served). Re-registering a kind replaces it.
func RegisterApproveAction(kind string, fn ApproveAction) {
	approveActionsMu.Lock()
	defer approveActionsMu.Unlock()
	approveActions[kind] = fn
}

// approveActionFor returns the action registered for a kind, if any.
func approveActionFor(kind string) (ApproveAction, bool) {
	approveActionsMu.RLock()
	defer approveActionsMu.RUnlock()
	fn, ok := approveActions[kind]
	return fn, ok
}

// HasApproveAction reports whether a kind has a registered action — i.e. its
// approval has a real side effect, so the engine must NOT auto-advance it under
// autopilot.
func HasApproveAction(kind string) bool {
	_, ok := approveActionFor(kind)
	return ok
}

// ToolProvider returns the tools the agent may call while drafting a task of a
// given kind. It's a func (not a static list) so tools can close over the app
// (to query the DB) and the specific task. Called once per draft.
type ToolProvider func(app core.App, task *core.Record) []ai.Tool

var (
	taskToolsMu sync.RWMutex
	taskTools   = map[string]ToolProvider{}
)

// RegisterTaskTools binds a tool set to a task kind. When the loop drafts a task
// of that kind it uses ai.GenerateWithTools so the agent can ACT (look something
// up, etc.) mid-draft instead of only describing. Call once at startup.
func RegisterTaskTools(kind string, provider ToolProvider) {
	taskToolsMu.Lock()
	defer taskToolsMu.Unlock()
	taskTools[kind] = provider
}

// toolsForKind returns the tool provider registered for a kind, if any.
func toolsForKind(kind string) (ToolProvider, bool) {
	taskToolsMu.RLock()
	defer taskToolsMu.RUnlock()
	p, ok := taskTools[kind]
	return p, ok
}

// HasTaskTools reports whether a kind has registered tools (so its draft runs the
// auto tool loop).
func HasTaskTools(kind string) bool {
	_, ok := toolsForKind(kind)
	return ok
}

// ---- proposed actions: the write-tool approval guard ----------------------
//
// A mutating tool must NOT act mid-draft (that would bypass the human approval the
// whole draft→needs_review→approve flow exists for). Instead it calls ProposeAction
// to record the intended side effect in _proposedActions. On APPROVE the engine
// runs every pending proposal (via the executor registered for its actionKind)
// before the task's ApproveAction; on REJECT/REVISE the proposals are discarded,
// never executed. The human approving the task IS the authorization.

// Proposed-action lifecycle statuses.
const (
	proposedProposed  = "proposed"
	proposedExecuted  = "executed"
	proposedDiscarded = "discarded"
	proposedFailed    = "failed"
)

var proposedStatuses = []string{proposedProposed, proposedExecuted, proposedDiscarded, proposedFailed}

// ActionExecutor performs a proposed side effect on approval. params is the JSON
// the proposing tool recorded; the returned string is stored as the outcome. A
// non-nil error aborts approval and leaves the task in needs_review.
type ActionExecutor func(app core.App, task *core.Record, params json.RawMessage) (string, error)

var (
	actionExecutorsMu sync.RWMutex
	actionExecutors   = map[string]ActionExecutor{}
)

// RegisterActionExecutor binds an executor to a proposed-action kind. Call once at
// startup. A write-tool's ProposeAction(actionKind, …) and its executor here must
// use the same actionKind string.
func RegisterActionExecutor(actionKind string, fn ActionExecutor) {
	actionExecutorsMu.Lock()
	defer actionExecutorsMu.Unlock()
	actionExecutors[actionKind] = fn
}

func actionExecutorFor(actionKind string) (ActionExecutor, bool) {
	actionExecutorsMu.RLock()
	defer actionExecutorsMu.RUnlock()
	fn, ok := actionExecutors[actionKind]
	return fn, ok
}

// ProposeAction records a side effect a write-tool wants to perform, pending human
// approval of taskID. It does NOT perform the effect. Exported so feature-package
// tools can call it from their Execute funcs. params is JSON-marshaled and handed
// back to the executor verbatim on approval.
func ProposeAction(app core.App, taskID, actionKind string, params any) error {
	col, err := app.FindCollectionByNameOrId(proposedActionCollection)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal proposed action params: %w", err)
	}
	rec := core.NewRecord(col)
	rec.Set("task", taskID)
	rec.Set("actionKind", actionKind)
	rec.Set("params", string(raw))
	rec.Set("status", proposedProposed)
	return app.Save(rec)
}

// pendingProposals returns a task's proposals that still need to run (proposed or a
// prior failed attempt — so re-approving retries a failure rather than skipping it).
func pendingProposals(app core.App, taskID string) ([]*core.Record, error) {
	return app.FindRecordsByFilter(proposedActionCollection,
		"task = {:t} && status != {:e} && status != {:d}", "created", 200, 0,
		dbx.Params{"t": taskID, "e": proposedExecuted, "d": proposedDiscarded})
}

// executeProposedActions runs every pending proposal for a task, in order. It stops
// at the first failure (marking that proposal failed) and returns the error, which
// keeps the task in needs_review so the operator can fix or reject. Tasks with no
// proposals (the common case) are a no-op.
func executeProposedActions(app core.App, task *core.Record) error {
	recs, err := pendingProposals(app, task.Id)
	if err != nil {
		return err
	}
	for _, r := range recs {
		kind := r.GetString("actionKind")
		exec, ok := actionExecutorFor(kind)
		if !ok {
			r.Set("status", proposedFailed)
			r.Set("result", "no executor registered for action kind")
			_ = app.Save(r)
			return fmt.Errorf("no executor registered for proposed action %q", kind)
		}
		result, eerr := exec(app, task, json.RawMessage(r.GetString("params")))
		if eerr != nil {
			r.Set("status", proposedFailed)
			r.Set("result", eerr.Error())
			_ = app.Save(r)
			return fmt.Errorf("proposed action %q failed: %w", kind, eerr)
		}
		r.Set("status", proposedExecuted)
		r.Set("result", result)
		if serr := app.Save(r); serr != nil {
			return serr
		}
	}
	return nil
}

// discardProposedActions marks a task's still-pending proposals discarded (on
// reject/revise) so they can never execute.
func discardProposedActions(app core.App, taskID string) {
	recs, err := pendingProposals(app, taskID)
	if err != nil {
		return
	}
	for _, r := range recs {
		r.Set("status", proposedDiscarded)
		_ = app.Save(r)
	}
}

// proposalViews returns a task's proposed actions as JSON-friendly maps for the
// detail endpoint, so an operator sees exactly what they're authorizing.
func proposalViews(app core.App, taskID string) []map[string]any {
	recs, err := app.FindRecordsByFilter(proposedActionCollection,
		"task = {:t}", "created", 200, 0, dbx.Params{"t": taskID})
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, map[string]any{
			"id":         r.Id,
			"actionKind": r.GetString("actionKind"),
			"params":     r.GetString("params"),
			"status":     r.GetString("status"),
			"result":     r.GetString("result"),
			"created":    r.GetDateTime("created").String(),
		})
	}
	return out
}

// EnqueueOpts describes a unit of work to put on the orchestrator queue.
type EnqueueOpts struct {
	Title       string
	Description string
	Kind        string // "" = software pipeline; else an approve-action kind
	AgentID     string // assign by explicit agent id, OR…
	Role        string // …by role (first active agent of that role)
	ParentID    string // lineage link (optional)
	CreatedBy   string // who/what created it (defaults to "system")
}

// EnqueueTask creates a pending task the always-on loop will pick up on its next
// tick. It's the exported entry point feature packages and triggers use to feed
// work to the engine. Resolves the agent by AgentID or Role.
func EnqueueTask(app core.App, o EnqueueOpts) (*core.Record, error) {
	agent, err := resolveAgent(app, o.AgentID, o.Role)
	if err != nil {
		return nil, err
	}
	createdBy := o.CreatedBy
	if createdBy == "" {
		createdBy = "system"
	}
	return createTask(app, o.Title, o.Description, agent.Id, o.ParentID, createdBy, o.Kind)
}
