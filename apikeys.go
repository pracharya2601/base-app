package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// A real API-key system on top of PocketBase (which has none natively):
//   - keys are random "pbk_<48 hex>" strings shown ONCE at creation
//   - only a SHA-256 hash is stored, never the plaintext
//   - each key is named, scoped, individually revocable, and listable
//   - a middleware accepts "X-API-Key" on any endpoint, enforces the key's
//     scopes, and (if allowed) authenticates the request as the minting superuser.

// apiKeyCollection is a SYSTEM collection (underscore-prefixed by convention,
// like _superusers). System = true protects it from rename/delete/flag changes.
const apiKeyCollection = "_apiKeys"

// knownScopes is the closed set of grantable scopes. Editing this (code, not
// schema) is how you add new scopes — no migration of the locked collection.
var knownScopes = map[string]string{
	"admin":         "Full access (everything a superuser can do).",
	"schema:read":   "Read collections and the field-type catalog.",
	"schema:write":  "Create/modify collections and fields (provision).",
	"records:read":  "Read records in any collection.",
	"records:write": "Create/update/delete records.",
	"settings:read":  "Read application settings.",
	"settings:write": "Modify application settings (SMTP, S3, backups, etc.).",
	"keys:manage":   "Mint, list, and revoke API keys.",
	"roles:read":    "Read roles and their permissions.",
	"roles:write":   "Create/modify roles, grant permissions, assign user roles.",
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newRawKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "pbk_" + hex.EncodeToString(b), nil // pbk_ + 48 hex chars
}

// requiredScope maps an HTTP request to the single scope needed to perform it.
// Order matters: record paths are checked before the generic collections path.
func requiredScope(method, path string) string {
	switch {
	case strings.HasPrefix(path, "/api/superadmin/apikeys"):
		return "keys:manage"
	case strings.HasPrefix(path, "/api/superadmin/provision"):
		return "schema:write"
	case strings.HasPrefix(path, "/api/superadmin/roles"),
		strings.HasPrefix(path, "/api/superadmin/users/roles"):
		if method == "GET" {
			return "roles:read"
		}
		return "roles:write"
	case strings.HasPrefix(path, "/api/superadmin/field-types"):
		return "schema:read"
	case strings.HasPrefix(path, "/api/settings"):
		if method == "GET" {
			return "settings:read"
		}
		return "settings:write"
	case strings.Contains(path, "/records"):
		if method == "GET" {
			return "records:read"
		}
		return "records:write"
	case strings.HasPrefix(path, "/api/collections"):
		if method == "GET" {
			return "schema:read"
		}
		return "schema:write"
	default:
		return "admin" // anything unrecognized requires full admin
	}
}

// hasScope reports whether the granted scopes satisfy the required one.
// "admin" satisfies everything.
func hasScope(granted []string, required string) bool {
	for _, s := range granted {
		if s == "admin" || s == required {
			return true
		}
	}
	return false
}

// ensureAPIKeyCollection creates the protected system collection if missing,
// and migrates an existing one by adding the "scopes" field (allowed: it's a
// new NON-system field, which the system-collection guard permits).
func ensureAPIKeyCollection(app core.App) error {
	// One-time migration: drop any legacy NON-system "api_keys" collection.
	if old, err := app.FindCollectionByNameOrId("api_keys"); err == nil && !old.System {
		_ = app.Delete(old)
	}

	if existing, err := app.FindCollectionByNameOrId(apiKeyCollection); err == nil {
		changed := false
		if existing.Fields.GetByName("scopes") == nil {
			existing.Fields.Add(&core.TextField{Name: "scopes"})
			changed = true
		}
		if existing.Fields.GetByName("serviceAccountId") == nil {
			existing.Fields.Add(&core.TextField{Name: "serviceAccountId"})
			changed = true
		}
		if changed {
			return app.Save(existing)
		}
		return nil
	}

	col := core.NewBaseCollection(apiKeyCollection)
	col.System = true // set only at creation — locks the collection thereafter
	col.Fields.Add(&core.TextField{Name: "name", Required: true})
	col.Fields.Add(&core.TextField{Name: "prefix"})        // shown for identification
	col.Fields.Add(&core.TextField{Name: "hash", Required: true})
	col.Fields.Add(&core.TextField{Name: "scopes"})        // space-delimited scope tokens
	col.Fields.Add(&core.BoolField{Name: "revoked"})
	col.Fields.Add(&core.NumberField{Name: "expiresUnix"}) // 0 = never
	col.Fields.Add(&core.NumberField{Name: "lastUsedUnix"})
	col.Fields.Add(&core.TextField{Name: "superuserId"})      // superuser identity for control-plane routes
	col.Fields.Add(&core.TextField{Name: "serviceAccountId"}) // roled identity for data-plane (record) routes
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	return app.Save(col)
}

// apiKeyAuthMiddleware runs AFTER the built-in JWT loader (higher priority value)
// so the e.Auth it sets isn't overwritten. It only acts when an X-API-Key header
// is present and the request isn't already authenticated.
func apiKeyAuthMiddleware(app core.App) *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id:       "apiKeyAuth",
		Priority: apis.DefaultLoadAuthTokenMiddlewarePriority + 1,
		Func: func(e *core.RequestEvent) error {
			if e.Auth != nil {
				return e.Next() // already authenticated via a JWT
			}
			raw := e.Request.Header.Get("X-API-Key")
			if raw == "" {
				return e.Next() // no key -> proceed as guest
			}

			rec, err := app.FindFirstRecordByFilter(apiKeyCollection,
				"hash = {:h} && revoked = false", dbx.Params{"h": sha256hex(raw)})
			if err != nil {
				return e.UnauthorizedError("invalid API key", nil)
			}
			if exp := rec.GetInt("expiresUnix"); exp != 0 && time.Now().Unix() > int64(exp) {
				return e.UnauthorizedError("API key expired", nil)
			}

			// best-effort "last used" stamp
			rec.Set("lastUsedUnix", time.Now().Unix())
			_ = app.Save(rec)

			// DATA PLANE: on record routes the key acts AS its roled service
			// account, so the SAME native collection rules that gate users gate the
			// key. No scope check here — the key's roles are the gate.
			if saID := rec.GetString("serviceAccountId"); saID != "" && isRecordRoute(e.Request.URL.Path) {
				sa, err := app.FindRecordById(serviceAccountCollection, saID)
				if err != nil {
					return e.UnauthorizedError("API key service account no longer exists", nil)
				}
				e.Auth = sa
				return e.Next()
			}

			// CONTROL PLANE (schema/settings/key management), or a legacy key with
			// no service account: gate by scope and act as the minting superuser.
			granted := strings.Fields(rec.GetString("scopes"))
			needed := requiredScope(e.Request.Method, e.Request.URL.Path)
			if !hasScope(granted, needed) {
				return e.ForbiddenError("API key missing required scope: "+needed, nil)
			}
			su, err := app.FindRecordById(core.CollectionNameSuperusers, rec.GetString("superuserId"))
			if err != nil {
				return e.UnauthorizedError("API key identity no longer exists", nil)
			}
			e.Auth = su
			return e.Next()
		},
	}
}

// registerAPIKeyRoutes wires mint / list / revoke endpoints (superuser-only),
// plus a public scope catalog for discovery.
func registerAPIKeyRoutes(se *core.ServeEvent, app core.App) {
	// Public: list grantable scopes (so callers/LLM/frontend know what to request).
	se.Router.GET("/api/superadmin/scopes", func(e *core.RequestEvent) error {
		out := make([]map[string]string, 0, len(knownScopes))
		for s, d := range knownScopes {
			out = append(out, map[string]string{"scope": s, "description": d})
		}
		return e.JSON(200, map[string]any{"scopes": out})
	})

	// Mint a new key. The plaintext is returned ONCE and never stored.
	se.Router.POST("/api/superadmin/apikeys", func(e *core.RequestEvent) error {
		var body struct {
			Name          string   `json:"name"`
			ExpiresInDays int      `json:"expiresInDays"`
			Scopes        []string `json:"scopes"`
			Roles         []string `json:"roles"` // _roles record ids the key may use on data (record) routes
		}
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		if body.Name == "" {
			return e.BadRequestError("'name' is required", nil)
		}
		scopes := body.Scopes
		if len(scopes) == 0 {
			scopes = []string{"admin"} // control-plane default; data access is governed by roles below
		}
		for _, s := range scopes {
			if _, ok := knownScopes[s]; !ok {
				return e.BadRequestError("unknown scope: "+s, nil)
			}
		}
		// Validate role ids up front (empty roles => the key can't touch records).
		for _, rid := range body.Roles {
			if _, err := app.FindRecordById(roleCollection, rid); err != nil {
				return e.BadRequestError("unknown role id: "+rid, nil)
			}
		}
		raw, err := newRawKey()
		if err != nil {
			return e.InternalServerError("failed to generate key", err)
		}
		col, err := app.FindCollectionByNameOrId(apiKeyCollection)
		if err != nil {
			return e.InternalServerError("api key collection missing", err)
		}
		// The roled identity the key acts AS on record routes.
		saID, err := createServiceAccount(app, body.Name, body.Roles)
		if err != nil {
			return e.InternalServerError("failed to create service account", err)
		}
		rec := core.NewRecord(col)
		rec.Set("name", body.Name)
		rec.Set("prefix", raw[:14])
		rec.Set("hash", sha256hex(raw))
		rec.Set("scopes", strings.Join(scopes, " "))
		rec.Set("revoked", false)
		rec.Set("superuserId", e.Auth.Id)
		rec.Set("serviceAccountId", saID)
		if body.ExpiresInDays > 0 {
			rec.Set("expiresUnix", time.Now().Add(time.Duration(body.ExpiresInDays)*24*time.Hour).Unix())
		}
		if err := app.Save(rec); err != nil {
			return e.InternalServerError("failed to save key", err)
		}
		return e.JSON(201, map[string]any{
			"id":      rec.Id,
			"name":    body.Name,
			"prefix":  rec.GetString("prefix"),
			"scopes":  scopes,
			"roles":   body.Roles,
			"key":     raw, // shown only once
			"warning": "store this key now — it cannot be retrieved again",
		})
	}).Bind(apis.RequireSuperuserAuth())

	// List keys (metadata only, never the hash or plaintext).
	se.Router.GET("/api/superadmin/apikeys", func(e *core.RequestEvent) error {
		recs, err := app.FindAllRecords(apiKeyCollection)
		if err != nil {
			return e.InternalServerError("failed to list keys", err)
		}
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			// Resolve the key's role names via its service account (data-plane gate).
			roleNames := []string{}
			if saID := r.GetString("serviceAccountId"); saID != "" {
				if sa, err := app.FindRecordById(serviceAccountCollection, saID); err == nil {
					for _, rid := range sa.GetStringSlice("roles") {
						if rr, err := app.FindRecordById(roleCollection, rid); err == nil {
							roleNames = append(roleNames, rr.GetString("name"))
						}
					}
				}
			}
			out = append(out, map[string]any{
				"id":           r.Id,
				"name":         r.GetString("name"),
				"prefix":       r.GetString("prefix"),
				"scopes":       strings.Fields(r.GetString("scopes")),
				"roles":        roleNames,
				"revoked":      r.GetBool("revoked"),
				"expiresUnix":  r.GetInt("expiresUnix"),
				"lastUsedUnix": r.GetInt("lastUsedUnix"),
			})
		}
		return e.JSON(200, map[string]any{"apiKeys": out})
	}).Bind(apis.RequireSuperuserAuth())

	// Revoke a key instantly (soft delete so it stays auditable).
	se.Router.DELETE("/api/superadmin/apikeys/{id}", func(e *core.RequestEvent) error {
		id := e.Request.PathValue("id")
		rec, err := app.FindRecordById(apiKeyCollection, id)
		if err != nil {
			return e.NotFoundError("key not found", err)
		}
		rec.Set("revoked", true)
		if err := app.Save(rec); err != nil {
			return e.InternalServerError("failed to revoke key", err)
		}
		return e.JSON(200, map[string]any{"id": id, "revoked": true})
	}).Bind(apis.RequireSuperuserAuth())
}
