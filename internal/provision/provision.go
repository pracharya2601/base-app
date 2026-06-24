// Package provision is the importable core of the superadmin provisioning logic —
// create collections, add fields, set access rules (incl. the RBAC rule generator),
// seed records, set the app name — idempotently. It was extracted out of package
// main so BOTH the HTTP endpoint (POST /api/superadmin/provision) and in-process
// callers (e.g. the agentic ops executor that applies an approved schema change)
// share one implementation instead of duplicating it. The root `package main`
// cannot be imported, so shared platform logic lives here.
package provision

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// FieldSpec describes one field to create. jsonschema tags double as the contract
// the agentic propose_schema tool exposes to the model.
type FieldSpec struct {
	Name     string `json:"name" jsonschema:"description=Unique field name within the collection"`
	Type     string `json:"type" jsonschema:"description=One of: text, number, bool, email, select, relation"`
	Required bool   `json:"required" jsonschema:"description=Whether the value must be non-empty"`

	// select-only
	Values    []string `json:"values" jsonschema:"description=Allowed values (required for select)"`
	MaxSelect int      `json:"maxSelect" jsonschema:"description=>1 for multi-select/multi-relation; default 1"`

	// relation-only
	Collection    string `json:"collection" jsonschema:"description=Target collection name or id (required for relation)"`
	CascadeDelete bool   `json:"cascadeDelete" jsonschema:"description=Delete this record when the linked record is deleted"`
}

// Rules carries the five PocketBase access rules. Each is a pointer so "leave
// unchanged" (nil) is distinct from "public" ("").
type Rules struct {
	List   *string `json:"list"`
	View   *string `json:"view"`
	Create *string `json:"create"`
	Update *string `json:"update"`
	Delete *string `json:"delete"`
}

// CollectionSpec is one collection to create or extend.
type CollectionSpec struct {
	Name   string      `json:"name" jsonschema:"description=Collection name"`
	Fields []FieldSpec `json:"fields" jsonschema:"description=Fields to create"`
	Rules  *Rules      `json:"rules"`
	RBAC   bool        `json:"rbac" jsonschema:"description=If true, auto-generate role-permission access rules from the name"`
}

// Spec is a full provisioning request.
type Spec struct {
	AppName     string                      `json:"appName" jsonschema:"description=Optional app display name to set"`
	Collections []CollectionSpec            `json:"collections" jsonschema:"description=Collections to create or extend"`
	Seed        map[string][]map[string]any `json:"seed" jsonschema:"description=Optional seed records keyed by collection name"`
}

// Result is the idempotent outcome of Apply (JSON keys match the legacy endpoint).
type Result struct {
	CollectionsCreated []string            `json:"collectionsCreated"`
	CollectionsExisted []string            `json:"collectionsExisted"`
	FieldsAdded        map[string][]string `json:"fieldsAdded"`
	RecordsSeeded      map[string]int      `json:"recordsSeeded"`
	AppName            string              `json:"appName,omitempty"`
}

// InvalidSpecError marks a caller/spec problem (→ HTTP 400) as opposed to an
// internal failure (→ 500). The route uses errors.As to choose the status.
type InvalidSpecError struct{ Err error }

func (e *InvalidSpecError) Error() string { return e.Err.Error() }
func (e *InvalidSpecError) Unwrap() error { return e.Err }

func invalid(format string, a ...any) error { return &InvalidSpecError{fmt.Errorf(format, a...)} }

// RBACRules generates the five native access rules for a collection governed by the
// _roles/_permissions system. Each rule exact-matches the required token, a
// per-collection wildcard, or the global "*", across all of a user's roles.
func RBACRules(name string) *Rules {
	tok := func(action string) *string {
		s := fmt.Sprintf(
			`@request.auth.roles.permissions.token ?= "%s:%s"`+
				` || @request.auth.roles.permissions.token ?= "%s:*"`+
				` || @request.auth.roles.permissions.token ?= "*"`,
			name, action, name)
		return &s
	}
	read := tok("read")
	return &Rules{List: read, View: read, Create: tok("create"), Update: tok("update"), Delete: tok("delete")}
}

// ApplyRules sets any provided (non-nil) rules onto the collection and reports
// whether anything changed (so the caller knows to save).
func ApplyRules(col *core.Collection, r *Rules) bool {
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

// newField maps a FieldSpec to a concrete PocketBase field.
func newField(app core.App, f FieldSpec) (core.Field, error) {
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
			return nil, invalid("select field %q requires a non-empty \"values\" list", f.Name)
		}
		maxSelect := f.MaxSelect
		if maxSelect < 1 {
			maxSelect = 1
		}
		return &core.SelectField{Name: f.Name, Required: f.Required, Values: f.Values, MaxSelect: maxSelect}, nil
	case "relation":
		if f.Collection == "" {
			return nil, invalid("relation field %q requires \"collection\" (target name or id)", f.Name)
		}
		target, err := app.FindCollectionByNameOrId(f.Collection)
		if err != nil {
			return nil, invalid("relation field %q: target collection %q not found", f.Name, f.Collection)
		}
		maxSelect := f.MaxSelect
		if maxSelect < 1 {
			maxSelect = 1
		}
		return &core.RelationField{Name: f.Name, Required: f.Required, CollectionId: target.Id, MaxSelect: maxSelect, CascadeDelete: f.CascadeDelete}, nil
	default:
		return nil, invalid("unsupported field type %q for field %q", f.Type, f.Name)
	}
}

// Apply runs the provisioning spec idempotently: create missing collections,
// merge new fields into existing ones, apply rules, seed records, set the app name.
// Validation problems are returned as *InvalidSpecError; persistence failures as
// plain errors.
func Apply(app core.App, spec Spec) (Result, error) {
	res := Result{
		CollectionsCreated: []string{},
		CollectionsExisted: []string{},
		FieldsAdded:        map[string][]string{},
		RecordsSeeded:      map[string]int{},
	}

	// 1. Collections — create missing, else merge in absent fields.
	for _, cs := range spec.Collections {
		col, err := app.FindCollectionByNameOrId(cs.Name)
		if err != nil {
			col = core.NewBaseCollection(cs.Name)
			col.ListRule = nil // superuser-only by default
			col.ViewRule = nil
			for _, fs := range cs.Fields {
				field, ferr := newField(app, fs)
				if ferr != nil {
					return res, ferr
				}
				col.Fields.Add(field)
			}
			col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
			if cs.RBAC {
				ApplyRules(col, RBACRules(cs.Name))
			}
			ApplyRules(col, cs.Rules) // explicit rules override generated
			if serr := app.Save(col); serr != nil {
				return res, fmt.Errorf("failed to create collection %s: %w", cs.Name, serr)
			}
			res.CollectionsCreated = append(res.CollectionsCreated, cs.Name)
			continue
		}

		added := []string{}
		for _, fs := range cs.Fields {
			if col.Fields.GetByName(fs.Name) != nil {
				continue
			}
			field, ferr := newField(app, fs)
			if ferr != nil {
				return res, ferr
			}
			col.Fields.Add(field)
			added = append(added, fs.Name)
		}
		rulesChanged := false
		if cs.RBAC {
			rulesChanged = ApplyRules(col, RBACRules(cs.Name))
		}
		if ApplyRules(col, cs.Rules) {
			rulesChanged = true
		}
		if len(added) > 0 || rulesChanged {
			if serr := app.Save(col); serr != nil {
				return res, fmt.Errorf("failed to update collection %s: %w", cs.Name, serr)
			}
		}
		if len(added) > 0 {
			res.FieldsAdded[cs.Name] = added
		}
		res.CollectionsExisted = append(res.CollectionsExisted, cs.Name)
	}

	// 2. Seed records.
	for colName, rows := range spec.Seed {
		col, err := app.FindCollectionByNameOrId(colName)
		if err != nil {
			return res, invalid("seed target collection not found: %s", colName)
		}
		for _, row := range rows {
			rec := core.NewRecord(col)
			for k, v := range row {
				rec.Set(k, v)
			}
			if err := app.Save(rec); err != nil {
				return res, fmt.Errorf("failed to seed record in %s: %w", colName, err)
			}
			res.RecordsSeeded[colName]++
		}
	}

	// 3. App name.
	if spec.AppName != "" {
		settings := app.Settings()
		settings.Meta.AppName = spec.AppName
		if err := app.Save(settings); err != nil {
			return res, fmt.Errorf("failed to update settings: %w", err)
		}
		res.AppName = spec.AppName
	}

	return res, nil
}
