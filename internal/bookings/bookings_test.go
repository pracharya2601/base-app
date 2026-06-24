package bookings

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
	if err := EnsureSchema(app); err != nil {
		t.Fatalf("bookings.EnsureSchema: %v", err)
	}
	Register(app)
	return app
}

func newRequest(t *testing.T, app core.App, service, date, email string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(requestCollection)
	if err != nil {
		t.Fatalf("request collection: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("service", service)
	rec.Set("requestedDate", date)
	rec.Set("customerEmail", email)
	rec.Set("customerName", "Cust")
	if err := app.Save(rec); err != nil { // fires the trigger
		t.Fatalf("save request: %v", err)
	}
	return rec
}

func reqTask(t *testing.T, app core.App) *core.Record {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter(orchestrator.TaskCollection,
		"kind = {:k}", dbx.Params{"k": KindBookingReply})
	if err != nil {
		t.Fatalf("no booking task: %v", err)
	}
	return rec
}

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

// Schema + a scheduler agent seeded; tools + executor registered.
func TestSetupWiring(t *testing.T) {
	app := setup(t)
	for _, name := range []string{requestCollection, bookingCollection} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %s missing: %v", name, err)
		}
	}
	n, _ := app.CountRecords(orchestrator.AgentCollection, dbx.NewExp("role = {:r}", dbx.Params{"r": RoleScheduler}))
	if n != 1 {
		t.Errorf("scheduler agents = %d, want 1", n)
	}
	if !orchestrator.HasTaskTools(KindBookingReply) {
		t.Error("no tools registered for booking_reply")
	}
}

// The TRIGGER: a new request enqueues a scheduler task and flips the request to
// drafting, linked to the task.
func TestRequestTriggerEnqueues(t *testing.T) {
	app := setup(t)
	req := newRequest(t, app, "Haircut", "2026-07-01", "cust@example.com")
	task := reqTask(t, app)
	if task.GetString("state") != orchestrator.StatePending {
		t.Errorf("task state = %q, want pending", task.GetString("state"))
	}
	req, _ = app.FindRecordById(requestCollection, req.Id)
	if req.GetString("task") != task.Id || req.GetString("status") != statusDrafting {
		t.Errorf("request not linked/drafting: task=%q status=%q", req.GetString("task"), req.GetString("status"))
	}
}

// check_availability reflects existing bookings against capacity.
func TestAvailability(t *testing.T) {
	app := setup(t)
	if got := availabilityFor(app, "2026-07-01"); !strings.Contains(got, "5 of 5") {
		t.Errorf("empty date should show full availability, got: %s", got)
	}
	// Fill the date to capacity.
	col, _ := app.FindCollectionByNameOrId(bookingCollection)
	for i := 0; i < dailyCapacity; i++ {
		r := core.NewRecord(col)
		r.Set("service", "X")
		r.Set("date", "2026-07-01")
		r.Set("status", "confirmed")
		if err := app.Save(r); err != nil {
			t.Fatalf("seed booking: %v", err)
		}
	}
	if got := availabilityFor(app, "2026-07-01"); !strings.Contains(got, "fully booked") {
		t.Errorf("full date should show fully booked, got: %s", got)
	}
}

// THE GUARD: confirm_booking must PROPOSE (recorded, pending) and NOT create a
// booking. The executor creates it only on approval.
func TestBookingIsProposedNotCreated(t *testing.T) {
	app := setup(t)
	newRequest(t, app, "Massage", "2026-08-15", "zoe@example.com")
	task := reqTask(t, app)

	confirm := findTool(t, bookingTools(app, task), ActionConfirmBooking)
	msg, err := confirm.Execute(context.Background(), json.RawMessage(`{"service":"Massage","date":"2026-08-15"}`))
	if err != nil {
		t.Fatalf("confirm_booking execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "queued") {
		t.Errorf("expected a queued message, got: %s", msg)
	}
	// Guard: no booking created yet.
	if n, _ := app.CountRecords(bookingCollection, dbx.NewExp("date = {:d}", dbx.Params{"d": "2026-08-15"})); n != 0 {
		t.Fatalf("a booking was created at propose time — guard failed (n=%d)", n)
	}
	// A proposal exists.
	pa, err := app.FindFirstRecordByFilter("_proposedActions",
		"task = {:t} && actionKind = {:k}", dbx.Params{"t": task.Id, "k": ActionConfirmBooking})
	if err != nil || pa.GetString("status") != "proposed" {
		t.Fatalf("expected a proposed booking row, got %v (err %v)", pa, err)
	}

	// The executor (on approval) creates the real booking.
	if _, err := executeConfirmBooking(app, task, json.RawMessage(`{"service":"Massage","date":"2026-08-15","customerEmail":"zoe@example.com"}`)); err != nil {
		t.Fatalf("executeConfirmBooking: %v", err)
	}
	if n, _ := app.CountRecords(bookingCollection, dbx.NewExp("date = {:d}", dbx.Params{"d": "2026-08-15"})); n != 1 {
		t.Errorf("after executor, bookings on date = %d, want 1", n)
	}
}

// confirm_booking on a full date proposes nothing.
func TestBookingFullDateProposesNothing(t *testing.T) {
	app := setup(t)
	newRequest(t, app, "Facial", "2026-09-09", "amy@example.com")
	task := reqTask(t, app)
	col, _ := app.FindCollectionByNameOrId(bookingCollection)
	for i := 0; i < dailyCapacity; i++ {
		r := core.NewRecord(col)
		r.Set("service", "X")
		r.Set("date", "2026-09-09")
		r.Set("status", "confirmed")
		_ = app.Save(r)
	}
	confirm := findTool(t, bookingTools(app, task), ActionConfirmBooking)
	msg, _ := confirm.Execute(context.Background(), json.RawMessage(`{"service":"Facial","date":"2026-09-09"}`))
	if !strings.Contains(msg, "fully booked") {
		t.Errorf("expected fully-booked message, got: %s", msg)
	}
	n, _ := app.CountRecords("_proposedActions", dbx.NewExp("task = {:t}", dbx.Params{"t": task.Id}))
	if n != 0 {
		t.Errorf("no proposal should exist for a full date, got %d", n)
	}
}
