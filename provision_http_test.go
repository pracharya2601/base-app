package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// HTTP-level tests for the provision endpoint (POST /api/superadmin/provision),
// driven through the real router + apiKeyAuthMiddleware exactly as main() wires
// it. Auth is exercised via API keys (an admin-scope key acts as superuser on the
// control plane; provision requires the schema:write scope).

// setupProvisionApp builds the RBAC stack, a superuser, an admin key + an
// ai:use-only key, and a pre-existing "widgets" collection (so the idempotency /
// add-field branches can be hit by a single request).
func setupProvisionApp(t testing.TB) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("NewTestApp: %v", err)
	}
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

	suCol, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatalf("find superusers: %v", err)
	}
	su := core.NewRecord(suCol)
	su.SetEmail("provision-test@example.com")
	su.SetPassword("Test1234567!")
	if err := app.Save(su); err != nil {
		t.Fatalf("save superuser: %v", err)
	}

	seedKey(t, app, su.Id, rawAdmin, []string{"admin"}, nil, false, 0)
	seedKey(t, app, su.Id, rawAIOnly, []string{"ai:use"}, nil, false, 0)

	// A pre-existing collection so the "exists" / "add field" branches are
	// reachable in a single provision request.
	widgets := core.NewBaseCollection("widgets")
	widgets.Fields.Add(&core.TextField{Name: "title"})
	widgets.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	if err := app.Save(widgets); err != nil {
		t.Fatalf("pre-create widgets: %v", err)
	}

	return app
}

// provisionScenario pre-wires a scenario with the provision fixture + middleware.
func provisionScenario(sc tests.ApiScenario) tests.ApiScenario {
	sc.TestAppFactory = func(t testing.TB) *tests.TestApp { return setupProvisionApp(t) }
	sc.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		e.Router.Bind(apiKeyAuthMiddleware(app))
		registerProvisionRoutes(e, app)
	}
	return sc
}

func body(s string) *strings.Reader { return strings.NewReader(s) }

func runProvision(t *testing.T, scenarios []tests.ApiScenario) {
	for _, sc := range scenarios {
		built := provisionScenario(sc)
		t.Run(sc.Name, func(t *testing.T) { built.Test(t) })
	}
}

// ---- Auth gate ----

func TestProvision_AuthGate(t *testing.T) {
	runProvision(t, []tests.ApiScenario{
		{
			Name:            "guest is rejected",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Body:            body(`{}`),
			ExpectedStatus:  401,
			ExpectedContent: []string{"authorization token"},
		},
		{
			Name:            "key without schema:write scope is forbidden",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAIOnly),
			Body:            body(`{}`),
			ExpectedStatus:  403,
			ExpectedContent: []string{"missing required scope", "schema:write"},
		},
		{
			Name:            "admin-scope key is admitted (empty provision is a no-op)",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"collectionsCreated":[]`},
		},
	})
}

// ---- Collection create / idempotency / field merge ----

func TestProvision_Collections(t *testing.T) {
	runProvision(t, []tests.ApiScenario{
		{
			Name:            "create a new collection with fields",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"books","fields":[{"name":"title","type":"text"},{"name":"pages","type":"number"}]}]}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"collectionsCreated":["books"]`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, _ *http.Response) {
				col, err := app.FindCollectionByNameOrId("books")
				if err != nil {
					t.Fatalf("books not created: %v", err)
				}
				for _, f := range []string{"title", "pages", "created"} {
					if col.Fields.GetByName(f) == nil {
						t.Errorf("books missing field %q", f)
					}
				}
			},
		},
		{
			Name:            "re-provisioning an existing collection is idempotent",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"widgets","fields":[{"name":"title","type":"text"}]}]}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"collectionsExisted":["widgets"]`, `"collectionsCreated":[]`},
		},
		{
			Name:            "new fields are merged into an existing collection",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"widgets","fields":[{"name":"title","type":"text"},{"name":"color","type":"text"}]}]}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"fieldsAdded":{"widgets":["color"]}`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, _ *http.Response) {
				col, _ := app.FindCollectionByNameOrId("widgets")
				if col.Fields.GetByName("color") == nil {
					t.Error("widgets.color was not added")
				}
			},
		},
		{
			Name:            "relation field resolves its target collection",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"links","fields":[{"name":"widget","type":"relation","collection":"widgets"}]}]}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"collectionsCreated":["links"]`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, _ *http.Response) {
				links, err := app.FindCollectionByNameOrId("links")
				if err != nil {
					t.Fatalf("links not created: %v", err)
				}
				widgets, _ := app.FindCollectionByNameOrId("widgets")
				rel, ok := links.Fields.GetByName("widget").(*core.RelationField)
				if !ok {
					t.Fatal("links.widget is not a RelationField")
				}
				if rel.CollectionId != widgets.Id {
					t.Errorf("links.widget targets %q, want widgets (%q)", rel.CollectionId, widgets.Id)
				}
			},
		},
		{
			Name:            "unsupported field type is a 400",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"bad","fields":[{"name":"f","type":"nope"}]}]}`),
			ExpectedStatus:  400,
			ExpectedContent: []string{"Unsupported field type", "nope"},
		},
	})
}

// ---- rbac flag, seeding, settings ----

func TestProvision_RBACSeedSettings(t *testing.T) {
	runProvision(t, []tests.ApiScenario{
		{
			Name:            "rbac:true writes native role-permission rules",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"posts","rbac":true,"fields":[{"name":"body","type":"text"}]}]}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"collectionsCreated":["posts"]`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, _ *http.Response) {
				posts, err := app.FindCollectionByNameOrId("posts")
				if err != nil {
					t.Fatalf("posts not created: %v", err)
				}
				if posts.ListRule == nil || !strings.Contains(*posts.ListRule, "posts:read") {
					t.Errorf("posts.ListRule = %v, want it to reference posts:read", posts.ListRule)
				}
				if posts.CreateRule == nil || !strings.Contains(*posts.CreateRule, "posts:create") {
					t.Errorf("posts.CreateRule = %v, want it to reference posts:create", posts.CreateRule)
				}
			},
		},
		{
			Name:            "records are seeded into a freshly created collection",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"collections":[{"name":"notes","fields":[{"name":"text","type":"text"}]}],"seed":{"notes":[{"text":"hi"},{"text":"yo"}]}}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"recordsSeeded":{"notes":2}`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, _ *http.Response) {
				n, err := app.CountRecords("notes")
				if err != nil {
					t.Fatalf("count notes: %v", err)
				}
				if n != 2 {
					t.Errorf("seeded notes = %d, want 2", n)
				}
			},
		},
		{
			Name:            "seeding a missing collection is a 400",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"seed":{"ghost":[{"x":1}]}}`),
			ExpectedStatus:  400,
			ExpectedContent: []string{"Seed target collection not found", "ghost"},
		},
		{
			Name:            "appName updates application settings",
			Method:          http.MethodPost,
			URL:             "/api/superadmin/provision",
			Headers:         key(rawAdmin),
			Body:            body(`{"appName":"Provisioned App"}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"appName":"Provisioned App"`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, _ *http.Response) {
				if got := app.Settings().Meta.AppName; got != "Provisioned App" {
					t.Errorf("AppName = %q, want %q", got, "Provisioned App")
				}
			},
		},
	})
}
