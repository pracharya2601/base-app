// Package support is the first real "company function" the orchestrator runs:
// autonomous customer support. It is self-contained and plugs into the generic
// orchestrator engine through three seams — it does NOT modify the engine:
//
//   - a TRIGGER: a new support_tickets row auto-enqueues an orchestrator task
//     (work arrives on its own; the always-on loop drafts a reply).
//   - a STATE MIRROR: as the task moves pending→needs_review→done, the ticket's
//     status is kept in sync, so the ticket is the operator's view of progress.
//   - an APPROVE-ACTION: approving the drafted reply SENDS it (email if SMTP is
//     configured, otherwise recorded on the ticket) and resolves the ticket.
//
// This is the template every future function (orders, bookings, …) follows:
// define your domain collection(s), enrich+enqueue on a trigger, and register what
// "approve" does. The agent drafts; a human approves; the action makes it real.
package support

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/mailer"

	"base-app/internal/orchestrator"
)

const (
	ticketCollection = "support_tickets"

	// KindSupportReply tags the orchestrator task that drafts a reply to a ticket.
	// It selects this package's approve-action in the engine.
	KindSupportReply = "support_reply"

	// RoleSupport is the agent role that handles support tickets.
	RoleSupport = "support"
)

// Ticket statuses (mirrored from the handling task's state).
const (
	statusOpen        = "open"
	statusDrafting    = "drafting"
	statusAwaiting    = "awaiting_approval"
	statusResolved    = "resolved"
	statusClosed      = "closed"
	contextTicketsCap = 5 // how many prior tickets to feed the agent as context
)

var ticketStatuses = []string{statusOpen, statusDrafting, statusAwaiting, statusResolved, statusClosed}

const supportPersona = "You are a friendly, accurate customer support agent for this company. " +
	"Given a customer's message and any prior context about them, write a clear, empathetic reply that " +
	"directly resolves their issue or asks for the specific information you still need. Be concise and " +
	"professional. " +
	"You have a lookup_orders tool: call it with the customer's email to fetch their real orders BEFORE " +
	"answering anything about order status, shipping, delivery, or refunds, and base your reply on what it " +
	"returns. " +
	"When a refund is clearly warranted, use the issue_refund tool — it does NOT refund immediately, it " +
	"queues the refund for human approval, so tell the customer their refund is being processed. " +
	"Do NOT invent facts, order numbers, refunds, or policies you weren't given — if a tool returns nothing " +
	"or you still lack what you need, say what you can do and what you need from them. Your reply is a " +
	"DRAFT a human will approve before it is sent to the customer, so write it as the final message TO the " +
	"customer (no preamble, no notes to the reviewer, no subject line)."

// Register wires the support function into a running app during OnServe. It seeds
// the support agent, binds the trigger + state-mirror hooks, and registers the
// approve-action. Call AFTER orchestrator.EnsureSchema (it needs _agents/_tasks)
// and after support.EnsureSchema.
func Register(app core.App) {
	if err := orchestrator.SeedAgent(app, "Sam (Support)", RoleSupport, supportPersona, orchestrator.SystemOwner); err != nil {
		app.Logger().Error("support: seed agent failed", "err", err)
	}

	// TRIGGER: a new ticket → an orchestrator task that drafts a reply.
	app.OnRecordAfterCreateSuccess(ticketCollection).BindFunc(func(e *core.RecordEvent) error {
		enqueueForTicket(e.App, e.Record)
		return e.Next()
	})

	// STATE MIRROR: keep the ticket's status in step with its handling task.
	app.OnRecordAfterUpdateSuccess(orchestrator.TaskCollection).BindFunc(func(e *core.RecordEvent) error {
		syncTicketFromTask(e.App, e.Record)
		return e.Next()
	})

	// APPROVE-ACTION: approving the drafted reply sends it + resolves the ticket.
	orchestrator.RegisterApproveAction(KindSupportReply, sendReply)

	// TOOLS: let the support agent look up the customer's orders mid-draft (goai's
	// auto tool loop). The agent acts on real data instead of only describing.
	orchestrator.RegisterTaskTools(KindSupportReply, orderTools)
	// WRITE-TOOL EXECUTOR: the refund the agent PROPOSES only runs here, after a
	// human approves the reply (see support_orders.go).
	orchestrator.RegisterActionExecutor(ActionIssueRefund, executeRefund)
	seedDemoOrders(app) // no-op unless SUPPORT_SEED_DEMO is set

	app.Logger().Info("support: customer-support function active")
}

// EnsureSchema creates the support collections (support_tickets + orders) if
// missing. Idempotent and order-independent, so it's safe on existing installs.
func EnsureSchema(app core.App) error {
	if err := ensureTickets(app); err != nil {
		return err
	}
	return ensureOrders(app)
}

// ensureTickets creates the support_tickets collection if missing. Create is
// PUBLIC so a "contact us" form works out of the box — list/view/manage stay
// superuser/role-gated (the auto-RBAC backfill leaves a collection's rules alone
// once any rule is set non-nil). NOTE: public create means anonymous submissions
// can drive LLM spend; the orchestrator's ORCH_DAILY_TOKEN_BUDGET is the backstop.
// Tighten the create rule (e.g. require auth) if that's a concern.
func ensureTickets(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(ticketCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(ticketCollection)
	c.Fields.Add(&core.TextField{Name: "subject", Required: true})
	c.Fields.Add(&core.TextField{Name: "body", Required: true}) // the customer's message
	c.Fields.Add(&core.EmailField{Name: "customerEmail"})
	c.Fields.Add(&core.TextField{Name: "customerName"})
	c.Fields.Add(&core.SelectField{Name: "status", Values: ticketStatuses, MaxSelect: 1})
	c.Fields.Add(&core.TextField{Name: "resolution"}) // the reply that was sent, set on approve
	c.Fields.Add(&core.TextField{Name: "task"})       // id of the orchestrator task handling it
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	public := ""
	c.CreateRule = &public // public submission; other rules stay nil = superuser-only
	return app.Save(c)
}

// enqueueForTicket builds a context-rich task from a new ticket and queues it for
// the support agent. The agent's reply is grounded in the customer's prior tickets
// (gathered here and passed in the task body) — no live tool-calling needed yet.
func enqueueForTicket(app core.App, ticket *core.Record) {
	desc := buildTicketPrompt(app, ticket)
	task, err := orchestrator.EnqueueTask(app, orchestrator.EnqueueOpts{
		Title:       "Support: " + ticket.GetString("subject"),
		Description: desc,
		Kind:        KindSupportReply,
		Role:        RoleSupport,
		CreatedBy:   "support-trigger",
	})
	if err != nil {
		app.Logger().Error("support: failed to enqueue ticket task", "ticket", ticket.Id, "err", err)
		return
	}
	ticket.Set("task", task.Id)
	ticket.Set("status", statusDrafting)
	if err := app.Save(ticket); err != nil {
		app.Logger().Error("support: failed to link task to ticket", "ticket", ticket.Id, "err", err)
	}
}

// buildTicketPrompt renders the customer message + recent history into the task
// description the support agent works from.
func buildTicketPrompt(app core.App, ticket *core.Record) string {
	var b strings.Builder
	name := ticket.GetString("customerName")
	email := ticket.GetString("customerEmail")
	fmt.Fprintf(&b, "A customer has contacted support. Write a reply.\n\n")
	fmt.Fprintf(&b, "Customer: %s <%s>\n", orElse(name, "(unknown)"), orElse(email, "(no email)"))
	fmt.Fprintf(&b, "Subject: %s\n\n", ticket.GetString("subject"))
	fmt.Fprintf(&b, "Message:\n%s\n", ticket.GetString("body"))

	if hist := recentHistory(app, email, ticket.Id); hist != "" {
		b.WriteString("\n--- This customer's recent tickets (for context) ---\n")
		b.WriteString(hist)
	}
	return b.String()
}

// recentHistory summarizes the customer's prior tickets (by email), newest first,
// so the agent can be consistent and aware. Returns "" when there's no usable
// history (no email, or no prior tickets).
func recentHistory(app core.App, email, excludeID string) string {
	if email == "" {
		return ""
	}
	recs, err := app.FindRecordsByFilter(ticketCollection,
		"customerEmail = {:e} && id != {:id}", "-created", contextTicketsCap, 0,
		dbx.Params{"e": email, "id": excludeID})
	if err != nil || len(recs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range recs {
		fmt.Fprintf(&b, "- [%s] %s", orElse(r.GetString("status"), statusOpen), r.GetString("subject"))
		if res := r.GetString("resolution"); res != "" {
			fmt.Fprintf(&b, " → %s", truncate(res, 200))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// statusForTaskState maps an orchestrator task state to the ticket status the
// operator should see. The second result is false when the state has no ticket
// meaning (so we don't touch the ticket).
func statusForTaskState(state string) (string, bool) {
	switch state {
	case orchestrator.StatePending, orchestrator.StateWorking:
		return statusDrafting, true
	case orchestrator.StateNeedsReview:
		return statusAwaiting, true
	case orchestrator.StateDone:
		return statusResolved, true
	case orchestrator.StateRejected:
		return statusClosed, true
	case orchestrator.StateFailed:
		return statusOpen, true // surfaced as open again so it can be retried
	}
	return "", false
}

// syncTicketFromTask mirrors a support task's state onto its ticket. Bound to
// _tasks updates; a no-op for non-support tasks and when the status is unchanged.
func syncTicketFromTask(app core.App, task *core.Record) {
	if task.GetString("kind") != KindSupportReply {
		return
	}
	want, ok := statusForTaskState(task.GetString("state"))
	if !ok {
		return
	}
	ticket := findTicketByTask(app, task.Id)
	if ticket == nil || ticket.GetString("status") == want {
		return
	}
	// Don't downgrade a resolved ticket (the approve-action already set it).
	if ticket.GetString("status") == statusResolved && want != statusResolved {
		return
	}
	ticket.Set("status", want)
	if err := app.Save(ticket); err != nil {
		app.Logger().Error("support: failed to mirror ticket status", "ticket", ticket.Id, "err", err)
	}
}

// sendReply is the approve-action for a support reply: the operator approved the
// draft, so make it real — email it (if SMTP is configured) and resolve the
// ticket. Recorded on the ticket either way so it's never lost.
func sendReply(app core.App, task *core.Record) error {
	ticket := findTicketByTask(app, task.Id)
	if ticket == nil {
		return fmt.Errorf("no support ticket linked to task %s", task.Id)
	}
	reply := strings.TrimSpace(task.GetString("output"))
	if reply == "" {
		return fmt.Errorf("task %s has no drafted reply to send", task.Id)
	}

	if to := ticket.GetString("customerEmail"); to != "" && app.Settings().SMTP.Enabled {
		from := app.Settings().Meta.SenderAddress
		msg := &mailer.Message{
			From:    mail.Address{Name: app.Settings().Meta.SenderName, Address: from},
			To:      []mail.Address{{Address: to, Name: ticket.GetString("customerName")}},
			Subject: "Re: " + ticket.GetString("subject"),
			Text:    reply,
		}
		if err := app.NewMailClient().Send(msg); err != nil {
			return fmt.Errorf("failed to send support email: %w", err)
		}
		app.Logger().Info("support: reply emailed", "ticket", ticket.Id, "to", to)
	} else {
		app.Logger().Info("support: reply recorded (no email sent — SMTP off or no address)", "ticket", ticket.Id)
	}

	ticket.Set("resolution", reply)
	ticket.Set("status", statusResolved)
	return app.Save(ticket)
}

// findTicketByTask returns the ticket whose `task` link points at taskID, or nil.
func findTicketByTask(app core.App, taskID string) *core.Record {
	r, err := app.FindFirstRecordByFilter(ticketCollection, "task = {:t}", dbx.Params{"t": taskID})
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
