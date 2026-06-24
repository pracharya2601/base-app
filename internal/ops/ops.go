// Package ops is the agentic ADMIN function: instead of a CRUD dashboard, an
// operator states intent ("add a blog_posts collection…") and an ops agent
// translates it into a concrete platform change, PROPOSES it (behind the same
// approval guard as every other write-tool), and a human approves to apply it.
// This is the foundation for "run everything from /admin via the agentic system":
// each new admin capability is just another tool the ops agent can propose.
//
// First capability: schema provisioning (via internal/provision). The agent never
// mutates schema mid-draft — it proposes a provision Spec that runs only on approve.
package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/ai"
	"base-app/internal/orchestrator"
	"base-app/internal/provision"
)

const (
	// KindOpsCommand tags an agentic admin task.
	KindOpsCommand = "ops_command"
	// RoleOps is the platform-operations agent role.
	RoleOps = "ops"
	// ActionProvisionSchema is the proposed-action kind for a schema change.
	ActionProvisionSchema = "provision_schema"
)

const opsPersona = "You are a platform operations engineer for this PocketBase-based backend. Translate the " +
	"operator's request into a concrete schema change and call the propose_schema tool with the collections to " +
	"create or extend — each with a name and fields (type: text, number, bool, email, select, or relation), and " +
	"rbac:true when the collection should get role-based access rules. The change does NOT apply immediately: " +
	"propose_schema QUEUES it for human approval. After proposing, tell the operator in one sentence what you " +
	"queued. Keep it minimal — do not invent fields or collections the operator didn't ask for. If the request " +
	"isn't a schema change you can express, say so plainly instead of calling the tool."

// Register wires the ops function: seed the ops agent, register its tool, the
// approval-time executor, and a trivial approve-action (which also makes ops tasks
// action-kind, so they always require human approval and are never autopiloted).
func Register(app core.App) {
	if err := orchestrator.SeedAgent(app, "Ozzy (Ops)", RoleOps, opsPersona, orchestrator.SystemOwner); err != nil {
		app.Logger().Error("ops: seed agent failed", "err", err)
	}
	orchestrator.RegisterApproveAction(KindOpsCommand, opsApprove)
	orchestrator.RegisterTaskTools(KindOpsCommand, opsTools)
	orchestrator.RegisterActionExecutor(ActionProvisionSchema, executeProvision)
	app.Logger().Info("ops: agentic platform-ops function active")
}

// opsApprove is the approve-action for an ops task. The actual schema change is a
// proposed action that the engine already executed (in the approve route) before
// this runs, so there's nothing more to do — its job is to mark the kind as
// action-kind (human-approval-required, autopilot-exempt).
func opsApprove(app core.App, task *core.Record) error { return nil }

// opsTools gives the ops agent its propose_schema tool, closed over the task so the
// proposal attaches to it.
func opsTools(app core.App, task *core.Record) []ai.Tool {
	return []ai.Tool{
		ai.NewTool(
			"propose_schema",
			"Propose creating or extending collections (a schema change). Does NOT apply immediately — it "+
				"QUEUES the change for human approval. Provide collections with a name and fields "+
				"(type: text|number|bool|email|select|relation), and rbac:true to auto-generate access rules.",
			func(ctx context.Context, in struct {
				Collections []provision.CollectionSpec `json:"collections" jsonschema:"description=Collections to create or extend"`
				AppName     string                     `json:"appName" jsonschema:"description=Optional app display name to set"`
			}) (string, error) {
				if task == nil {
					return "", fmt.Errorf("no task context to attach the schema proposal to")
				}
				if len(in.Collections) == 0 && in.AppName == "" {
					return "Nothing to propose — provide at least one collection.", nil
				}
				spec := provision.Spec{Collections: in.Collections, AppName: in.AppName}
				if err := orchestrator.ProposeAction(app, task.Id, ActionProvisionSchema, spec); err != nil {
					return "", err
				}
				names := make([]string, 0, len(in.Collections))
				for _, c := range in.Collections {
					names = append(names, c.Name)
				}
				return fmt.Sprintf("Queued a schema change for approval: %s. It will be applied once approved.",
					strings.Join(names, ", ")), nil
			}),
	}
}

// executeProvision is the approval-time executor: it applies the proposed schema
// change via the shared provision core. Runs only after a human approves the task.
func executeProvision(app core.App, _ *core.Record, params json.RawMessage) (string, error) {
	var spec provision.Spec
	if err := json.Unmarshal(params, &spec); err != nil {
		return "", fmt.Errorf("bad provision params: %w", err)
	}
	res, err := provision.Apply(app, spec)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Schema applied — created: %v, extended: %v.", res.CollectionsCreated, res.CollectionsExisted), nil
}
