package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"base-app/internal/adminui"
	"base-app/internal/ai"
	"base-app/internal/bookings"
	"base-app/internal/ops"
	"base-app/internal/orchestrator"
	"base-app/internal/provision"
	"base-app/internal/support"
)

// encryptionKeyStatus classifies PB_ENCRYPTION_KEY for the boot-time security
// guard. ok=true means at-rest encryption is active for BOTH PocketBase's settings
// blob (via --encryptionEnv) and the AI proxy's stored provider keys (which encrypt
// themselves with the same key). Otherwise reason explains why those secrets fall
// back to PLAINTEXT — local/dev still boots, but production must not run this way.
func encryptionKeyStatus(key string) (ok bool, reason string) {
	switch {
	case key == "":
		return false, "PB_ENCRYPTION_KEY is unset"
	case len(key) != 32:
		return false, fmt.Sprintf("PB_ENCRYPTION_KEY must be exactly 32 chars (AES-256); got %d", len(key))
	default:
		return true, ""
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
		// Security guard: surface the at-rest encryption posture once at boot. Both the
		// settings blob and the AI proxy's stored provider keys silently fall back to
		// plaintext without a valid PB_ENCRYPTION_KEY, so make that loud rather than
		// letting prod ship secrets unencrypted unnoticed.
		if ok, reason := encryptionKeyStatus(os.Getenv("PB_ENCRYPTION_KEY")); ok {
			app.Logger().Info("security: at-rest encryption ACTIVE (settings blob + AI provider keys)")
		} else {
			app.Logger().Warn("security: at-rest encryption DISABLED — secrets stored in PLAINTEXT",
				"reason", reason,
				"impact", "PocketBase settings (SMTP/S3 creds) and stored AI provider API keys are not encrypted at rest")
		}

		// API-key system: ensure storage, register the auth middleware (runs
		// after the JWT loader), and wire the mint/list/revoke routes.
		if err := ensureAPIKeyCollection(app); err != nil {
			return err
		}
		se.Router.Bind(apiKeyAuthMiddleware(app))
		registerAPIKeyRoutes(se, app)
		adminui.RegisterKeys(se) // GET /admin/apikeys — browser UI for minting keys

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

		// AI proxy / orchestrator (rungs 0-2, see docs/AI-PROXY.md): ensure the
		// _aiProviders (encrypted keys) + _aiUsage (metering) system collections,
		// then wire /api/ai/{provider}/generate + /stream (JWT auth) and the
		// superuser provider-management routes.
		if err := ai.EnsureProvidersCollection(app); err != nil {
			return err
		}
		if err := ai.EnsureUsageCollection(app); err != nil {
			return err
		}
		if err := ai.EnsureImagesCollection(app); err != nil {
			return err
		}
		ai.RegisterRoutes(se, app)
		ai.RegisterImageRoutes(se, app)
		adminui.RegisterAIUI(se)  // GET /admin/ai — standalone provider-keys UI (back-compat)
		adminui.RegisterAdmin(se) // GET /admin — unified console (API keys + AI providers + test)

		registerProvisionRoutes(se, app)

		// Orchestrator (the "AI company"): ensure the _agents/_tasks/_runs schema,
		// seed the default software team, wire the human-operator routes, and start
		// the always-on loop. Agents draft work; humans approve to advance.
		if err := orchestrator.EnsureSchema(app); err != nil {
			return err
		}
		orchestrator.SeedTeam(app)
		orchestrator.RegisterRoutes(se, app)

		// Customer support: the first real "company function" running on the
		// orchestrator engine. A new support_tickets row auto-drafts a reply; a
		// human approves to send. Self-contained in internal/support, plugged in
		// via EnqueueTask + RegisterApproveAction (see that package).
		if err := support.EnsureSchema(app); err != nil {
			return err
		}
		support.Register(app)

		// Bookings: a SECOND company function on the same engine — proof the
		// template generalizes (new package + these two lines, zero engine edits).
		if err := bookings.EnsureSchema(app); err != nil {
			return err
		}
		bookings.Register(app)

		// Ops: the agentic ADMIN function — state intent in /admin, the ops agent
		// proposes a platform change (schema, …), a human approves to apply it. The
		// step toward running everything from /admin via the agentic system.
		ops.Register(app)

		orchestrator.Start(app)

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// registerProvisionRoutes wires the public field-type catalog and the
// superuser-only one-call provisioning endpoint (create collections + add
// fields + set rules + seed records + appName, all idempotent).
func registerProvisionRoutes(se *core.ServeEvent, app core.App) {
	// Self-describing field-type catalog. PUBLIC (pure capability metadata):
	// the frontend, the MCP server, and humans all read this to know what
	// the provision endpoint can build. This is the shared contract.
	se.Router.GET("/api/superadmin/field-types", func(e *core.RequestEvent) error {
		return e.JSON(http.StatusOK, map[string]any{"fieldTypes": fieldTypeCatalog()})
	})

	// One-call provisioning: create collections + seed records + set appName.
	// Guarded so ONLY a _superusers token can call it. The idempotent core lives in
	// internal/provision (shared with the agentic ops executor); this is the thin
	// HTTP wrapper that maps spec errors -> 400 and persistence failures -> 500.
	se.Router.POST("/api/superadmin/provision", func(e *core.RequestEvent) error {
		req := provision.Spec{}
		if err := e.BindBody(&req); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		res, err := provision.Apply(app, req)
		if err != nil {
			var bad *provision.InvalidSpecError
			if errors.As(err, &bad) {
				return e.BadRequestError(err.Error(), err)
			}
			return e.InternalServerError(err.Error(), err)
		}
		return e.JSON(http.StatusOK, res)
	}).Bind(apis.RequireSuperuserAuth())
}
