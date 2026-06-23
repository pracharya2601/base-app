package main

import (
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// Service accounts are the auth identity an API key acts AS. Making the key a
// roled, non-superuser identity means the SAME native collection rules that gate
// human users also gate API keys — one enforcement path, no bypass.
//
// _serviceAccounts is a SYSTEM AUTH collection with a `roles` relation (→ _roles)
// matching users.roles, so `@request.auth.roles.permissions.token` resolves the
// same way regardless of whether the caller is a user or a key.

const serviceAccountCollection = "_serviceAccounts"

// ensureServiceAccountsCollection creates the system auth collection (or adds the
// roles relation to an existing one). Requires _roles to exist already.
func ensureServiceAccountsCollection(app core.App) error {
	roles, err := app.FindCollectionByNameOrId(roleCollection)
	if err != nil {
		return err
	}

	if existing, err := app.FindCollectionByNameOrId(serviceAccountCollection); err == nil {
		if existing.Fields.GetByName("roles") == nil {
			existing.Fields.Add(&core.RelationField{Name: "roles", CollectionId: roles.Id, MaxSelect: 20})
			return app.Save(existing)
		}
		return nil
	}

	col := core.NewAuthCollection(serviceAccountCollection)
	col.System = true // locks it after creation
	col.Fields.Add(&core.RelationField{Name: "roles", CollectionId: roles.Id, MaxSelect: 20})
	col.Fields.Add(&core.TextField{Name: "label"}) // human-friendly name, mirrors the key name
	return app.Save(col)
}

// isRecordRoute reports whether a path is a record CRUD endpoint
// (/api/collections/{c}/records...), i.e. the "data plane" gated by RBAC rules.
func isRecordRoute(path string) bool {
	return strings.HasPrefix(path, "/api/collections/") && strings.Contains(path, "/records")
}

// createServiceAccount creates a roled, non-loginable auth identity for an API
// key to act AS. The random email/password are never used to log in — the API
// key itself is the credential; this record just carries the roles.
func createServiceAccount(app core.App, label string, roleIds []string) (string, error) {
	col, err := app.FindCollectionByNameOrId(serviceAccountCollection)
	if err != nil {
		return "", err
	}
	raw, err := newRawKey() // reuse the key randomness helper (apikeys.go)
	if err != nil {
		return "", err
	}
	id := strings.TrimPrefix(raw, "pbk_")
	rec := core.NewRecord(col)
	rec.SetEmail(id[:16] + "@apikey.local")
	rec.SetPassword(id)
	rec.Set("verified", true)
	rec.Set("label", label)
	if len(roleIds) > 0 {
		rec.Set("roles", roleIds)
	}
	if err := app.Save(rec); err != nil {
		return "", err
	}
	return rec.Id, nil
}

// migrateAPIKeysToServiceAccounts backfills a service account (defaulting to the
// admin role for backward-compatible full access) for any pre-existing key that
// doesn't have one yet. Idempotent.
func migrateAPIKeysToServiceAccounts(app core.App) {
	admin, err := app.FindFirstRecordByFilter(roleCollection, "name = 'admin'")
	if err != nil {
		return
	}
	keys, err := app.FindAllRecords(apiKeyCollection)
	if err != nil {
		return
	}
	for _, k := range keys {
		if k.GetString("serviceAccountId") != "" {
			continue
		}
		said, err := createServiceAccount(app, k.GetString("name"), []string{admin.Id})
		if err != nil {
			app.Logger().Error("apikey->SA migration: create failed", "key", k.Id, "err", err)
			continue
		}
		k.Set("serviceAccountId", said)
		if err := app.Save(k); err != nil {
			app.Logger().Error("apikey->SA migration: link failed", "key", k.Id, "err", err)
		}
	}
}
