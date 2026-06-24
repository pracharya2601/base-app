package support

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/ai"
	"base-app/internal/orchestrator"
)

// This file gives the support agent its abilities (goai auto tool loop, registered
// for KindSupportReply): a READ tool (lookup_orders) it runs freely mid-draft, and
// a WRITE tool (issue_refund) that does NOT mutate immediately — it PROPOSES the
// refund (orchestrator.ProposeAction) so the side effect only happens when a human
// approves the reply. The agent acts on live data; mutations stay behind the gate.

const ordersCollection = "orders"

// ActionIssueRefund is the proposed-action kind for a refund. The issue_refund
// tool proposes it; executeRefund (registered as its executor) runs it on approval.
const ActionIssueRefund = "issue_refund"

var orderStatuses = []string{"placed", "paid", "shipped", "delivered", "cancelled", "refunded"}

// ensureOrders creates a minimal orders collection so lookup_orders has something
// real to query. Normal (non-system) collection → auto-RBAC governs writes; the
// tool reads server-side via app (not rule-gated), which is what we want.
// Idempotent.
func ensureOrders(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(ordersCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(ordersCollection)
	c.Fields.Add(&core.TextField{Name: "orderNumber", Required: true})
	c.Fields.Add(&core.EmailField{Name: "customerEmail"})
	c.Fields.Add(&core.SelectField{Name: "status", Values: orderStatuses, MaxSelect: 1})
	c.Fields.Add(&core.NumberField{Name: "total"})
	c.Fields.Add(&core.TextField{Name: "items"})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	return app.Save(c)
}

// orderTools is the ToolProvider registered for support-reply tasks. It closes
// over the specific task so the write-tool can attach its proposal to it.
func orderTools(app core.App, task *core.Record) []ai.Tool {
	return []ai.Tool{
		ai.NewTool(
			"lookup_orders",
			"Look up a customer's orders by their email address. Returns each order's number, "+
				"status, and total. Call this before answering anything about order status, shipping, "+
				"delivery, or refunds, and base your reply on the real result.",
			func(ctx context.Context, in struct {
				CustomerEmail string `json:"customerEmail" jsonschema:"description=The customer's email address to look up orders for"`
			}) (string, error) {
				return lookupOrders(app, in.CustomerEmail), nil
			}),
		ai.NewTool(
			ActionIssueRefund,
			"Propose a refund for one of the customer's orders. This does NOT refund immediately — "+
				"it QUEUES the refund for human approval, which happens when this reply is approved. "+
				"Use it when a refund is warranted, then tell the customer their refund is being processed.",
			func(ctx context.Context, in struct {
				OrderNumber string `json:"orderNumber" jsonschema:"description=The order number to refund"`
				Reason      string `json:"reason" jsonschema:"description=Why the customer is owed a refund"`
			}) (string, error) {
				if task == nil {
					return "", fmt.Errorf("no task context to attach the refund proposal to")
				}
				order, err := app.FindFirstRecordByFilter(ordersCollection,
					"orderNumber = {:n}", dbx.Params{"n": in.OrderNumber})
				if err != nil {
					return fmt.Sprintf("No order %s exists, so no refund was queued. Ask the customer to confirm their order number.", in.OrderNumber), nil
				}
				if err := orchestrator.ProposeAction(app, task.Id, ActionIssueRefund, map[string]any{
					"orderNumber": in.OrderNumber,
					"reason":      in.Reason,
				}); err != nil {
					return "", err
				}
				return fmt.Sprintf("A refund of %.2f for order %s has been queued and will be applied once this reply is approved.",
					order.GetFloat("total"), in.OrderNumber), nil
			}),
	}
}

// executeRefund is the approval-time executor for a proposed refund: it performs
// the real mutation (mark the order refunded). Registered via
// orchestrator.RegisterActionExecutor; runs only after a human approves the task.
func executeRefund(app core.App, _ *core.Record, params json.RawMessage) (string, error) {
	var p struct {
		OrderNumber string `json:"orderNumber"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("bad refund params: %w", err)
	}
	order, err := app.FindFirstRecordByFilter(ordersCollection,
		"orderNumber = {:n}", dbx.Params{"n": p.OrderNumber})
	if err != nil {
		return "", fmt.Errorf("order %s not found", p.OrderNumber)
	}
	order.Set("status", "refunded")
	if err := app.Save(order); err != nil {
		return "", fmt.Errorf("failed to refund order %s: %w", p.OrderNumber, err)
	}
	return fmt.Sprintf("Order %s refunded (%.2f).", p.OrderNumber, order.GetFloat("total")), nil
}

// lookupOrders is the tool's implementation: a server-side query of the orders
// collection by email, formatted for the model. Never returns an error to the
// loop — a human-readable "not found"/"failed" string is more useful to the model.
func lookupOrders(app core.App, email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "No email was provided, so no orders could be looked up."
	}
	recs, err := app.FindRecordsByFilter(ordersCollection,
		"customerEmail = {:e}", "-created", 20, 0, dbx.Params{"e": email})
	if err != nil {
		return "The order lookup failed due to an internal error."
	}
	if len(recs) == 0 {
		return fmt.Sprintf("No orders found for %s.", email)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Orders for %s:\n", email)
	for _, r := range recs {
		fmt.Fprintf(&b, "- %s | status: %s | total: %.2f",
			r.GetString("orderNumber"), r.GetString("status"), r.GetFloat("total"))
		if items := r.GetString("items"); items != "" {
			fmt.Fprintf(&b, " | items: %s", items)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// seedDemoOrders inserts a couple of demo orders so the lookup tool is testable
// out of the box — but ONLY when SUPPORT_SEED_DEMO is set, so a real deployment's
// DB stays clean. Idempotent (no-op if any order already exists).
func seedDemoOrders(app core.App) {
	if os.Getenv("SUPPORT_SEED_DEMO") == "" {
		return
	}
	if n, _ := app.CountRecords(ordersCollection); n > 0 {
		return
	}
	col, err := app.FindCollectionByNameOrId(ordersCollection)
	if err != nil {
		return
	}
	for _, d := range []map[string]any{
		{"orderNumber": "A-1001", "customerEmail": "demo@example.com", "status": "shipped", "total": 49.99, "items": "1x Widget"},
		{"orderNumber": "A-1002", "customerEmail": "demo@example.com", "status": "delivered", "total": 19.50, "items": "2x Gadget"},
	} {
		rec := core.NewRecord(col)
		for k, v := range d {
			rec.Set(k, v)
		}
		if err := app.Save(rec); err != nil {
			app.Logger().Error("support: seed demo order failed", "err", err)
		}
	}
}
