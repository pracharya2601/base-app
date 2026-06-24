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

// EnsureSchema must create the owner-keyed _orchConfigs collection and backfill the
// `owner` field onto _agents/_tasks/_runs.
func TestMultiTenantSchema(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	cfg, err := app.FindCollectionByNameOrId(configCollection)
	if err != nil {
		t.Fatalf("_orchConfigs missing: %v", err)
	}
	if !cfg.System {
		t.Error("_orchConfigs should be System=true")
	}
	for _, f := range []string{"owner", "autopilot", "dailyTokenBudget", "provider"} {
		if cfg.Fields.GetByName(f) == nil {
			t.Errorf("_orchConfigs missing field %q", f)
		}
	}
	for _, name := range []string{agentCollection, taskCollection, runCollection} {
		c, _ := app.FindCollectionByNameOrId(name)
		if c.Fields.GetByName("owner") == nil {
			t.Errorf("%s missing owner field", name)
		}
	}
	// Idempotent (re-running must not error on the now-existing owner fields).
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema second pass: %v", err)
	}
}

// SeedTeamForOwner is per-tenant idempotent: each owner gets its own 3 agents, and
// re-seeding the same owner is a no-op.
func TestSeedTeamForOwnerIsolation(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeamForOwner(app, "userA")
	SeedTeamForOwner(app, "userA") // no-op
	SeedTeamForOwner(app, "userB")

	for _, owner := range []string{"userA", "userB"} {
		n, _ := app.CountRecords(agentCollection, dbx.NewExp("owner = {:o}", dbx.Params{"o": owner}))
		if n != 3 {
			t.Errorf("owner %q should have 3 agents, got %d", owner, n)
		}
	}
	total, _ := app.CountRecords(agentCollection)
	if total != 6 {
		t.Errorf("two tenants => 6 agents total, got %d", total)
	}
}

// seedRunOwned inserts a run tagged with an owner + token total for budget tests.
func seedRunOwned(t testing.TB, app core.App, owner string, tokens int) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(runCollection)
	if err != nil {
		t.Fatalf("runs collection: %v", err)
	}
	r := core.NewRecord(col)
	r.Set("owner", owner)
	r.Set("status", "ok")
	r.Set("totalTokens", tokens)
	if err := app.Save(r); err != nil {
		t.Fatalf("save run: %v", err)
	}
}

// tokensUsedSince must sum per-tenant when given an owner, and across all tenants
// when given "". This is the airtight budget-isolation guarantee.
func TestTokenBudgetIsolatedByOwner(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	seedRunOwned(t, app, "userA", 100)
	seedRunOwned(t, app, "userA", 50)
	seedRunOwned(t, app, "userB", 7)

	since := nowMinus24h()
	if got := tokensUsedSince(app, "userA", since); got != 150 {
		t.Errorf("userA spend = %d, want 150", got)
	}
	if got := tokensUsedSince(app, "userB", since); got != 7 {
		t.Errorf("userB spend = %d, want 7 (must not see userA's spend)", got)
	}
	if got := tokensUsedSince(app, "", since); got != 157 {
		t.Errorf("global spend = %d, want 157", got)
	}
}

// advanceTask is the shared approve/autopilot logic: it marks the task done and
// spawns the next-role handoff — except at the end of the pipeline, where it just
// finishes.
func TestAdvanceTaskEndOfPipeline(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)
	rev, _ := activeAgentByRole(app, RoleReviewer)
	task, err := createTask(app, "review the code", "looks fine", rev.Id, "", "")
	if err != nil {
		t.Fatalf("createTask: %v", err)
	}
	child, na, next, err := advanceTask(app, task, "autopilot")
	if err != nil {
		t.Fatalf("advanceTask: %v", err)
	}
	if child != nil || na != nil || next != "" {
		t.Errorf("reviewer is last in pipeline; expected no handoff, got next=%q child=%v", next, child)
	}
	if got := task.GetString("state"); got != StateDone {
		t.Errorf("task state = %q, want done", got)
	}
}

// Config is DB-driven: loadOrchConfig overlays the _orchConfigs row on the env
// defaults. autopilot is authoritative once a row exists; numbers/strings overlay
// only when set.
func TestConfigIsDBDriven(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if loadOrchConfig(app, SystemOwner).autoApprove {
		t.Fatal("expected autopilot off by default (no row)")
	}
	if _, err := upsertOrchConfig(app, SystemOwner, func(r *core.Record) { r.Set("autopilot", true) }); err != nil {
		t.Fatalf("upsert autopilot: %v", err)
	}
	if !loadOrchConfig(app, SystemOwner).autoApprove {
		t.Fatal("expected autopilot on after upsert")
	}
	if _, err := upsertOrchConfig(app, SystemOwner, func(r *core.Record) {
		r.Set("maxTokens", 1234)
		r.Set("provider", "openai")
	}); err != nil {
		t.Fatalf("upsert overlay: %v", err)
	}
	c := loadOrchConfig(app, SystemOwner)
	if c.maxTokens != 1234 || c.provider != "openai" || !c.autoApprove {
		t.Errorf("overlay failed: maxTokens=%d provider=%s autopilot=%v", c.maxTokens, c.provider, c.autoApprove)
	}
}

// revisionPrompt must carry BOTH the prior draft and the feedback, and demand the
// complete revised draft (so downstream stages never have to stitch a diff).
func TestRevisionPrompt(t *testing.T) {
	p := revisionPrompt("Add OAuth", "the original ask", "## prior draft body", "fix the token refresh bug")
	for _, want := range []string{"COMPLETE", "## prior draft body", "fix the token refresh bug", "Add OAuth", "the original ask"} {
		if !strings.Contains(p, want) {
			t.Errorf("revisionPrompt missing %q in:\n%s", want, p)
		}
	}
	// Empty feedback gets a sensible default rather than a blank section.
	if p := revisionPrompt("t", "", "draft", ""); !strings.Contains(p, "no specific feedback") {
		t.Errorf("empty feedback should produce a default instruction, got:\n%s", p)
	}
}

// seedRun inserts a logged run for a task so revision-count logic has data.
func seedRun(t testing.TB, app core.App, taskID, agentID string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(runCollection)
	if err != nil {
		t.Fatalf("runs collection: %v", err)
	}
	r := core.NewRecord(col)
	r.Set("task", taskID)
	r.Set("agent", agentID)
	r.Set("status", "ok")
	if err := app.Save(r); err != nil {
		t.Fatalf("save run: %v", err)
	}
}

// revisionsSoFar = total runs minus the original draft (never negative).
func TestRevisionsSoFar(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)
	eng, _ := activeAgentByRole(app, RoleEngineer)
	task, _ := createTask(app, "build it", "", eng.Id, "", "")

	if got := revisionsSoFar(app, task.Id); got != 0 {
		t.Errorf("no runs => 0 revisions, got %d", got)
	}
	seedRun(t, app, task.Id, eng.Id) // the original draft
	if got := revisionsSoFar(app, task.Id); got != 0 {
		t.Errorf("original draft alone => 0 revisions, got %d", got)
	}
	seedRun(t, app, task.Id, eng.Id) // one rework pass
	if got := revisionsSoFar(app, task.Id); got != 1 {
		t.Errorf("after one rework => 1 revision, got %d", got)
	}
}

// The spend guard: a revision attempt past the cap must FAIL the task without an
// LLM call (processTask returns before ai.Generate, so no run is logged).
func TestProcessTaskRevisionCapFailsWithoutLLM(t *testing.T) {
	// config{} below has autoApprove=false, so the needs_review branch is asserted.
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)
	eng, _ := activeAgentByRole(app, RoleEngineer)

	// A re-queued task: pending, with a prior draft and feedback.
	task, _ := createTask(app, "build it", "", eng.Id, "", "")
	task.Set("output", "## prior draft")
	task.Set("errorMsg", "address the edge cases")
	task.Set("state", StatePending)
	if err := app.Save(task); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Already at the cap: 1 original + 2 reworks = 3 runs => revisionsSoFar == 2.
	seedRun(t, app, task.Id, eng.Id)
	seedRun(t, app, task.Id, eng.Id)
	seedRun(t, app, task.Id, eng.Id)

	before, _ := app.CountRecords(runCollection, dbx.NewExp("task = {:t}", dbx.Params{"t": task.Id}))
	processTask(app, config{maxRevisions: 2, provider: "anthropic"}, task)

	reloaded, _ := app.FindRecordById(taskCollection, task.Id)
	if reloaded.GetString("state") != StateFailed {
		t.Errorf("over-cap revision should fail the task, got state %q", reloaded.GetString("state"))
	}
	if !strings.Contains(reloaded.GetString("errorMsg"), "max revisions") {
		t.Errorf("expected max-revisions reason, got %q", reloaded.GetString("errorMsg"))
	}
	after, _ := app.CountRecords(runCollection, dbx.NewExp("task = {:t}", dbx.Params{"t": task.Id}))
	if after != before {
		t.Errorf("cap should short-circuit before any LLM call; runs %d -> %d", before, after)
	}
}

// parseVerdict reads the trailing VERDICT line, case-insensitively, last-wins.
func TestParseVerdict(t *testing.T) {
	cases := map[string]string{
		"looks fine\n\nVERDICT: APPROVE":                    verdictApprove,
		"issues...\nVERDICT: CHANGES_REQUESTED":             verdictChanges,
		"lower case verdict: approve":                       verdictApprove,
		"no verdict line here":                              "",
		"VERDICT: CHANGES_REQUESTED\n...\nVERDICT: APPROVE": verdictApprove, // last wins
		"VERDICT: lgtm ship it":                             verdictApprove,
		"VERDICT: please request changes":                   verdictChanges,
	}
	for in, want := range cases {
		if got := parseVerdict(in); got != want {
			t.Errorf("parseVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}

// In autopilot, a reviewer "changes requested" verdict must re-queue the upstream
// author task to pending (with the review as feedback) and finish the review —
// rather than shipping the rejected draft. No LLM call.
func TestAutopilotReviewGateRequeuesUpstream(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)
	eng, _ := activeAgentByRole(app, RoleEngineer)
	rev, _ := activeAgentByRole(app, RoleReviewer)

	// The engineer's completed draft, and a reviewer task that points back to it.
	engTask, _ := createTask(app, "build feature", "", eng.Id, "", "")
	engTask.Set("output", "## engineer draft v1")
	engTask.Set("state", StateDone)
	_ = app.Save(engTask)
	revTask, _ := createTask(app, "reviewer: build feature", "## engineer draft v1", rev.Id, engTask.Id, "autopilot")

	handled := autopilotReviewGate(app, config{maxRevisions: 3}, revTask, rev,
		"Found a nil-deref.\n\nVERDICT: CHANGES_REQUESTED")
	if !handled {
		t.Fatal("changes-requested verdict should be handled by the gate")
	}
	parent, _ := app.FindRecordById(taskCollection, engTask.Id)
	if parent.GetString("state") != StatePending {
		t.Errorf("upstream task should be re-queued to pending, got %q", parent.GetString("state"))
	}
	if !strings.Contains(parent.GetString("errorMsg"), "nil-deref") {
		t.Errorf("review should be stored as rework feedback, got %q", parent.GetString("errorMsg"))
	}
	if parent.GetString("output") != "## engineer draft v1" {
		t.Errorf("prior draft must survive as rework context, got %q", parent.GetString("output"))
	}
	done, _ := app.FindRecordById(taskCollection, revTask.Id)
	if done.GetString("state") != StateDone {
		t.Errorf("review task should be done, got %q", done.GetString("state"))
	}
}

// The gate must NOT loop on approve, on a non-reviewer agent, or once the upstream
// task has exhausted its revision budget — in all cases it returns false ("advance
// normally") and leaves records untouched.
func TestAutopilotReviewGatePassThrough(t *testing.T) {
	app := newTestApp(t)
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	SeedTeam(app)
	eng, _ := activeAgentByRole(app, RoleEngineer)
	rev, _ := activeAgentByRole(app, RoleReviewer)

	engTask, _ := createTask(app, "build feature", "", eng.Id, "", "")
	engTask.Set("output", "## engineer draft v1")
	engTask.Set("state", StateDone)
	_ = app.Save(engTask)
	revTask, _ := createTask(app, "reviewer: build feature", "## draft", rev.Id, engTask.Id, "autopilot")

	// Approve verdict -> not handled (chain finishes normally).
	if autopilotReviewGate(app, config{maxRevisions: 3}, revTask, rev, "great work\n\nVERDICT: APPROVE") {
		t.Error("approve verdict must not be handled by the gate")
	}
	// Non-reviewer agent -> not handled even with a changes verdict.
	if autopilotReviewGate(app, config{maxRevisions: 3}, revTask, eng, "VERDICT: CHANGES_REQUESTED") {
		t.Error("non-reviewer agent must not trigger the review gate")
	}
	// Changes requested but the upstream task is already at its revision cap.
	seedRun(t, app, engTask.Id, eng.Id)
	seedRun(t, app, engTask.Id, eng.Id) // revisionsSoFar == 1
	if autopilotReviewGate(app, config{maxRevisions: 1}, revTask, rev, "VERDICT: CHANGES_REQUESTED") {
		t.Error("gate must not re-queue once the upstream revision budget is exhausted")
	}
}

// POST /revise on a needs_review draft re-queues the SAME task to pending, keeps
// the prior draft as rework context, and stores the feedback — no LLM call here.
func TestReviseRequeuesTask(t *testing.T) {
	scenario := tests.ApiScenario{
		Name:            "revise PM draft -> re-queued to pending",
		Method:          http.MethodPost,
		Body:            strings.NewReader(`{"feedback":"tighten the acceptance criteria"}`),
		ExpectedStatus:  200,
		ExpectedContent: []string{`"state":"pending"`, `"maxRevisions":`},
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
		pm, _ := activeAgentByRole(app, RolePM)
		task, _ := createTask(app, "Add OAuth login", "", pm.Id, "", "")
		task.Set("state", StateNeedsReview)
		task.Set("output", "## Spec draft")
		if err := app.Save(task); err != nil {
			t.Fatalf("save review task: %v", err)
		}
		taskID = task.Id
		return app
	}
	scenario.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		RegisterRoutes(e, app)
		scenario.URL = "/api/orchestrator/tasks/" + taskID + "/revise"
		su, _ := app.FindFirstRecordByFilter(core.CollectionNameSuperusers, "1=1")
		token, _ := su.NewAuthToken()
		scenario.Headers = map[string]string{"Authorization": token}
	}
	scenario.AfterTestFunc = func(t testing.TB, app *tests.TestApp, _ *http.Response) {
		task, _ := app.FindRecordById(taskCollection, taskID)
		if task.GetString("state") != StatePending {
			t.Errorf("revised task state = %q, want pending", task.GetString("state"))
		}
		if task.GetString("output") != "## Spec draft" {
			t.Errorf("prior draft must be preserved as rework context, got %q", task.GetString("output"))
		}
		if task.GetString("errorMsg") != "tighten the acceptance criteria" {
			t.Errorf("feedback should be stored, got %q", task.GetString("errorMsg"))
		}
	}
	scenario.Test(t)
}

// /revise rejects an empty feedback body (rework must be actionable).
func TestReviseRequiresFeedback(t *testing.T) {
	scenario := tests.ApiScenario{
		Name:            "revise without feedback -> 400",
		Method:          http.MethodPost,
		Body:            strings.NewReader(`{"feedback":"   "}`),
		ExpectedStatus:  400,
		ExpectedContent: []string{"feedback"},
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
		pm, _ := activeAgentByRole(app, RolePM)
		task, _ := createTask(app, "Add OAuth login", "", pm.Id, "", "")
		task.Set("state", StateNeedsReview)
		task.Set("output", "## Spec draft")
		_ = app.Save(task)
		taskID = task.Id
		return app
	}
	scenario.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		RegisterRoutes(e, app)
		scenario.URL = "/api/orchestrator/tasks/" + taskID + "/revise"
		su, _ := app.FindFirstRecordByFilter(core.CollectionNameSuperusers, "1=1")
		token, _ := su.NewAuthToken()
		scenario.Headers = map[string]string{"Authorization": token}
	}
	scenario.Test(t)
}

// GET /tasks lists tasks newest-first and honors the ?role filter.
func TestListTasksFilteredByRole(t *testing.T) {
	scenario := tests.ApiScenario{
		Name:            "list engineer tasks only",
		Method:          http.MethodGet,
		ExpectedStatus:  200,
		ExpectedContent: []string{`"items":`, "build the thing", `"role":"engineer"`},
		NotExpectedContent: []string{
			"write the spec", // a PM task must be filtered out
		},
	}
	scenario.TestAppFactory = func(t testing.TB) *tests.TestApp {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatalf("NewTestApp: %v", err)
		}
		if err := EnsureSchema(app); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		SeedTeam(app)
		pm, _ := activeAgentByRole(app, RolePM)
		eng, _ := activeAgentByRole(app, RoleEngineer)
		if _, err := createTask(app, "write the spec", "", pm.Id, "", ""); err != nil {
			t.Fatalf("pm task: %v", err)
		}
		if _, err := createTask(app, "build the thing", "", eng.Id, "", ""); err != nil {
			t.Fatalf("eng task: %v", err)
		}
		return app
	}
	scenario.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		RegisterRoutes(e, app)
		scenario.URL = "/api/orchestrator/tasks?role=engineer"
		su, _ := app.FindFirstRecordByFilter(core.CollectionNameSuperusers, "1=1")
		token, _ := su.NewAuthToken()
		scenario.Headers = map[string]string{"Authorization": token}
	}
	scenario.Test(t)
}

// GET /tasks/{id} returns the draft, the ancestor lineage (oldest-first), and the
// task's runs.
func TestTaskDetailWithLineageAndRuns(t *testing.T) {
	scenario := tests.ApiScenario{
		Name:           "task detail with lineage + runs",
		Method:         http.MethodGet,
		ExpectedStatus: 200,
		ExpectedContent: []string{
			`"lineage":`, "Add OAuth", // the PM ancestor appears in lineage
			"## engineer draft",           // the child's own output
			`"runs":`, `"totalTokens":42`, // its logged run
		},
	}
	var childID string
	scenario.TestAppFactory = func(t testing.TB) *tests.TestApp {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatalf("NewTestApp: %v", err)
		}
		if err := EnsureSchema(app); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		SeedTeam(app)
		pm, _ := activeAgentByRole(app, RolePM)
		eng, _ := activeAgentByRole(app, RoleEngineer)
		parent, _ := createTask(app, "Add OAuth", "", pm.Id, "", "")
		parent.Set("state", StateDone)
		parent.Set("output", "## spec")
		_ = app.Save(parent)
		child, _ := createTask(app, "engineer: Add OAuth", "## spec", eng.Id, parent.Id, "")
		child.Set("state", StateNeedsReview)
		child.Set("output", "## engineer draft")
		_ = app.Save(child)
		// a logged run with a known token total
		col, _ := app.FindCollectionByNameOrId(runCollection)
		run := core.NewRecord(col)
		run.Set("task", child.Id)
		run.Set("agent", eng.Id)
		run.Set("status", "ok")
		run.Set("totalTokens", 42)
		_ = app.Save(run)
		childID = child.Id
		return app
	}
	scenario.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		RegisterRoutes(e, app)
		scenario.URL = "/api/orchestrator/tasks/" + childID
		su, _ := app.FindFirstRecordByFilter(core.CollectionNameSuperusers, "1=1")
		token, _ := su.NewAuthToken()
		scenario.Headers = map[string]string{"Authorization": token}
	}
	scenario.Test(t)
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
