package main

import (
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// newTestApp spins up a real PocketBase app (cloned test data + migrations) so we
// can exercise the schema-mutating RBAC setup against an actual DB.
func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	t.Cleanup(func() { app.Cleanup() })
	return app
}

// ensureRBACStack brings up the four custom system collections in dependency
// order, exactly as main() does at boot.
func ensureRBACStack(t *testing.T, app core.App) {
	t.Helper()
	if err := ensurePermissionsCollection(app); err != nil {
		t.Fatalf("ensurePermissionsCollection: %v", err)
	}
	if err := ensureRolesCollection(app); err != nil {
		t.Fatalf("ensureRolesCollection: %v", err)
	}
	if err := ensureServiceAccountsCollection(app); err != nil {
		t.Fatalf("ensureServiceAccountsCollection: %v", err)
	}
	if err := ensureAPIKeyCollection(app); err != nil {
		t.Fatalf("ensureAPIKeyCollection: %v", err)
	}
}

func TestEnsureRBACStackCreatesCollections(t *testing.T) {
	app := newTestApp(t)
	ensureRBACStack(t, app)

	// All four exist and are locked system collections.
	for _, name := range []string{permissionCollection, roleCollection, serviceAccountCollection, apiKeyCollection} {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatalf("collection %s missing: %v", name, err)
		}
		if !c.System {
			t.Errorf("collection %s should be System=true", name)
		}
	}

	// _roles.permissions must be a relation to _permissions (the normalized model).
	roles, _ := app.FindCollectionByNameOrId(roleCollection)
	perms, _ := app.FindCollectionByNameOrId(permissionCollection)
	rel, ok := roles.Fields.GetByName("permissions").(*core.RelationField)
	if !ok {
		t.Fatal("_roles.permissions is not a RelationField")
	}
	if rel.CollectionId != perms.Id {
		t.Errorf("_roles.permissions targets %q, want _permissions (%q)", rel.CollectionId, perms.Id)
	}

	// _serviceAccounts must be an auth collection carrying a roles relation.
	sa, _ := app.FindCollectionByNameOrId(serviceAccountCollection)
	if !sa.IsAuth() {
		t.Error("_serviceAccounts must be an auth collection")
	}
	if _, ok := sa.Fields.GetByName("roles").(*core.RelationField); !ok {
		t.Error("_serviceAccounts.roles is not a RelationField")
	}

	// _apiKeys must carry the hash + scopes + serviceAccountId fields.
	keys, _ := app.FindCollectionByNameOrId(apiKeyCollection)
	for _, f := range []string{"hash", "scopes", "serviceAccountId", "superuserId"} {
		if keys.Fields.GetByName(f) == nil {
			t.Errorf("_apiKeys missing field %q", f)
		}
	}
}

// Running the whole setup twice must be a no-op, not an error or a duplicate.
func TestEnsureRBACStackIdempotent(t *testing.T) {
	app := newTestApp(t)
	ensureRBACStack(t, app)
	ensureRBACStack(t, app) // second pass must not error
}

func TestMigrateAndSeedRBAC(t *testing.T) {
	app := newTestApp(t)
	ensureRBACStack(t, app)
	migrateAndSeedRBAC(app)

	n, err := app.CountRecords(roleCollection)
	if err != nil {
		t.Fatalf("count roles: %v", err)
	}
	if n != 3 {
		t.Fatalf("seeded role count = %d, want 3 (viewer/editor/admin)", n)
	}

	// The three default role names are present.
	for _, name := range []string{"viewer", "editor", "admin"} {
		if _, err := app.FindFirstRecordByFilter(roleCollection, "name = {:n}", dbx.Params{"n": name}); err != nil {
			t.Errorf("default role %q missing", name)
		}
	}

	// admin holds the global "*" token.
	admin, err := app.FindFirstRecordByFilter(roleCollection, "name = 'admin'")
	if err != nil {
		t.Fatalf("admin role: %v", err)
	}
	if toks := roleTokens(app, admin); len(toks) != 1 || toks[0] != "*" {
		t.Errorf("admin tokens = %v, want [*]", toks)
	}

	// Idempotent: a second seed pass must not duplicate roles.
	migrateAndSeedRBAC(app)
	if n2, _ := app.CountRecords(roleCollection); n2 != 3 {
		t.Errorf("role count after second seed = %d, want 3", n2)
	}
}

// backfillRBAC must auto-create CRUD tokens AND apply the native rules for a
// pre-existing, ungoverned (nil-rule) collection.
func TestBackfillGovernsNewCollection(t *testing.T) {
	app := newTestApp(t)
	ensureRBACStack(t, app)

	col := core.NewBaseCollection("widgets")
	col.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(col); err != nil {
		t.Fatalf("create widgets: %v", err)
	}

	backfillRBAC(app)

	// The four widgets:* tokens now exist.
	for _, tok := range permissionTokens("widgets") {
		if _, err := app.FindFirstRecordByFilter(permissionCollection, "token = {:t}", dbx.Params{"t": tok}); err != nil {
			t.Errorf("token %q not created by backfill", tok)
		}
	}

	// The native rules are applied (read rule references the widgets:read token).
	fresh, _ := app.FindCollectionByNameOrId("widgets")
	if fresh.ListRule == nil {
		t.Fatal("widgets.ListRule is nil — backfill did not apply rules")
	}
	if !strings.Contains(*fresh.ListRule, `widgets:read`) {
		t.Errorf("widgets.ListRule = %q, want it to reference widgets:read", *fresh.ListRule)
	}
}

// Dropping a collection's tokens must remove exactly its <name>:* records.
func TestRemovePermissionsForCollection(t *testing.T) {
	app := newTestApp(t)
	ensureRBACStack(t, app)

	col := core.NewBaseCollection("gadgets")
	if err := app.Save(col); err != nil {
		t.Fatalf("create gadgets: %v", err)
	}
	syncPermissionsForCollection(app, col)
	// Sanity: tokens exist before removal.
	if _, err := app.FindFirstRecordByFilter(permissionCollection, "token = 'gadgets:read'"); err != nil {
		t.Fatalf("expected gadgets:read to exist before removal: %v", err)
	}

	removePermissionsForCollection(app, col)

	for _, tok := range permissionTokens("gadgets") {
		if _, err := app.FindFirstRecordByFilter(permissionCollection, "token = {:t}", dbx.Params{"t": tok}); err == nil {
			t.Errorf("token %q should have been deleted", tok)
		}
	}
}

// createServiceAccount must produce a roled, auth-collection identity — the thing
// an API key acts AS on the data plane.
func TestCreateServiceAccount(t *testing.T) {
	app := newTestApp(t)
	ensureRBACStack(t, app)
	migrateAndSeedRBAC(app)

	admin, err := app.FindFirstRecordByFilter(roleCollection, "name = 'admin'")
	if err != nil {
		t.Fatalf("admin role: %v", err)
	}

	said, err := createServiceAccount(app, "test-key", []string{admin.Id})
	if err != nil {
		t.Fatalf("createServiceAccount: %v", err)
	}
	if said == "" {
		t.Fatal("createServiceAccount returned empty id")
	}

	sa, err := app.FindRecordById(serviceAccountCollection, said)
	if err != nil {
		t.Fatalf("fetch service account: %v", err)
	}
	if got := sa.GetStringSlice("roles"); len(got) != 1 || got[0] != admin.Id {
		t.Errorf("service account roles = %v, want [%s]", got, admin.Id)
	}
	if sa.Email() == "" {
		t.Error("service account should have a (synthetic) email set")
	}
	if sa.GetString("label") != "test-key" {
		t.Errorf("label = %q, want test-key", sa.GetString("label"))
	}
}
