package support

import (
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"base-app/internal/orchestrator"
)

// setup gives a test app with the orchestrator schema + the support function wired
// (agent seeded, hooks bound, approve-action registered).
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
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("support.EnsureSchema: %v", err)
	}
	Register(app)
	return app
}

func newTicket(t *testing.T, app core.App, subject, body, email, name string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(ticketCollection)
	if err != nil {
		t.Fatalf("ticket collection: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("subject", subject)
	rec.Set("body", body)
	rec.Set("customerEmail", email)
	rec.Set("customerName", name)
	if err := app.Save(rec); err != nil { // fires the create trigger
		t.Fatalf("save ticket: %v", err)
	}
	return rec
}

func taskForKind(t *testing.T, app core.App, kind string) *core.Record {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter(orchestrator.TaskCollection,
		"kind = {:k}", dbx.Params{"k": kind})
	if err != nil {
		t.Fatalf("no task for kind %q: %v", kind, err)
	}
	return rec
}

// EnsureSchema creates a public-create support_tickets collection. Idempotent.
func TestEnsureSchema(t *testing.T) {
	app := setup(t)
	c, err := app.FindCollectionByNameOrId(ticketCollection)
	if err != nil {
		t.Fatalf("support_tickets missing: %v", err)
	}
	for _, f := range []string{"subject", "body", "customerEmail", "status", "resolution", "task"} {
		if c.Fields.GetByName(f) == nil {
			t.Errorf("support_tickets missing field %q", f)
		}
	}
	if c.CreateRule == nil || *c.CreateRule != "" {
		t.Errorf("create rule should be public (\"\"), got %v", c.CreateRule)
	}
	if c.ListRule != nil {
		t.Errorf("list rule should stay superuser-only (nil), got %v", *c.ListRule)
	}
	if err := EnsureSchema(app); err != nil { // idempotent
		t.Fatalf("EnsureSchema second pass: %v", err)
	}
}

// Register must seed exactly one active support agent (idempotently) and register
// the approve-action for the support-reply kind.
func TestRegisterSeedsAgentAndAction(t *testing.T) {
	app := setup(t)
	n, _ := app.CountRecords(orchestrator.AgentCollection,
		dbx.NewExp("role = {:r}", dbx.Params{"r": RoleSupport}))
	if n != 1 {
		t.Fatalf("support agents = %d, want 1", n)
	}
	Register(app) // second wiring must not duplicate the agent
	n2, _ := app.CountRecords(orchestrator.AgentCollection,
		dbx.NewExp("role = {:r}", dbx.Params{"r": RoleSupport}))
	if n2 != 1 {
		t.Errorf("support agents after re-register = %d, want 1", n2)
	}
	if !orchestrator.HasApproveAction(KindSupportReply) {
		t.Error("approve-action not registered for support_reply")
	}
}

// The TRIGGER: creating a ticket enqueues a support task for the support agent and
// flips the ticket into the drafting state, linked to the task.
func TestTicketTriggerEnqueuesTask(t *testing.T) {
	app := setup(t)
	ticket := newTicket(t, app, "Where is my order?", "It's been two weeks.", "alice@example.com", "Alice")

	task := taskForKind(t, app, KindSupportReply)
	if task.GetString("state") != orchestrator.StatePending {
		t.Errorf("task state = %q, want pending", task.GetString("state"))
	}
	agent, err := app.FindRecordById(orchestrator.AgentCollection, task.GetString("agent"))
	if err != nil || agent.GetString("role") != RoleSupport {
		t.Errorf("task not assigned to the support agent")
	}
	if task.GetString("description") == "" {
		t.Error("task description (the prompt) should be populated from the ticket")
	}

	// The ticket should now point at the task and be in the drafting state.
	ticket, _ = app.FindRecordById(ticketCollection, ticket.Id)
	if ticket.GetString("task") != task.Id {
		t.Errorf("ticket.task = %q, want %q", ticket.GetString("task"), task.Id)
	}
	if ticket.GetString("status") != statusDrafting {
		t.Errorf("ticket status = %q, want drafting", ticket.GetString("status"))
	}
}

// The STATE MIRROR: when the task reaches needs_review, the ticket should reflect
// awaiting_approval.
func TestTaskStateMirrorsToTicket(t *testing.T) {
	app := setup(t)
	ticket := newTicket(t, app, "Reset my password", "I'm locked out.", "bob@example.com", "Bob")
	task := taskForKind(t, app, KindSupportReply)

	// Simulate the agent producing a draft (no live LLM in tests).
	task.Set("output", "Hi Bob, here's how to reset your password…")
	task.Set("state", orchestrator.StateNeedsReview)
	if err := app.Save(task); err != nil { // fires the mirror hook
		t.Fatalf("save task: %v", err)
	}

	ticket, _ = app.FindRecordById(ticketCollection, ticket.Id)
	if ticket.GetString("status") != statusAwaiting {
		t.Errorf("ticket status = %q, want awaiting_approval", ticket.GetString("status"))
	}
}

// The APPROVE-ACTION: sending the reply records it on the ticket and resolves it
// (no SMTP configured in tests, so it's recorded rather than emailed).
func TestSendReplyResolvesTicket(t *testing.T) {
	app := setup(t)
	ticket := newTicket(t, app, "Refund request", "Please refund order #42.", "carol@example.com", "Carol")
	task := taskForKind(t, app, KindSupportReply)

	const reply = "Hi Carol, your refund for order #42 has been started."
	task.Set("output", reply)
	task.Set("state", orchestrator.StateNeedsReview)
	if err := app.Save(task); err != nil {
		t.Fatalf("save task: %v", err)
	}

	if err := sendReply(app, task); err != nil {
		t.Fatalf("sendReply: %v", err)
	}

	ticket, _ = app.FindRecordById(ticketCollection, ticket.Id)
	if ticket.GetString("status") != statusResolved {
		t.Errorf("ticket status = %q, want resolved", ticket.GetString("status"))
	}
	if ticket.GetString("resolution") != reply {
		t.Errorf("ticket resolution = %q, want the drafted reply", ticket.GetString("resolution"))
	}
}

// sendReply must fail loudly (leaving the task for retry) when there's no draft.
func TestSendReplyWithoutDraftErrors(t *testing.T) {
	app := setup(t)
	newTicket(t, app, "Empty", "x", "dave@example.com", "Dave")
	task := taskForKind(t, app, KindSupportReply)
	if err := sendReply(app, task); err == nil {
		t.Error("sendReply should error when the task has no drafted output")
	}
}
