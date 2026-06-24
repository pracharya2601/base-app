package orchestrator

import (
	"encoding/json"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// helper: enqueue a task to attach proposals to.
func aTask(t *testing.T, app core.App) *core.Record {
	t.Helper()
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)
	task, err := EnqueueTask(app, EnqueueOpts{Title: "x", Role: RolePM})
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	return task
}

func countPending(t *testing.T, app core.App, taskID string) int {
	t.Helper()
	recs, err := pendingProposals(app, taskID)
	if err != nil {
		t.Fatalf("pendingProposals: %v", err)
	}
	return len(recs)
}

// The write-tool guard: a proposed action is recorded but NOT executed until
// executeProposedActions runs (i.e. until a human approves the task).
func TestProposedActionExecutesOnlyOnApprove(t *testing.T) {
	app := newTestApp(t)
	task := aTask(t, app)

	ran := false
	RegisterActionExecutor("test_action", func(a core.App, tk *core.Record, params json.RawMessage) (string, error) {
		var p struct {
			V string `json:"v"`
		}
		_ = json.Unmarshal(params, &p)
		ran = true
		return "did:" + p.V, nil
	})

	if err := ProposeAction(app, task.Id, "test_action", map[string]any{"v": "hi"}); err != nil {
		t.Fatalf("ProposeAction: %v", err)
	}
	if ran {
		t.Fatal("proposing must NOT execute the action")
	}
	if countPending(t, app, task.Id) != 1 {
		t.Fatalf("want 1 pending proposal, got %d", countPending(t, app, task.Id))
	}

	if err := executeProposedActions(app, task); err != nil {
		t.Fatalf("executeProposedActions: %v", err)
	}
	if !ran {
		t.Error("approve should have executed the proposed action")
	}
	if countPending(t, app, task.Id) != 0 {
		t.Errorf("proposal should be executed (no longer pending), got %d pending", countPending(t, app, task.Id))
	}
	// Re-running is a no-op (nothing pending) — idempotent.
	if err := executeProposedActions(app, task); err != nil {
		t.Errorf("second execute should be a no-op, got: %v", err)
	}
}

// Discard (reject/revise) prevents a proposal from ever executing.
func TestDiscardProposedActions(t *testing.T) {
	app := newTestApp(t)
	task := aTask(t, app)
	RegisterActionExecutor("test_action2", func(a core.App, tk *core.Record, p json.RawMessage) (string, error) {
		t.Error("discarded action must never execute")
		return "", nil
	})
	if err := ProposeAction(app, task.Id, "test_action2", map[string]any{}); err != nil {
		t.Fatalf("ProposeAction: %v", err)
	}
	discardProposedActions(app, task.Id)
	if countPending(t, app, task.Id) != 0 {
		t.Error("discarded proposals should not be pending")
	}
	if err := executeProposedActions(app, task); err != nil { // nothing to run
		t.Errorf("execute after discard should be a no-op, got: %v", err)
	}
}

// An unknown action kind aborts approval (and marks the proposal failed).
func TestExecuteUnknownProposedActionErrors(t *testing.T) {
	app := newTestApp(t)
	task := aTask(t, app)
	if err := ProposeAction(app, task.Id, "no_executor_kind", map[string]any{}); err != nil {
		t.Fatalf("ProposeAction: %v", err)
	}
	if err := executeProposedActions(app, task); err == nil {
		t.Error("executeProposedActions should error when no executor is registered")
	}
	rec, _ := app.FindFirstRecordByFilter(proposedActionCollection,
		"task = {:t}", dbx.Params{"t": task.Id})
	if rec == nil || rec.GetString("status") != proposedFailed {
		t.Error("a failed proposal should be marked failed")
	}
}

func TestApproveActionRegistry(t *testing.T) {
	const kind = "test_kind_unique"
	if HasApproveAction(kind) {
		t.Fatalf("kind %q should not be registered yet", kind)
	}
	called := false
	RegisterApproveAction(kind, func(app core.App, task *core.Record) error {
		called = true
		return nil
	})
	if !HasApproveAction(kind) {
		t.Fatalf("kind %q should be registered after RegisterApproveAction", kind)
	}
	fn, ok := approveActionFor(kind)
	if !ok {
		t.Fatalf("approveActionFor(%q) returned ok=false", kind)
	}
	if err := fn(nil, nil); err != nil {
		t.Fatalf("action returned error: %v", err)
	}
	if !called {
		t.Error("registered action was not invoked")
	}
}

// EnqueueTask must resolve the agent by role and stamp the task with its kind so
// the approve path can dispatch to the right action.
func TestEnqueueTaskSetsKind(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)

	task, err := EnqueueTask(app, EnqueueOpts{
		Title: "draft a spec",
		Kind:  "some_kind",
		Role:  RolePM,
	})
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	if task.GetString("kind") != "some_kind" {
		t.Errorf("kind = %q, want some_kind", task.GetString("kind"))
	}
	if task.GetString("state") != StatePending {
		t.Errorf("state = %q, want pending", task.GetString("state"))
	}
	if task.GetString("createdBy") != "system" {
		t.Errorf("createdBy = %q, want system (default)", task.GetString("createdBy"))
	}
	// Unknown role must error rather than enqueue an unassigned task.
	if _, err := EnqueueTask(app, EnqueueOpts{Title: "x", Role: "nope"}); err == nil {
		t.Error("EnqueueTask with unknown role should error")
	}
}
