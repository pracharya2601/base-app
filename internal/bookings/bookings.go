// Package bookings is the SECOND "company function" on the orchestrator engine,
// built to prove the template generalizes. It adds an appointment/booking desk
// using ONLY the engine's exported seams — EnqueueTask, RegisterApproveAction,
// RegisterTaskTools, RegisterActionExecutor, ProposeAction, SeedAgent — with ZERO
// changes to internal/orchestrator. A new function = a new package + one line in
// main.go. It mirrors internal/support's shape on a different domain, and its
// write-tool CREATES a record (a booking) rather than updating one.
package bookings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/mailer"

	"base-app/internal/ai"
	"base-app/internal/orchestrator"
)

const (
	requestCollection = "booking_requests"
	bookingCollection = "bookings"

	// KindBookingReply tags the orchestrator task that drafts a reply to a request.
	KindBookingReply = "booking_reply"
	// RoleScheduler is the agent role that handles booking requests.
	RoleScheduler = "scheduler"
	// ActionConfirmBooking is the proposed-action kind for creating a booking.
	ActionConfirmBooking = "confirm_booking"

	// dailyCapacity is how many bookings a single date can hold (kept simple — a
	// real deployment would make this configurable per service/resource).
	dailyCapacity = 5
)

// Request statuses (mirrored from the handling task's state).
const (
	statusRequested = "requested"
	statusDrafting  = "drafting"
	statusAwaiting  = "awaiting_approval"
	statusConfirmed = "confirmed"
	statusDeclined  = "declined"
)

var requestStatuses = []string{statusRequested, statusDrafting, statusAwaiting, statusConfirmed, statusDeclined}
var bookingStatuses = []string{"confirmed", "cancelled"}

const schedulerPersona = "You are a polite, efficient scheduling assistant for this company. A customer has " +
	"requested a booking. ALWAYS use the check_availability tool for the requested date before committing to " +
	"anything. If a slot is open, use the confirm_booking tool — it does NOT book immediately, it QUEUES the " +
	"booking for human approval — then tell the customer their booking is being confirmed. If the date is full, " +
	"apologize and offer to find another date. Do not invent availability, prices, or policies. Your reply is a " +
	"DRAFT a human approves before it is sent, so write it as the final message TO the customer (no preamble, no " +
	"notes to the reviewer, no subject line)."

// Register wires the bookings function into a running app during OnServe. Call
// AFTER orchestrator.EnsureSchema and bookings.EnsureSchema.
func Register(app core.App) {
	if err := orchestrator.SeedAgent(app, "Bea (Scheduler)", RoleScheduler, schedulerPersona, orchestrator.SystemOwner); err != nil {
		app.Logger().Error("bookings: seed agent failed", "err", err)
	}

	// TRIGGER: a new request → an orchestrator task that drafts a reply.
	app.OnRecordAfterCreateSuccess(requestCollection).BindFunc(func(e *core.RecordEvent) error {
		enqueueForRequest(e.App, e.Record)
		return e.Next()
	})

	// STATE MIRROR: keep the request's status in step with its handling task.
	app.OnRecordAfterUpdateSuccess(orchestrator.TaskCollection).BindFunc(func(e *core.RecordEvent) error {
		syncRequestFromTask(e.App, e.Record)
		return e.Next()
	})

	// APPROVE-ACTION + WRITE-TOOL EXECUTOR: approving sends the reply; the proposed
	// booking (if any) is created by the executor first, on approval only.
	orchestrator.RegisterApproveAction(KindBookingReply, sendConfirmation)
	orchestrator.RegisterTaskTools(KindBookingReply, bookingTools)
	orchestrator.RegisterActionExecutor(ActionConfirmBooking, executeConfirmBooking)

	app.Logger().Info("bookings: appointment-desk function active")
}

// EnsureSchema creates the booking_requests + bookings collections if missing.
// Idempotent and order-independent.
func EnsureSchema(app core.App) error {
	if err := ensureRequests(app); err != nil {
		return err
	}
	return ensureBookings(app)
}

// ensureRequests creates booking_requests (public create, like a booking form).
func ensureRequests(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(requestCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(requestCollection)
	c.Fields.Add(&core.TextField{Name: "customerName"})
	c.Fields.Add(&core.EmailField{Name: "customerEmail"})
	c.Fields.Add(&core.TextField{Name: "service", Required: true})
	c.Fields.Add(&core.TextField{Name: "requestedDate", Required: true}) // ISO date string, kept simple
	c.Fields.Add(&core.TextField{Name: "notes"})
	c.Fields.Add(&core.SelectField{Name: "status", Values: requestStatuses, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "reply"}) // the message that was sent, set on approve
	c.Fields.Add(&core.TextField{Name: "task"})  // id of the orchestrator task handling it
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	public := ""
	c.CreateRule = &public // public submission; other rules stay nil = superuser-only
	return app.Save(c)
}

// ensureBookings creates the bookings collection — the confirmed outcomes the
// write-tool executor creates.
func ensureBookings(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(bookingCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(bookingCollection)
	c.Fields.Add(&core.TextField{Name: "customerName"})
	c.Fields.Add(&core.EmailField{Name: "customerEmail"})
	c.Fields.Add(&core.TextField{Name: "service", Required: true})
	c.Fields.Add(&core.TextField{Name: "date", Required: true})
	c.Fields.Add(&core.SelectField{Name: "status", Values: bookingStatuses, MaxSelect: 1})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	return app.Save(c)
}

// enqueueForRequest builds a task from a new booking request and queues it for the
// scheduler agent.
func enqueueForRequest(app core.App, req *core.Record) {
	task, err := orchestrator.EnqueueTask(app, orchestrator.EnqueueOpts{
		Title:       "Booking: " + req.GetString("service") + " on " + req.GetString("requestedDate"),
		Description: buildRequestPrompt(app, req),
		Kind:        KindBookingReply,
		Role:        RoleScheduler,
		CreatedBy:   "bookings-trigger",
	})
	if err != nil {
		app.Logger().Error("bookings: failed to enqueue request task", "request", req.Id, "err", err)
		return
	}
	req.Set("task", task.Id)
	req.Set("status", statusDrafting)
	if err := app.Save(req); err != nil {
		app.Logger().Error("bookings: failed to link task to request", "request", req.Id, "err", err)
	}
}

func buildRequestPrompt(app core.App, req *core.Record) string {
	var b strings.Builder
	date := req.GetString("requestedDate")
	fmt.Fprintf(&b, "A customer has requested a booking. Write a reply.\n\n")
	fmt.Fprintf(&b, "Customer: %s <%s>\n", orElse(req.GetString("customerName"), "(unknown)"), orElse(req.GetString("customerEmail"), "(no email)"))
	fmt.Fprintf(&b, "Service: %s\n", req.GetString("service"))
	fmt.Fprintf(&b, "Requested date: %s\n", date)
	if notes := req.GetString("notes"); notes != "" {
		fmt.Fprintf(&b, "Notes: %s\n", notes)
	}
	fmt.Fprintf(&b, "\nAvailability for %s: %s\n", date, availabilityFor(app, date))
	return b.String()
}

// ---- tools -----------------------------------------------------------------

// bookingTools is the ToolProvider for booking-reply tasks: a READ tool
// (check_availability) and a WRITE tool (confirm_booking) that proposes a booking.
func bookingTools(app core.App, task *core.Record) []ai.Tool {
	return []ai.Tool{
		ai.NewTool(
			"check_availability",
			"Check how many booking slots remain on a given date (YYYY-MM-DD). Call this before confirming.",
			func(ctx context.Context, in struct {
				Date string `json:"date" jsonschema:"description=The date to check, YYYY-MM-DD"`
			}) (string, error) {
				return availabilityFor(app, in.Date), nil
			}),
		ai.NewTool(
			ActionConfirmBooking,
			"Propose confirming a booking for the customer on a date. This does NOT book immediately — it "+
				"QUEUES the booking for human approval (created only when this reply is approved). Only use it "+
				"when check_availability shows an open slot.",
			func(ctx context.Context, in struct {
				Service string `json:"service" jsonschema:"description=The service being booked"`
				Date    string `json:"date" jsonschema:"description=The booking date, YYYY-MM-DD"`
			}) (string, error) {
				if task == nil {
					return "", fmt.Errorf("no task context to attach the booking proposal to")
				}
				if remaining(app, in.Date) <= 0 {
					return fmt.Sprintf("%s is fully booked, so no booking was queued. Offer the customer another date.", in.Date), nil
				}
				req := findRequestByTask(app, task.Id)
				if req == nil {
					return "", fmt.Errorf("no booking request linked to this task")
				}
				if err := orchestrator.ProposeAction(app, task.Id, ActionConfirmBooking, map[string]any{
					"service":       in.Service,
					"date":          in.Date,
					"customerEmail": req.GetString("customerEmail"),
					"customerName":  req.GetString("customerName"),
				}); err != nil {
					return "", err
				}
				return fmt.Sprintf("A booking for %s on %s has been queued and will be confirmed once this reply is approved.", in.Service, in.Date), nil
			}),
	}
}

// executeConfirmBooking is the approval-time executor: it CREATES the booking
// record (the real side effect). Re-checks capacity to avoid overbooking at
// execution time. Runs only after a human approves.
func executeConfirmBooking(app core.App, _ *core.Record, params json.RawMessage) (string, error) {
	var p struct {
		Service       string `json:"service"`
		Date          string `json:"date"`
		CustomerEmail string `json:"customerEmail"`
		CustomerName  string `json:"customerName"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("bad booking params: %w", err)
	}
	if remaining(app, p.Date) <= 0 {
		return "", fmt.Errorf("%s is now fully booked", p.Date)
	}
	col, err := app.FindCollectionByNameOrId(bookingCollection)
	if err != nil {
		return "", err
	}
	rec := core.NewRecord(col)
	rec.Set("customerName", p.CustomerName)
	rec.Set("customerEmail", p.CustomerEmail)
	rec.Set("service", p.Service)
	rec.Set("date", p.Date)
	rec.Set("status", "confirmed")
	if err := app.Save(rec); err != nil {
		return "", fmt.Errorf("failed to create booking: %w", err)
	}
	return fmt.Sprintf("Booked %s on %s.", p.Service, p.Date), nil
}

// ---- approve-action + state mirror ----------------------------------------

// sendConfirmation is the approve-action: send/record the reply and mark the
// request confirmed. The proposed booking (if any) was already created by the
// executor, which the engine runs before this.
func sendConfirmation(app core.App, task *core.Record) error {
	req := findRequestByTask(app, task.Id)
	if req == nil {
		return fmt.Errorf("no booking request linked to task %s", task.Id)
	}
	reply := strings.TrimSpace(task.GetString("output"))
	if reply == "" {
		return fmt.Errorf("task %s has no drafted reply to send", task.Id)
	}
	if to := req.GetString("customerEmail"); to != "" && app.Settings().SMTP.Enabled {
		msg := &mailer.Message{
			From:    mail.Address{Name: app.Settings().Meta.SenderName, Address: app.Settings().Meta.SenderAddress},
			To:      []mail.Address{{Address: to, Name: req.GetString("customerName")}},
			Subject: "Your booking: " + req.GetString("service"),
			Text:    reply,
		}
		if err := app.NewMailClient().Send(msg); err != nil {
			return fmt.Errorf("failed to send booking email: %w", err)
		}
		app.Logger().Info("bookings: confirmation emailed", "request", req.Id, "to", to)
	} else {
		app.Logger().Info("bookings: confirmation recorded (no email sent — SMTP off or no address)", "request", req.Id)
	}
	req.Set("reply", reply)
	req.Set("status", statusConfirmed)
	return app.Save(req)
}

func statusForTaskState(state string) (string, bool) {
	switch state {
	case orchestrator.StatePending, orchestrator.StateWorking:
		return statusDrafting, true
	case orchestrator.StateNeedsReview:
		return statusAwaiting, true
	case orchestrator.StateDone:
		return statusConfirmed, true
	case orchestrator.StateRejected:
		return statusDeclined, true
	case orchestrator.StateFailed:
		return statusRequested, true
	}
	return "", false
}

func syncRequestFromTask(app core.App, task *core.Record) {
	if task.GetString("kind") != KindBookingReply {
		return
	}
	want, ok := statusForTaskState(task.GetString("state"))
	if !ok {
		return
	}
	req := findRequestByTask(app, task.Id)
	if req == nil || req.GetString("status") == want {
		return
	}
	if req.GetString("status") == statusConfirmed && want != statusConfirmed {
		return // don't downgrade a confirmed request
	}
	req.Set("status", want)
	if err := app.Save(req); err != nil {
		app.Logger().Error("bookings: failed to mirror request status", "request", req.Id, "err", err)
	}
}

// ---- helpers ---------------------------------------------------------------

// remaining is how many slots are left on a date.
func remaining(app core.App, date string) int {
	n, err := app.CountRecords(bookingCollection,
		dbx.NewExp("date = {:d} AND status = {:s}", dbx.Params{"d": date, "s": "confirmed"}))
	if err != nil {
		return 0
	}
	r := dailyCapacity - int(n)
	if r < 0 {
		r = 0
	}
	return r
}

// availabilityFor renders a human/model-readable availability summary for a date.
func availabilityFor(app core.App, date string) string {
	if strings.TrimSpace(date) == "" {
		return "no date given"
	}
	r := remaining(app, date)
	if r <= 0 {
		return fmt.Sprintf("fully booked (%d/%d)", dailyCapacity, dailyCapacity)
	}
	return fmt.Sprintf("%d of %d slots open", r, dailyCapacity)
}

func findRequestByTask(app core.App, taskID string) *core.Record {
	r, err := app.FindFirstRecordByFilter(requestCollection, "task = {:t}", dbx.Params{"t": taskID})
	if err != nil {
		return nil
	}
	return r
}

func orElse(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
