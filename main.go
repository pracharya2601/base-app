package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// ---- request shapes for /api/superadmin/provision ----

type fieldSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // text | number | bool | email | select | relation
	Required bool   `json:"required"`

	// select-only
	Values    []string `json:"values"`    // allowed values (required for select)
	MaxSelect int      `json:"maxSelect"` // >1 for multi-select; defaults to 1

	// relation-only
	Collection    string `json:"collection"`    // target collection name or id (required for relation)
	CascadeDelete bool   `json:"cascadeDelete"` // delete this record when the linked record is deleted
}

// collectionRules carries the five PocketBase access rules as filter strings.
// Each is a pointer so provision can tell "leave unchanged" (nil/omitted) apart
// from "set to public" (""). A non-nil "" means public; a non-nil expression
// like `@request.auth.roles.name ?= "editor"` means role-gated. To make a rule
// superuser-only, omit it on create (new collections default to nil).
type collectionRules struct {
	List   *string `json:"list"`
	View   *string `json:"view"`
	Create *string `json:"create"`
	Update *string `json:"update"`
	Delete *string `json:"delete"`
}

type collectionSpec struct {
	Name   string           `json:"name"`
	Fields []fieldSpec      `json:"fields"`
	Rules  *collectionRules `json:"rules"` // optional explicit access rules
	RBAC   bool             `json:"rbac"`  // if true, auto-generate role-permission rules from the name
}

// rbacRules generates the five native access rules for a collection governed by
// the _roles/_permissions system. Each rule exact-matches the required token, a
// per-collection wildcard, or the global "*", across all of a user's roles.
func rbacRules(name string) *collectionRules {
	tok := func(action string) *string {
		s := fmt.Sprintf(
			`@request.auth.roles.permissions.token ?= "%s:%s"`+
				` || @request.auth.roles.permissions.token ?= "%s:*"`+
				` || @request.auth.roles.permissions.token ?= "*"`,
			name, action, name)
		return &s
	}
	read := tok("read")
	return &collectionRules{
		List:   read,
		View:   read,
		Create: tok("create"),
		Update: tok("update"),
		Delete: tok("delete"),
	}
}

// applyRules sets any provided (non-nil) rules onto the collection and reports
// whether anything changed (so the caller knows to save).
func applyRules(col *core.Collection, r *collectionRules) bool {
	if r == nil {
		return false
	}
	changed := false
	if r.List != nil {
		col.ListRule = r.List
		changed = true
	}
	if r.View != nil {
		col.ViewRule = r.View
		changed = true
	}
	if r.Create != nil {
		col.CreateRule = r.Create
		changed = true
	}
	if r.Update != nil {
		col.UpdateRule = r.Update
		changed = true
	}
	if r.Delete != nil {
		col.DeleteRule = r.Delete
		changed = true
	}
	return changed
}

type provisionRequest struct {
	AppName     string                       `json:"appName"`
	Collections []collectionSpec             `json:"collections"`
	Seed        map[string][]map[string]any  `json:"seed"` // collectionName -> []record
}

// newField maps a type string to a concrete PocketBase field. It needs the app
// so relation fields can resolve their target collection by name -> id.
func newField(app core.App, f fieldSpec) (core.Field, error) {
	switch f.Type {
	case "text", "":
		return &core.TextField{Name: f.Name, Required: f.Required}, nil
	case "number":
		return &core.NumberField{Name: f.Name, Required: f.Required}, nil
	case "bool":
		return &core.BoolField{Name: f.Name, Required: f.Required}, nil
	case "email":
		return &core.EmailField{Name: f.Name, Required: f.Required}, nil
	case "select":
		if len(f.Values) == 0 {
			return nil, fmt.Errorf("select field %q requires a non-empty \"values\" list", f.Name)
		}
		maxSelect := f.MaxSelect
		if maxSelect < 1 {
			maxSelect = 1 // single-select default
		}
		return &core.SelectField{
			Name:      f.Name,
			Required:  f.Required,
			Values:    f.Values,
			MaxSelect: maxSelect,
		}, nil
	case "relation":
		if f.Collection == "" {
			return nil, fmt.Errorf("relation field %q requires \"collection\" (target name or id)", f.Name)
		}
		target, err := app.FindCollectionByNameOrId(f.Collection)
		if err != nil {
			return nil, fmt.Errorf("relation field %q: target collection %q not found", f.Name, f.Collection)
		}
		maxSelect := f.MaxSelect
		if maxSelect < 1 {
			maxSelect = 1 // single relation default
		}
		return &core.RelationField{
			Name:          f.Name,
			Required:      f.Required,
			CollectionId:  target.Id,
			MaxSelect:     maxSelect,
			CascadeDelete: f.CascadeDelete,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported field type %q for field %q", f.Type, f.Name)
	}
}

// fieldTypeParam describes one configurable parameter of a field type.
type fieldTypeParam struct {
	Name        string `json:"name"`
	JSONType    string `json:"jsonType"` // string | number | boolean | string[]
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// fieldTypeDef is the self-describing contract for a supported field type.
// This single catalog is the source of truth consumed by: (1) the LLM via the
// MCP server, (2) the frontend for form rendering / type generation, and
// (3) this backend for validation.
type fieldTypeDef struct {
	Type        string           `json:"type"`
	Description string           `json:"description"`
	Params      []fieldTypeParam `json:"params"`
}

func fieldTypeCatalog() []fieldTypeDef {
	common := []fieldTypeParam{
		{Name: "name", JSONType: "string", Required: true, Description: "Unique field name within the collection."},
		{Name: "required", JSONType: "boolean", Required: false, Description: "If true, the value must be non-empty."},
	}
	return []fieldTypeDef{
		{Type: "text", Description: "Free-form short text.", Params: common},
		{Type: "number", Description: "Numeric value (int or float).", Params: common},
		{Type: "bool", Description: "True/false flag.", Params: common},
		{Type: "email", Description: "Validated email address.", Params: common},
		{Type: "select", Description: "One or more values from a fixed allowed list.", Params: append(append([]fieldTypeParam{}, common...),
			fieldTypeParam{Name: "values", JSONType: "string[]", Required: true, Description: "Allowed values."},
			fieldTypeParam{Name: "maxSelect", JSONType: "number", Required: false, Description: "Max selectable values; >1 = multi-select, default 1."},
		)},
		{Type: "relation", Description: "Link to record(s) in another collection.", Params: append(append([]fieldTypeParam{}, common...),
			fieldTypeParam{Name: "collection", JSONType: "string", Required: true, Description: "Target collection name or id."},
			fieldTypeParam{Name: "maxSelect", JSONType: "number", Required: false, Description: "Max linked records; >1 = multi-relation, default 1."},
			fieldTypeParam{Name: "cascadeDelete", JSONType: "boolean", Required: false, Description: "Delete this record when the linked record is deleted."},
		)},
	}
}

func main() {
	app := pocketbase.New()

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		// API-key system: ensure storage, register the auth middleware (runs
		// after the JWT loader), and wire the mint/list/revoke routes.
		if err := ensureAPIKeyCollection(app); err != nil {
			return err
		}
		se.Router.Bind(apiKeyAuthMiddleware(app))
		registerAPIKeyRoutes(se, app)
		registerKeysUI(se) // GET /admin/apikeys — browser UI for minting keys

		// User RBAC: ensure the _permissions + _roles system collections, run the
		// migration/seed, and expose the permission catalog. Enforcement is via
		// native per-collection rules (collections marked rbac:true in provision),
		// NOT middleware.
		if err := ensurePermissionsCollection(app); err != nil {
			return err
		}
		if err := ensureRolesCollection(app); err != nil {
			return err
		}
		if err := ensureServiceAccountsCollection(app); err != nil {
			return err
		}
		migrateAndSeedRBAC(app)
		migrateAPIKeysToServiceAccounts(app) // give existing keys a roled identity
		registerRoleRoutes(se, app)

		// Keep _permissions in sync with the schema: auto-create CRUD tokens when
		// a collection is created, delete them when it's dropped. Backfill covers
		// collections that already existed.
		registerPermissionSyncHooks(app)
		backfillRBAC(app)

		// Self-describing field-type catalog. PUBLIC (pure capability metadata):
		// the frontend, the MCP server, and humans all read this to know what
		// the provision endpoint can build. This is the shared contract.
		se.Router.GET("/api/superadmin/field-types", func(e *core.RequestEvent) error {
			return e.JSON(http.StatusOK, map[string]any{"fieldTypes": fieldTypeCatalog()})
		})

		// One-call provisioning: create collections + seed records + set appName.
		// Guarded so ONLY a _superusers token can call it.
		se.Router.POST("/api/superadmin/provision", func(e *core.RequestEvent) error {
			req := provisionRequest{}
			if err := e.BindBody(&req); err != nil {
				return e.BadRequestError("invalid request body", err)
			}

			result := map[string]any{}
			created := []string{}
			existed := []string{}
			fieldsAdded := map[string][]string{}
			seeded := map[string]int{}

			// 1. Collections — fully idempotent:
			//    - collection missing  -> create it with the given fields
			//    - collection present  -> add only the fields it doesn't have yet
			//      (server does the fetch-merge-save so callers never replace the
			//       whole fields array by hand).
			for _, cs := range req.Collections {
				col, err := app.FindCollectionByNameOrId(cs.Name)
				if err != nil {
					// Not found -> create new.
					col = core.NewBaseCollection(cs.Name)
					// superuser-only by default; loosen per your needs.
					col.ListRule = nil
					col.ViewRule = nil
					for _, fs := range cs.Fields {
						field, ferr := newField(app, fs)
						if ferr != nil {
							return e.BadRequestError(ferr.Error(), ferr)
						}
						col.Fields.Add(field)
					}
					col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
					if cs.RBAC {
						applyRules(col, rbacRules(cs.Name)) // auto-generated role-permission rules
					}
					applyRules(col, cs.Rules) // explicit rules override the generated ones
					if serr := app.Save(col); serr != nil {
						return e.InternalServerError("failed to create collection "+cs.Name, serr)
					}
					created = append(created, cs.Name)
					continue
				}

				// Exists -> merge in any fields that aren't already present, and
				// apply any access rules provided.
				added := []string{}
				for _, fs := range cs.Fields {
					if col.Fields.GetByName(fs.Name) != nil {
						continue // field already exists — skip (no clobber)
					}
					field, ferr := newField(app, fs)
					if ferr != nil {
						return e.BadRequestError(ferr.Error(), ferr)
					}
					col.Fields.Add(field)
					added = append(added, fs.Name)
				}
				rulesChanged := false
				if cs.RBAC {
					rulesChanged = applyRules(col, rbacRules(cs.Name))
				}
				if applyRules(col, cs.Rules) {
					rulesChanged = true
				}
				if len(added) > 0 || rulesChanged {
					if serr := app.Save(col); serr != nil {
						return e.InternalServerError("failed to update collection "+cs.Name, serr)
					}
				}
				if len(added) > 0 {
					fieldsAdded[cs.Name] = added
				}
				existed = append(existed, cs.Name)
			}

			// 2. Seed records.
			for colName, rows := range req.Seed {
				col, err := app.FindCollectionByNameOrId(colName)
				if err != nil {
					return e.BadRequestError("seed target collection not found: "+colName, err)
				}
				for _, row := range rows {
					rec := core.NewRecord(col)
					for k, v := range row {
						rec.Set(k, v)
					}
					if err := app.Save(rec); err != nil {
						return e.InternalServerError("failed to seed record in "+colName, err)
					}
					seeded[colName]++
				}
			}

			// 3. Update a global setting.
			if req.AppName != "" {
				settings := app.Settings()
				settings.Meta.AppName = req.AppName
				if err := app.Save(settings); err != nil {
					return e.InternalServerError("failed to update settings", err)
				}
				result["appName"] = req.AppName
			}

			result["collectionsCreated"] = created
			result["collectionsExisted"] = existed
			result["fieldsAdded"] = fieldsAdded
			result["recordsSeeded"] = seeded
			return e.JSON(http.StatusOK, result)
		}).Bind(apis.RequireSuperuserAuth())

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
