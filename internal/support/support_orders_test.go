package support

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/ai"
	"base-app/internal/orchestrator"
)

func newOrder(t *testing.T, app core.App, num, email, status string, total float64) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(ordersCollection)
	if err != nil {
		t.Fatalf("orders collection: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("orderNumber", num)
	rec.Set("customerEmail", email)
	rec.Set("status", status)
	rec.Set("total", total)
	if err := app.Save(rec); err != nil {
		t.Fatalf("save order: %v", err)
	}
}

func TestEnsureOrdersCollection(t *testing.T) {
	app := setup(t)
	c, err := app.FindCollectionByNameOrId(ordersCollection)
	if err != nil {
		t.Fatalf("orders collection missing: %v", err)
	}
	for _, f := range []string{"orderNumber", "customerEmail", "status", "total"} {
		if c.Fields.GetByName(f) == nil {
			t.Errorf("orders missing field %q", f)
		}
	}
}

func TestLookupOrders(t *testing.T) {
	app := setup(t)
	newOrder(t, app, "A-1001", "alice@example.com", "shipped", 49.99)
	newOrder(t, app, "A-1002", "alice@example.com", "delivered", 19.50)
	newOrder(t, app, "B-2001", "bob@example.com", "placed", 5.00)

	out := lookupOrders(app, "alice@example.com")
	if !strings.Contains(out, "A-1001") || !strings.Contains(out, "A-1002") {
		t.Errorf("expected both of Alice's orders, got: %s", out)
	}
	if strings.Contains(out, "B-2001") {
		t.Errorf("Bob's order leaked into Alice's lookup: %s", out)
	}
	if !strings.Contains(out, "shipped") {
		t.Errorf("expected order status in output, got: %s", out)
	}

	if got := lookupOrders(app, "nobody@example.com"); !strings.Contains(got, "No orders found") {
		t.Errorf("expected not-found message, got: %s", got)
	}
	if got := lookupOrders(app, ""); !strings.Contains(got, "No email") {
		t.Errorf("expected no-email message, got: %s", got)
	}
}

// The tool is registered for support-reply tasks and its Execute path unmarshals
// the model's JSON args and returns the order data.
func TestOrderToolRegisteredAndExecutes(t *testing.T) {
	app := setup(t)
	if !orchestrator.HasTaskTools(KindSupportReply) {
		t.Fatal("no tools registered for support_reply")
	}
	newOrder(t, app, "A-1001", "carol@example.com", "refunded", 30.00)

	lookup := findTool(t, orderTools(app, nil), "lookup_orders")
	out, err := lookup.Execute(context.Background(), json.RawMessage(`{"customerEmail":"carol@example.com"}`))
	if err != nil {
		t.Fatalf("tool execute: %v", err)
	}
	if !strings.Contains(out, "A-1001") || !strings.Contains(out, "refunded") {
		t.Errorf("tool did not return the order, got: %s", out)
	}
}

// findTool returns the named tool from a provider's set.
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

func orderStatus(t *testing.T, app core.App, num string) string {
	t.Helper()
	r, err := app.FindFirstRecordByFilter(ordersCollection, "orderNumber = {:n}", dbx.Params{"n": num})
	if err != nil {
		t.Fatalf("order %s: %v", num, err)
	}
	return r.GetString("status")
}

// THE GUARD: calling issue_refund mid-draft must PROPOSE the refund (recorded,
// pending) and must NOT mutate the order. The executor only mutates on approval.
func TestRefundIsProposedNotExecuted(t *testing.T) {
	app := setup(t)
	newOrder(t, app, "A-9001", "dora@example.com", "delivered", 75.00)

	// A real task to attach the proposal to.
	ticket := newTicket(t, app, "Refund please", "It arrived broken.", "dora@example.com", "Dora")
	task := taskForKind(t, app, KindSupportReply)
	_ = ticket

	refund := findTool(t, orderTools(app, task), ActionIssueRefund)
	msg, err := refund.Execute(context.Background(), json.RawMessage(`{"orderNumber":"A-9001","reason":"arrived broken"}`))
	if err != nil {
		t.Fatalf("issue_refund execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "queued") {
		t.Errorf("expected a 'queued for approval' message, got: %s", msg)
	}
	// Guard: order is NOT refunded yet.
	if s := orderStatus(t, app, "A-9001"); s == "refunded" {
		t.Fatalf("order was refunded at propose time — the guard failed (status=%s)", s)
	}
	// A proposal was recorded for the task.
	if !orchestrator.HasTaskTools(KindSupportReply) {
		t.Error("sanity: support_reply should have tools")
	}
	pa, err := app.FindFirstRecordByFilter("_proposedActions",
		"task = {:t} && actionKind = {:k}", dbx.Params{"t": task.Id, "k": ActionIssueRefund})
	if err != nil || pa.GetString("status") != "proposed" {
		t.Fatalf("expected a proposed refund row, got %v (err %v)", pa, err)
	}

	// The executor (runs only on approval) performs the real mutation.
	if _, err := executeRefund(app, task, json.RawMessage(`{"orderNumber":"A-9001","reason":"arrived broken"}`)); err != nil {
		t.Fatalf("executeRefund: %v", err)
	}
	if s := orderStatus(t, app, "A-9001"); s != "refunded" {
		t.Errorf("after executor, order status = %q, want refunded", s)
	}
}

// issue_refund on a non-existent order proposes nothing and returns a soft message.
func TestRefundUnknownOrderProposesNothing(t *testing.T) {
	app := setup(t)
	ticket := newTicket(t, app, "Refund", "x", "ed@example.com", "Ed")
	_ = ticket
	task := taskForKind(t, app, KindSupportReply)

	refund := findTool(t, orderTools(app, task), ActionIssueRefund)
	msg, err := refund.Execute(context.Background(), json.RawMessage(`{"orderNumber":"ZZZ","reason":"x"}`))
	if err != nil {
		t.Fatalf("issue_refund execute: %v", err)
	}
	if !strings.Contains(msg, "No order") {
		t.Errorf("expected a no-order message, got: %s", msg)
	}
	n, _ := app.CountRecords("_proposedActions", dbx.NewExp("task = {:t}", dbx.Params{"t": task.Id}))
	if n != 0 {
		t.Errorf("no proposal should be recorded for an unknown order, got %d", n)
	}
}
