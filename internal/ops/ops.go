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

	"github.com/pocketbase/dbx"
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
	// ActionManageRole is the proposed-action kind for an RBAC role change.
	ActionManageRole = "manage_role"

	// RBAC system collections (the canonical machinery lives in the root roles.go;
	// this package only does a minimal self-contained role upsert).
	permissionCollection = "_permissions"
	roleCollection       = "_roles"
)

const opsPersona = "You are a platform operations engineer for this PocketBase-based backend. Translate the " +
	"operator's request into concrete platform changes using your tools:\n" +
	"- propose_schema: create or extend collections (fields of type text, number, bool, email, select, relation; " +
	"rbac:true to auto-generate role-based access rules).\n" +
	"- manage_role: create or update an access role and the permission tokens it grants " +
	"(\"collection:action\", \"collection:*\", or \"*\").\n" +
	"Tools do NOT apply immediately — they QUEUE the change for human approval. After proposing, tell the operator " +
	"in one sentence what you queued. Keep it minimal — don't invent collections, fields, or permissions the " +
	"operator didn't ask for. If the request isn't something your tools can express, say so plainly."

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
	orchestrator.RegisterActionExecutor(ActionManageRole, executeManageRole)
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
		ai.NewTool(
			"manage_role",
			"Propose creating or updating an access ROLE and the permission tokens it grants. Does NOT apply "+
				"immediately — it QUEUES the change for human approval. Tokens are \"collection:action\" "+
				"(action = read|create|update|delete), \"collection:*\" for all actions on a collection, or \"*\" "+
				"for full access.",
			func(ctx context.Context, in struct {
				RoleName    string   `json:"roleName" jsonschema:"description=The role name, e.g. support-readonly"`
				Description string   `json:"description" jsonschema:"description=Optional human-readable description"`
				Permissions []string `json:"permissions" jsonschema:"description=Permission tokens, e.g. [\"orders:read\",\"support_tickets:read\"]"`
			}) (string, error) {
				if task == nil {
					return "", fmt.Errorf("no task context to attach the role proposal to")
				}
				if strings.TrimSpace(in.RoleName) == "" {
					return "Provide a roleName.", nil
				}
				if err := orchestrator.ProposeAction(app, task.Id, ActionManageRole, map[string]any{
					"roleName":    in.RoleName,
					"description": in.Description,
					"permissions": in.Permissions,
				}); err != nil {
					return "", err
				}
				return fmt.Sprintf("Queued a role change for approval: %q granting %d permission(s). Applies once approved.",
					in.RoleName, len(in.Permissions)), nil
			}),
	}
}

// executeManageRole is the approval-time executor for a role change: find-or-create
// the permission tokens, then upsert the role with that permission set. Minimal and
// self-contained — the canonical RBAC machinery (hooks/backfill/seed) stays in the
// root roles.go; if more RBAC ops are added, extract a shared internal/rbac core.
func executeManageRole(app core.App, _ *core.Record, params json.RawMessage) (string, error) {
	var p struct {
		RoleName    string   `json:"roleName"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("bad role params: %w", err)
	}
	if strings.TrimSpace(p.RoleName) == "" {
		return "", fmt.Errorf("roleName is required")
	}
	ids := make([]string, 0, len(p.Permissions))
	for _, tok := range p.Permissions {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		id, err := findOrCreatePermissionID(app, tok)
		if err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	role, err := app.FindFirstRecordByFilter(roleCollection, "name = {:n}", dbx.Params{"n": p.RoleName})
	if err != nil {
		col, cerr := app.FindCollectionByNameOrId(roleCollection)
		if cerr != nil {
			return "", cerr
		}
		role = core.NewRecord(col)
		role.Set("name", p.RoleName)
	}
	if p.Description != "" {
		role.Set("description", p.Description)
	}
	role.Set("permissions", ids)
	if err := app.Save(role); err != nil {
		return "", fmt.Errorf("failed to save role %q: %w", p.RoleName, err)
	}
	return fmt.Sprintf("Role %q now grants: %s.", p.RoleName, strings.Join(p.Permissions, ", ")), nil
}

// findOrCreatePermissionID returns the _permissions record id for a token, creating
// the record if it doesn't exist yet.
func findOrCreatePermissionID(app core.App, token string) (string, error) {
	if rec, err := app.FindFirstRecordByFilter(permissionCollection, "token = {:t}", dbx.Params{"t": token}); err == nil {
		return rec.Id, nil
	}
	col, err := app.FindCollectionByNameOrId(permissionCollection)
	if err != nil {
		return "", err
	}
	rec := core.NewRecord(col)
	rec.Set("token", token)
	if err := app.Save(rec); err != nil {
		return "", fmt.Errorf("failed to create permission %q: %w", token, err)
	}
	return rec.Id, nil
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
