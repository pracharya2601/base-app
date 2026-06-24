package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// HTTP-level tests for apiKeyAuthMiddleware — the actual request path that splits
// the control plane (scope-gated, acts as the minting superuser) from the data
// plane (RBAC-gated, acts as the key's roled service account). These are the
// crown-jewel auth behaviors, so we drive them end-to-end through a real router
// using fixed, known plaintext keys.

// Fixed plaintext keys. The middleware looks keys up by sha256hex(raw), so any
// stable string works; we just need to know the value to send in the header.
const (
	rawAdmin   = "rawkey-admin-00000001"   // scopes: admin; role: super (*)
	rawReader  = "rawkey-reader-00000001"  // scopes: records:read; role: reader (memos:read)
	rawNoRole  = "rawkey-norole-00000001"  // scopes: admin; NO roles
	rawAIOnly  = "rawkey-aionly-00000001"  // scopes: ai:use only; NO roles
	rawExpired = "rawkey-expired-00000001" // scopes: admin; role super; already expired
	rawRevoked = "rawkey-revoked-00000001" // scopes: admin; role super; revoked
	rawUnknown = "rawkey-not-in-database"  // never stored
)

// seedKey writes an _apiKeys record (and its backing service account) with a
// known plaintext, mirroring what the mint endpoint does.
func seedKey(t testing.TB, app core.App, suID, raw string, scopes, roleIds []string, revoked bool, expiresUnix int64) {
	t.Helper()
	saID, err := createServiceAccount(app, "k-"+raw, roleIds)
	if err != nil {
		t.Fatalf("createServiceAccount(%s): %v", raw, err)
	}
	col, err := app.FindCollectionByNameOrId(apiKeyCollection)
	if err != nil {
		t.Fatalf("find _apiKeys: %v", err)
	}
	rec := core.NewRecord(col)
	rec.Set("name", "k-"+raw)
	rec.Set("prefix", raw[:min(14, len(raw))])
	rec.Set("hash", sha256hex(raw))
	rec.Set("scopes", strings.Join(scopes, " "))
	rec.Set("revoked", revoked)
	rec.Set("superuserId", suID)
	rec.Set("serviceAccountId", saID)
	if expiresUnix != 0 {
		rec.Set("expiresUnix", expiresUnix)
	}
	if err := app.Save(rec); err != nil {
		t.Fatalf("save key %s: %v", raw, err)
	}
}

// setupMiddlewareApp builds a fully self-contained fixture: the RBAC stack, a
// superuser, an RBAC-governed "memos" collection with one record, two roles
// (reader -> memos:read, super -> *), and the six keys above.
func setupMiddlewareApp(t testing.TB) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
	// NOTE: cleanup is handled by ApiScenario (it owns the app once returned).

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

	// A real superuser for the control plane to act as.
	suCol, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatalf("find superusers: %v", err)
	}
	su := core.NewRecord(suCol)
	su.SetEmail("middleware-test@example.com")
	su.SetPassword("Test1234567!")
	if err := app.Save(su); err != nil {
		t.Fatalf("save superuser: %v", err)
	}

	// Permission tokens + roles (self-contained; not relying on seed defaults).
	permsCol, _ := app.FindCollectionByNameOrId(permissionCollection)
	mkPerm := func(tok string) string {
		r := core.NewRecord(permsCol)
		r.Set("token", tok)
		if err := app.Save(r); err != nil {
			t.Fatalf("save perm %s: %v", tok, err)
		}
		return r.Id
	}
	memosRead := mkPerm("memos:read")
	star := mkPerm("*")

	rolesCol, _ := app.FindCollectionByNameOrId(roleCollection)
	mkRole := func(name string, perms []string) string {
		r := core.NewRecord(rolesCol)
		r.Set("name", name)
		r.Set("permissions", perms)
		if err := app.Save(r); err != nil {
			t.Fatalf("save role %s: %v", name, err)
		}
		return r.Id
	}
	readerRole := mkRole("reader", []string{memosRead})
	superRole := mkRole("super", []string{star})

	// An RBAC-governed collection with one record. Rules applied explicitly since
	// the create hook isn't registered in this harness.
	memos := core.NewBaseCollection("memos")
	memos.Fields.Add(&core.TextField{Name: "title"})
	applyRules(memos, rbacRules("memos"))
	if err := app.Save(memos); err != nil {
		t.Fatalf("save memos: %v", err)
	}
	memo := core.NewRecord(memos)
	memo.Set("title", "memo-one")
	if err := app.Save(memo); err != nil {
		t.Fatalf("save memo record: %v", err)
	}

	// The six keys.
	seedKey(t, app, su.Id, rawAdmin, []string{"admin"}, []string{superRole}, false, 0)
	seedKey(t, app, su.Id, rawReader, []string{"records:read"}, []string{readerRole}, false, 0)
	seedKey(t, app, su.Id, rawNoRole, []string{"admin"}, nil, false, 0)
	seedKey(t, app, su.Id, rawAIOnly, []string{"ai:use"}, nil, false, 0)
	seedKey(t, app, su.Id, rawExpired, []string{"admin"}, []string{superRole}, false, time.Now().Add(-time.Hour).Unix())
	seedKey(t, app, su.Id, rawRevoked, []string{"admin"}, []string{superRole}, true, 0)

	return app
}

// scenario pre-wires an ApiScenario with the fixture factory and binds the real
// middleware + control-plane routes onto the serve event, exactly as main()'s
// OnServe does.
func scenario(sc tests.ApiScenario) tests.ApiScenario {
	sc.TestAppFactory = func(t testing.TB) *tests.TestApp { return setupMiddlewareApp(t) }
	sc.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		e.Router.Bind(apiKeyAuthMiddleware(app))
		registerAPIKeyRoutes(e, app)
	}
	return sc
}

func key(raw string) map[string]string { return map[string]string{"X-API-Key": raw} }

// ---- Control plane: scope gate, acts as the minting superuser ----

func TestMiddleware_ControlPlane(t *testing.T) {
	scenarios := []tests.ApiScenario{
		{
			Name:            "guest (no key) is rejected by RequireSuperuserAuth",
			Method:          http.MethodGet,
			URL:             "/api/superadmin/apikeys",
			ExpectedStatus:  401,
			ExpectedContent: []string{"authorization token"},
		},
		{
			Name:            "admin-scope key is admitted and acts as superuser",
			Method:          http.MethodGet,
			URL:             "/api/superadmin/apikeys",
			Headers:         key(rawAdmin),
			ExpectedStatus:  200,
			ExpectedContent: []string{"apiKeys"},
		},
		{
			Name:            "key missing the required scope is forbidden",
			Method:          http.MethodGet,
			URL:             "/api/superadmin/apikeys", // needs keys:manage; ai:use key lacks it
			Headers:         key(rawAIOnly),
			ExpectedStatus:  403,
			ExpectedContent: []string{"missing required scope", "keys:manage"},
		},
		{
			Name:            "unknown key is unauthorized",
			Method:          http.MethodGet,
			URL:             "/api/superadmin/apikeys",
			Headers:         key(rawUnknown),
			ExpectedStatus:  401,
			ExpectedContent: []string{"Invalid API key"},
		},
		{
			Name:            "revoked key is unauthorized (filtered out)",
			Method:          http.MethodGet,
			URL:             "/api/superadmin/apikeys",
			Headers:         key(rawRevoked),
			ExpectedStatus:  401,
			ExpectedContent: []string{"Invalid API key"},
		},
		{
			Name:            "expired key is unauthorized",
			Method:          http.MethodGet,
			URL:             "/api/superadmin/apikeys",
			Headers:         key(rawExpired),
			ExpectedStatus:  401,
			ExpectedContent: []string{"API key expired"},
		},
	}
	for _, sc := range scenarios {
		built := scenario(sc)
		t.Run(sc.Name, func(t *testing.T) { built.Test(t) })
	}
}

// ---- Data plane: RBAC gate, acts as the key's roled service account ----
// The crucial property: scope does NOT grant data access — the key's ROLE does.

func TestMiddleware_DataPlane(t *testing.T) {
	scenarios := []tests.ApiScenario{
		{
			Name:            "reader-role key can list the RBAC collection",
			Method:          http.MethodGet,
			URL:             "/api/collections/memos/records",
			Headers:         key(rawReader),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":1`, "memo-one"},
		},
		{
			Name:            "super-role key can list (holds the * token)",
			Method:          http.MethodGet,
			URL:             "/api/collections/memos/records",
			Headers:         key(rawAdmin),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":1`, "memo-one"},
		},
		{
			Name:            "admin-SCOPE but NO-role key cannot read data (no superuser bypass)",
			Method:          http.MethodGet,
			URL:             "/api/collections/memos/records",
			Headers:         key(rawNoRole),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":0`},
		},
		{
			Name:            "guest cannot read the RBAC collection",
			Method:          http.MethodGet,
			URL:             "/api/collections/memos/records",
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":0`},
		},
	}
	for _, sc := range scenarios {
		built := scenario(sc)
		t.Run(sc.Name, func(t *testing.T) { built.Test(t) })
	}
}
