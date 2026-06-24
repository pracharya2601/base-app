package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"base-app/internal/ai"
	"base-app/internal/orchestrator"
	"base-app/internal/provision"
)

func findTool(t *testing.T, tools []ai.Tool, name string) ai.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)
	return ai.Tool{}
}

// ensureTestRBAC stands up minimal _permissions/_roles collections (the real ones
// are created by the root roles.go at boot, which the test harness doesn't run).
func ensureTestRBAC(t *testing.T, app core.App) {
	t.Helper()
	perms := core.NewBaseCollection("_permissions")
	perms.System = true
	perms.Fields.Add(&core.TextField{Name: "token", Required: true})
	if err := app.Save(perms); err != nil {
		t.Fatalf("create _permissions: %v", err)
	}
	roles := core.NewBaseCollection("_roles")
	roles.System = true
	roles.Fields.Add(&core.TextField{Name: "name", Required: true})
	roles.Fields.Add(&core.TextField{Name: "description"})
	roles.Fields.Add(&core.RelationField{Name: "permissions", CollectionId: perms.Id, MaxSelect: 50})
	if err := app.Save(roles); err != nil {
		t.Fatalf("create _roles: %v", err)
	}
	// The real app's migrateAndSeedRBAC adds users.roles; mirror it for assign tests.
	if users, err := app.FindCollectionByNameOrId(userCollection); err == nil && users.Fields.GetByName("roles") == nil {
		users.Fields.Add(&core.RelationField{Name: "roles", CollectionId: roles.Id, MaxSelect: 20})
		if err := app.Save(users); err != nil {
			t.Fatalf("add users.roles: %v", err)
		}
	}
}

func newUser(t *testing.T, app core.App, email string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(userCollection)
	if err != nil {
		t.Fatalf("users collection: %v", err)
	}
	u := core.NewRecord(col)
	u.Set("email", email)
	u.SetPassword("password123")
	if err := app.Save(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

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

	tool := findTool(t, opsTools(app, task), "propose_schema")
	args := `{"collections":[{"name":"blog_posts","fields":[{"name":"title","type":"text"},{"name":"body","type":"text"}],"rbac":true}]}`
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
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

// THE GUARD (role tool): manage_role must PROPOSE the role and NOT create it; the
// executor creates the role + its permission tokens only on approval.
func TestRoleIsProposedNotApplied(t *testing.T) {
	app := setup(t)
	ensureTestRBAC(t, app)
	task := opsTask(t, app)

	tool := findTool(t, opsTools(app, task), "manage_role")
	args := `{"roleName":"support-readonly","description":"read support data","permissions":["orders:read","support_tickets:read"]}`
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("manage_role execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "queued") {
		t.Errorf("expected a queued message, got: %s", out)
	}
	// Guard: no role created yet.
	if _, err := app.FindFirstRecordByFilter(roleCollection, "name = {:n}", dbx.Params{"n": "support-readonly"}); err == nil {
		t.Fatal("role was created at propose time — the guard failed")
	}

	// Executor (on approval) creates the role + permission tokens.
	if _, err := executeManageRole(app, task, json.RawMessage(args)); err != nil {
		t.Fatalf("executeManageRole: %v", err)
	}
	role, err := app.FindFirstRecordByFilter(roleCollection, "name = {:n}", dbx.Params{"n": "support-readonly"})
	if err != nil {
		t.Fatalf("role not created after executor: %v", err)
	}
	if got := len(role.GetStringSlice("permissions")); got != 2 {
		t.Errorf("role permissions = %d, want 2", got)
	}
	// Both tokens exist as _permissions records.
	for _, tok := range []string{"orders:read", "support_tickets:read"} {
		if _, err := app.FindFirstRecordByFilter(permissionCollection, "token = {:t}", dbx.Params{"t": tok}); err != nil {
			t.Errorf("permission token %q not created", tok)
		}
	}
	// Re-running (upsert) must not duplicate the role.
	if _, err := executeManageRole(app, task, json.RawMessage(args)); err != nil {
		t.Fatalf("executeManageRole re-run: %v", err)
	}
	if n, _ := app.CountRecords(roleCollection, dbx.NewExp("name = {:n}", dbx.Params{"n": "support-readonly"})); n != 1 {
		t.Errorf("role upsert duplicated: count=%d", n)
	}
}

// THE GUARD (assign tool): assign_user_roles must PROPOSE and not change the user;
// the executor sets the user's roles only on approval (and errors on unknown roles).
func TestAssignUserRolesIsProposedNotApplied(t *testing.T) {
	app := setup(t)
	ensureTestRBAC(t, app)
	user := newUser(t, app, "alice@example.com")
	// A role to assign.
	task := opsTask(t, app)
	if _, err := executeManageRole(app, task, json.RawMessage(`{"roleName":"support-readonly","permissions":["orders:read"]}`)); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	tool := findTool(t, opsTools(app, task), "assign_user_roles")
	args := `{"userEmail":"alice@example.com","roles":["support-readonly"]}`
	out, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("assign_user_roles execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "queued") {
		t.Errorf("expected a queued message, got: %s", out)
	}
	// Guard: user has no roles yet.
	user, _ = app.FindRecordById(userCollection, user.Id)
	if len(user.GetStringSlice("roles")) != 0 {
		t.Fatal("user roles changed at propose time — the guard failed")
	}

	// Executor (on approval) assigns the role.
	if _, err := executeAssignUserRoles(app, task, json.RawMessage(args)); err != nil {
		t.Fatalf("executeAssignUserRoles: %v", err)
	}
	user, _ = app.FindRecordById(userCollection, user.Id)
	if got := len(user.GetStringSlice("roles")); got != 1 {
		t.Errorf("user roles = %d, want 1", got)
	}

	// Unknown role -> error (and the user is untouched by that attempt).
	if _, err := executeAssignUserRoles(app, task, json.RawMessage(`{"userEmail":"alice@example.com","roles":["does-not-exist"]}`)); err == nil {
		t.Error("expected an error assigning a non-existent role")
	}
	// Unknown user -> error.
	if _, err := executeAssignUserRoles(app, task, json.RawMessage(`{"userEmail":"nobody@example.com","roles":["support-readonly"]}`)); err == nil {
		t.Error("expected an error for unknown user")
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
