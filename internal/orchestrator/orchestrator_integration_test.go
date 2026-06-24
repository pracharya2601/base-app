package orchestrator

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(func() { app.Cleanup() })
	return app
}

func TestEnsureSchemaAndSeed(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	// Collections exist and are locked.
	for _, name := range []string{agentCollection, taskCollection, runCollection} {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatalf("collection %s missing: %v", name, err)
		}
		if !c.System {
			t.Errorf("%s should be System=true", name)
		}
	}
	// _tasks.agent is a relation to _agents.
	tasks, _ := app.FindCollectionByNameOrId(taskCollection)
	agents, _ := app.FindCollectionByNameOrId(agentCollection)
	rel, ok := tasks.Fields.GetByName("agent").(*core.RelationField)
	if !ok || rel.CollectionId != agents.Id {
		t.Error("_tasks.agent should be a relation to _agents")
	}

	// Idempotent.
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema second pass: %v", err)
	}

	// Seed the team.
	SeedTeam(app)
	n, _ := app.CountRecords(agentCollection)
	if n != 3 {
		t.Fatalf("seeded agents = %d, want 3", n)
	}
	for _, role := range []string{RolePM, RoleEngineer, RoleReviewer} {
		if _, err := activeAgentByRole(app, role); err != nil {
			t.Errorf("no active agent for role %q after seed", role)
		}
	}
	// Seeding again must not duplicate.
	SeedTeam(app)
	if n2, _ := app.CountRecords(agentCollection); n2 != 3 {
		t.Errorf("agents after second seed = %d, want 3", n2)
	}
}

// Approving a needs_review task must mark it done AND hand off to the next role
// with the draft as input — without any LLM call (we pre-set the output).
func TestApproveHandsOffToNextRole(t *testing.T) {
	scenario := tests.ApiScenario{
		Name:   "approve PM task -> hands off to engineer",
		Method: http.MethodPost,
		// URL filled in BeforeTestFunc once we know the task id.
		ExpectedStatus:  200,
		ExpectedContent: []string{`"state":"done"`, `"role":"engineer"`, "Ed (Engineer)"},
	}

	var taskID string
	scenario.TestAppFactory = func(t testing.TB) *tests.TestApp {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatalf("NewTestApp: %v", err)
		}
		if err := EnsureSchema(app); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		SeedTeam(app)

		// A PM task that has already produced a draft and is awaiting review.
		pm, err := activeAgentByRole(app, RolePM)
		if err != nil {
			t.Fatalf("pm agent: %v", err)
		}
		task, err := createTask(app, "Add OAuth login", "", pm.Id, "", "")
		if err != nil {
			t.Fatalf("createTask: %v", err)
		}
		task.Set("state", StateNeedsReview)
		task.Set("output", "## Spec\n- as a user I can log in with OAuth")
		if err := app.Save(task); err != nil {
			t.Fatalf("save review task: %v", err)
		}
		taskID = task.Id
		return app
	}
	scenario.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		RegisterRoutes(e, app)
		scenario.URL = "/api/orchestrator/tasks/" + taskID + "/approve"

		// Mint a superuser token for the (superuser-bound) approve route.
		su, err := app.FindFirstRecordByFilter(core.CollectionNameSuperusers, "1=1")
		if err != nil {
			t.Fatalf("find superuser: %v", err)
		}
		token, err := su.NewAuthToken()
		if err != nil {
			t.Fatalf("mint superuser token: %v", err)
		}
		scenario.Headers = map[string]string{"Authorization": token}
	}
	scenario.AfterTestFunc = func(t testing.TB, app *tests.TestApp, _ *http.Response) {
		// The approved task is done; exactly one engineer handoff task now exists,
		// pending, carrying the PM's draft as its description.
		done, err := app.FindRecordById(taskCollection, taskID)
		if err != nil {
			t.Fatalf("reload task: %v", err)
		}
		if done.GetString("state") != StateDone {
			t.Errorf("approved task state = %q, want done", done.GetString("state"))
		}
		eng, _ := activeAgentByRole(app, RoleEngineer)
		kids, err := app.FindRecordsByFilter(taskCollection, "agent = {:a} && state = {:s}", "created", 5, 0,
			dbx.Params{"a": eng.Id, "s": StatePending})
		if err != nil || len(kids) != 1 {
			t.Fatalf("expected exactly 1 pending engineer task, got %d (err %v)", len(kids), err)
		}
		if !strings.Contains(kids[0].GetString("description"), "OAuth") {
			t.Errorf("handoff task should carry the PM draft, got %q", kids[0].GetString("description"))
		}
		if kids[0].GetString("parentTask") != taskID {
			t.Errorf("handoff parentTask = %q, want %q", kids[0].GetString("parentTask"), taskID)
		}
	}

	scenario.Test(t)
}
