package main

import (
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/provision"
)

// User-facing RBAC enforced entirely by PocketBase's NATIVE per-collection rules
// (no custom middleware). The model is normalized for exact-match matching:
//
//   _permissions  — system collection; one record per access token, e.g.
//                   "articles:create", "articles:*" (collection wildcard), "*" (global)
//   _roles        — system collection; "permissions" is a multi-relation to _permissions
//   users.roles   — multi-relation to _roles
//
// A collection marked rbac:true in provision gets rules like:
//   @request.auth.roles.permissions.token ?= "articles:create"
//      || @request.auth.roles.permissions.token ?= "articles:*"
//      || @request.auth.roles.permissions.token ?= "*"
// The relation traversal flattens tokens across ALL of a user's roles, so holding
// multiple roles just unions their permissions. ?= is exact (no substring risk).

const (
	roleCollection       = "_roles"
	permissionCollection = "_permissions"
)

// knownActions is the CRUD vocabulary a token's action half can use. "read"
// covers both list and view.
var knownActions = []string{"read", "create", "update", "delete"}

// ensurePermissionsCollection creates the _permissions system collection.
func ensurePermissionsCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(permissionCollection); err == nil {
		return nil
	}
	col := core.NewBaseCollection(permissionCollection)
	col.System = true // locks the collection after creation
	col.Fields.Add(&core.TextField{Name: "token", Required: true})
	col.Fields.Add(&core.TextField{Name: "description"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(col)
}

// ensureRolesCollection creates/upgrades the _roles system collection so that its
// "permissions" field is a multi-relation to _permissions. A legacy TEXT
// permissions field (from the middleware-era design) is converted in place; the
// now-orphaned role records are wiped so they get reseeded with relations.
func ensureRolesCollection(app core.App) error {
	perms, err := app.FindCollectionByNameOrId(permissionCollection)
	if err != nil {
		return err
	}

	if existing, err := app.FindCollectionByNameOrId(roleCollection); err == nil {
		f := existing.Fields.GetByName("permissions")
		if _, isRel := f.(*core.RelationField); f != nil && !isRel {
			// Legacy text field -> drop it, re-add as a relation (two saves so the
			// new field gets a fresh identity), then wipe stale role records.
			existing.Fields.RemoveByName("permissions")
			if err := app.Save(existing); err != nil {
				return err
			}
			existing, _ = app.FindCollectionByNameOrId(roleCollection)
			existing.Fields.Add(&core.RelationField{Name: "permissions", CollectionId: perms.Id, MaxSelect: 200})
			if err := app.Save(existing); err != nil {
				return err
			}
			if recs, _ := app.FindAllRecords(roleCollection); recs != nil {
				for _, r := range recs {
					_ = app.Delete(r)
				}
			}
			return nil
		}
		if f == nil {
			existing.Fields.Add(&core.RelationField{Name: "permissions", CollectionId: perms.Id, MaxSelect: 200})
			return app.Save(existing)
		}
		return nil
	}

	col := core.NewBaseCollection(roleCollection)
	col.System = true
	col.Fields.Add(&core.TextField{Name: "name", Required: true})
	col.Fields.Add(&core.TextField{Name: "description"})
	col.Fields.Add(&core.RelationField{Name: "permissions", CollectionId: perms.Id, MaxSelect: 200})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(col)
}

// findOrCreatePermission returns the _permissions record id for a token,
// creating the record if it doesn't exist yet.
func findOrCreatePermission(app core.App, col *core.Collection, token, desc string) string {
	if rec, err := app.FindFirstRecordByFilter(permissionCollection, "token = {:t}", dbx.Params{"t": token}); err == nil {
		return rec.Id
	}
	rec := core.NewRecord(col)
	rec.Set("token", token)
	rec.Set("description", desc)
	if err := app.Save(rec); err != nil {
		app.Logger().Error("failed to create permission", "token", token, "err", err)
		return ""
	}
	return rec.Id
}

// migrateAndSeedRBAC is a best-effort, idempotent startup step: repoint
// users.roles -> _roles, drop the legacy non-system "roles" collection, and seed
// default permission tokens + roles when _roles is empty.
func migrateAndSeedRBAC(app core.App) {
	permsCol, err := app.FindCollectionByNameOrId(permissionCollection)
	if err != nil {
		return
	}
	rolesCol, err := app.FindCollectionByNameOrId(roleCollection)
	if err != nil {
		return
	}

	// Repoint users.roles -> _roles (relation target is immutable, so drop+re-add).
	if users, err := app.FindCollectionByNameOrId("users"); err == nil {
		if rf, ok := users.Fields.GetByName("roles").(*core.RelationField); ok && rf.CollectionId != rolesCol.Id {
			users.Fields.RemoveByName("roles")
			if err := app.Save(users); err != nil {
				app.Logger().Error("rbac: failed to drop stale users.roles", "err", err)
			}
			users, _ = app.FindCollectionByNameOrId("users")
		}
		if users != nil && users.Fields.GetByName("roles") == nil {
			users.Fields.Add(&core.RelationField{Name: "roles", CollectionId: rolesCol.Id, MaxSelect: 20})
			if err := app.Save(users); err != nil {
				app.Logger().Error("rbac: failed to add users.roles -> _roles", "err", err)
			}
		}
	}

	// Drop the legacy non-system "roles" collection, if it lingers.
	if old, err := app.FindCollectionByNameOrId("roles"); err == nil && !old.System {
		_ = app.Delete(old)
	}

	// Seed default tokens + roles when empty.
	if n, _ := app.CountRecords(roleCollection); n == 0 {
		all := findOrCreatePermission(app, permsCol, "*", "Full access to all collections and actions")
		aRead := findOrCreatePermission(app, permsCol, "articles:read", "Read articles")
		aCreate := findOrCreatePermission(app, permsCol, "articles:create", "Create articles")
		aUpdate := findOrCreatePermission(app, permsCol, "articles:update", "Update articles")
		uRead := findOrCreatePermission(app, permsCol, "users:read", "Read users")

		seed := []struct {
			name, desc string
			perms      []string
		}{
			{"viewer", "Read-only access", []string{aRead, uRead}},
			{"editor", "Read and write articles", []string{aRead, aCreate, aUpdate}},
			{"admin", "Full access to all collections", []string{all}},
		}
		for _, s := range seed {
			rec := core.NewRecord(rolesCol)
			rec.Set("name", s.name)
			rec.Set("description", s.desc)
			rec.Set("permissions", s.perms)
			if err := app.Save(rec); err != nil {
				app.Logger().Error("rbac: seed role failed", "name", s.name, "err", err)
			}
		}
	}
}

// governsPermissions reports whether a collection should have CRUD permission
// tokens auto-managed for it. We skip system collections (incl. _roles,
// _permissions, _apiKeys, _superusers, ...) — their access isn't role-gated.
func governsPermissions(c *core.Collection) bool {
	return c != nil && !c.System
}

// permissionTokens returns the four CRUD tokens for a collection name.
func permissionTokens(name string) []string {
	return []string{name + ":read", name + ":create", name + ":update", name + ":delete"}
}

// syncPermissionsForCollection ensures the CRUD permission records exist for a
// collection (idempotent — findOrCreate skips ones already present).
func syncPermissionsForCollection(app core.App, c *core.Collection) {
	if !governsPermissions(c) {
		return
	}
	permsCol, err := app.FindCollectionByNameOrId(permissionCollection)
	if err != nil {
		return
	}
	for _, tok := range permissionTokens(c.Name) {
		findOrCreatePermission(app, permsCol, tok, "auto-created for collection "+c.Name)
	}
}

// removePermissionsForCollection deletes every permission record scoped to a
// collection (its "<name>:..." tokens). PocketBase strips the deleted ids from
// any _roles.permissions relations automatically.
func removePermissionsForCollection(app core.App, c *core.Collection) {
	if c == nil {
		return
	}
	recs, err := app.FindAllRecords(permissionCollection)
	if err != nil {
		return
	}
	prefix := c.Name + ":"
	for _, r := range recs {
		if strings.HasPrefix(r.GetString("token"), prefix) {
			if err := app.Delete(r); err != nil {
				app.Logger().Error("rbac: failed to delete permission", "token", r.GetString("token"), "err", err)
			}
		}
	}
}

// applyRbacRulesIfUnset auto-applies the native role-permission rules to a freshly
// created collection — but ONLY when the creator left all five rules at the
// default (nil). If any rule was set explicitly (dashboard or provision `rules`),
// we respect it and don't clobber. This makes every new table RBAC-governed by
// default; grant the relevant tokens to a role (or hold `*`) to access it.
func applyRbacRulesIfUnset(app core.App, c *core.Collection) {
	if !governsPermissions(c) {
		return
	}
	if c.ListRule != nil || c.ViewRule != nil || c.CreateRule != nil || c.UpdateRule != nil || c.DeleteRule != nil {
		return // explicit rules present — leave them alone
	}
	fresh, err := app.FindCollectionByNameOrId(c.Id)
	if err != nil {
		return
	}
	provision.ApplyRules(fresh, provision.RBACRules(fresh.Name))
	if err := app.Save(fresh); err != nil {
		app.Logger().Error("rbac: failed to auto-apply rules on create", "collection", fresh.Name, "err", err)
	}
}

// registerPermissionSyncHooks keeps RBAC in lockstep with the schema: creating a
// collection auto-creates its CRUD tokens AND applies role-permission rules;
// deleting it removes the tokens.
func registerPermissionSyncHooks(app core.App) {
	app.OnCollectionAfterCreateSuccess().BindFunc(func(e *core.CollectionEvent) error {
		syncPermissionsForCollection(e.App, e.Collection)
		applyRbacRulesIfUnset(e.App, e.Collection)
		return e.Next()
	})
	app.OnCollectionAfterDeleteSuccess().BindFunc(func(e *core.CollectionEvent) error {
		removePermissionsForCollection(e.App, e.Collection)
		return e.Next()
	})
}

// backfillRBAC ensures every non-system collection is fully RBAC-managed at
// startup: its CRUD tokens exist AND its rules are governed (when left at the
// default nil — collections with explicit rules, like `users`, are untouched).
// This is the startup counterpart to the create hook, so governance is universal
// and automatic without anyone remembering to opt a collection in.
func backfillRBAC(app core.App) {
	cols, err := app.FindAllCollections()
	if err != nil {
		return
	}
	for _, c := range cols {
		syncPermissionsForCollection(app, c)
		applyRbacRulesIfUnset(app, c)
	}
}

// roleTokens resolves a role record's permission relation ids to token strings.
func roleTokens(app core.App, role *core.Record) []string {
	tokens := []string{}
	for _, pid := range role.GetStringSlice("permissions") {
		if p, err := app.FindRecordById(permissionCollection, pid); err == nil {
			tokens = append(tokens, p.GetString("token"))
		}
	}
	return tokens
}

// registerRoleRoutes exposes the permission grammar plus agent-drivable RBAC
// management endpoints (list/upsert roles, assign user roles). The management
// routes are superuser-bound and scope-gated (roles:read / roles:write) so an
// API key can drive them on the control plane.
func registerRoleRoutes(se *core.ServeEvent, app core.App) {
	// Public: the token grammar so callers/LLMs know what permissions look like.
	se.Router.GET("/api/superadmin/permissions", func(e *core.RequestEvent) error {
		return e.JSON(200, map[string]any{
			"format":     "<collection>:<action>",
			"actions":    knownActions,
			"wildcards":  []string{"<collection>:*", "*"},
			"example":    []string{"users:read", "users:create", "articles:update", "articles:delete"},
			"enforcedBy": "native per-collection API rules (set a collection's rbac:true in provision)",
			"storedAs":   "records in the _permissions system collection, related from _roles.permissions",
		})
	})

	// List roles with their resolved permission tokens.
	se.Router.GET("/api/superadmin/roles", func(e *core.RequestEvent) error {
		roles, err := app.FindAllRecords(roleCollection)
		if err != nil {
			return e.InternalServerError("failed to list roles", err)
		}
		out := make([]map[string]any, 0, len(roles))
		for _, r := range roles {
			out = append(out, map[string]any{
				"id":          r.Id,
				"name":        r.GetString("name"),
				"description": r.GetString("description"),
				"permissions": roleTokens(app, r),
			})
		}
		return e.JSON(200, map[string]any{"roles": out})
	}).Bind(apis.RequireSuperuserAuth())

	// Upsert a role by name and set its FULL permission-token list (tokens are
	// created in _permissions if missing). Idempotent.
	se.Router.POST("/api/superadmin/roles", func(e *core.RequestEvent) error {
		var body struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Permissions []string `json:"permissions"`
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		if body.Name == "" {
			return e.BadRequestError("'name' is required", nil)
		}
		permsCol, err := app.FindCollectionByNameOrId(permissionCollection)
		if err != nil {
			return e.InternalServerError("permissions collection missing", err)
		}
		rolesCol, err := app.FindCollectionByNameOrId(roleCollection)
		if err != nil {
			return e.InternalServerError("roles collection missing", err)
		}
		role, err := app.FindFirstRecordByFilter(roleCollection, "name = {:n}", dbx.Params{"n": body.Name})
		if err != nil {
			role = core.NewRecord(rolesCol)
			role.Set("name", body.Name)
		}
		if body.Description != "" {
			role.Set("description", body.Description)
		}
		ids := make([]string, 0, len(body.Permissions))
		for _, tok := range body.Permissions {
			if id := findOrCreatePermission(app, permsCol, tok, ""); id != "" {
				ids = append(ids, id)
			}
		}
		role.Set("permissions", ids)
		if err := app.Save(role); err != nil {
			return e.InternalServerError("failed to save role", err)
		}
		return e.JSON(200, map[string]any{
			"id": role.Id, "name": role.GetString("name"),
			"description": role.GetString("description"),
			"permissions": roleTokens(app, role),
		})
	}).Bind(apis.RequireSuperuserAuth())

	// Set a user's roles (by role NAME). Identify the user by id or email.
	se.Router.POST("/api/superadmin/users/roles", func(e *core.RequestEvent) error {
		var body struct {
			UserID string   `json:"userId"`
			Email  string   `json:"email"`
			Roles  []string `json:"roles"`
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		var user *core.Record
		var err error
		switch {
		case body.UserID != "":
			user, err = app.FindRecordById("users", body.UserID)
		case body.Email != "":
			user, err = app.FindFirstRecordByFilter("users", "email = {:e}", dbx.Params{"e": body.Email})
		default:
			return e.BadRequestError("provide 'userId' or 'email'", nil)
		}
		if err != nil {
			return e.NotFoundError("user not found", err)
		}
		ids := make([]string, 0, len(body.Roles))
		for _, name := range body.Roles {
			r, err := app.FindFirstRecordByFilter(roleCollection, "name = {:n}", dbx.Params{"n": name})
			if err != nil {
				return e.BadRequestError("unknown role: "+name, nil)
			}
			ids = append(ids, r.Id)
		}
		user.Set("roles", ids)
		if err := app.Save(user); err != nil {
			return e.InternalServerError("failed to set user roles", err)
		}
		return e.JSON(200, map[string]any{"userId": user.Id, "email": user.GetString("email"), "roles": body.Roles})
	}).Bind(apis.RequireSuperuserAuth())
}
