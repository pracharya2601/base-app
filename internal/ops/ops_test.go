package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"base-app/internal/orchestrator"
	"base-app/internal/provision"
)

func setup(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(func() { app.Cleanup() })
	if err := orchestrator.EnsureSchema(app); err != nil {
		t.Fatalf("orchestrator.EnsureSchema: %v", err)
	}
	Register(app)
	return app
}

// An ops task to attach a proposal to.
func opsTask(t *testing.T, app core.App) *core.Record {
	t.Helper()
	task, err := orchestrator.EnqueueTask(app, orchestrator.EnqueueOpts{
		Title: "add a blog", Kind: KindOpsCommand, Role: RoleOps,
	})
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	return task
}

func TestRegisterSeedsOpsAgentAndTools(t *testing.T) {
	app := setup(t)
	n, _ := app.CountRecords(orchestrator.AgentCollection, dbx.NewExp("role = {:r}", dbx.Params{"r": RoleOps}))
	if n != 1 {
		t.Fatalf("ops agents = %d, want 1", n)
	}
	if !orchestrator.HasTaskTools(KindOpsCommand) {
		t.Error("no tools registered for ops_command")
	}
	if !orchestrator.HasApproveAction(KindOpsCommand) {
		t.Error("ops_command should be action-kind (human-approval-required)")
	}
}

// THE GUARD: propose_schema must PROPOSE the change (recorded, pending) and must
// NOT create the collection. The executor creates it only on approval.
func TestSchemaIsProposedNotApplied(t *testing.T) {
	app := setup(t)
	task := opsTask(t, app)

	tools := opsTools(app, task)
	if len(tools) != 1 || tools[0].Name != "propose_schema" {
		t.Fatalf("expected a propose_schema tool, got %#v", tools)
	}
	args := `{"collections":[{"name":"blog_posts","fields":[{"name":"title","type":"text"},{"name":"body","type":"text"}],"rbac":true}]}`
	out, err := tools[0].Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("propose_schema execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "queued") {
		t.Errorf("expected a queued message, got: %s", out)
	}
	// Guard: the collection does NOT exist yet.
	if _, err := app.FindCollectionByNameOrId("blog_posts"); err == nil {
		t.Fatal("blog_posts was created at propose time — the guard failed")
	}
	// A proposal was recorded for the task.
	pa, err := app.FindFirstRecordByFilter("_proposedActions",
		"task = {:t} && actionKind = {:k}", dbx.Params{"t": task.Id, "k": ActionProvisionSchema})
	if err != nil || pa.GetString("status") != "proposed" {
		t.Fatalf("expected a proposed schema row, got %v (err %v)", pa, err)
	}

	// The executor (on approval) applies the schema for real.
	if _, err := executeProvision(app, task, json.RawMessage(args)); err != nil {
		t.Fatalf("executeProvision: %v", err)
	}
	col, err := app.FindCollectionByNameOrId("blog_posts")
	if err != nil {
		t.Fatalf("blog_posts not created after executor: %v", err)
	}
	for _, f := range []string{"title", "body"} {
		if col.Fields.GetByName(f) == nil {
			t.Errorf("blog_posts missing field %q", f)
		}
	}
	// rbac:true should have produced access rules via the shared provision core.
	if col.CreateRule == nil {
		t.Error("expected rbac create rule to be set")
	}
}

// executeProvision surfaces bad params as an error (so approval stays open).
func TestExecuteProvisionBadParams(t *testing.T) {
	app := setup(t)
	if _, err := executeProvision(app, nil, json.RawMessage(`not json`)); err == nil {
		t.Error("expected an error for malformed params")
	}
	// Sanity: the shared core is what applies (smoke).
	if _, err := provision.Apply(app, provision.Spec{}); err != nil {
		t.Errorf("empty spec should be a no-op, got: %v", err)
	}
}
