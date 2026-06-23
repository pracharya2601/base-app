// pb-mcp is an MCP server that exposes PocketBase superadmin operations as
// LLM-readable tools. Each tool carries a name + description + typed params so
// a model understands what the call means. It holds a superuser token and calls
// the REST endpoints (including our custom /api/superadmin/provision).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ---- thin PocketBase superuser client ----

type pbClient struct {
	base, email, password, token, apiKey string
	// staticToken means the token came from PB_TOKEN and there's no password
	// to auto-regenerate with — on expiry we must fail loudly for a re-mint.
	staticToken bool
	http        *http.Client
}

func (c *pbClient) auth() error {
	payload, _ := json.Marshal(map[string]string{"identity": c.email, "password": c.password})
	resp, err := c.http.Post(c.base+"/api/collections/_superusers/auth-with-password",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("superuser auth failed (%d): %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	c.token = out.Token
	return nil
}

// request calls PocketBase, auto-authenticating / retrying once on 401.
func (c *pbClient) request(method, path string, body []byte) ([]byte, int, error) {
	send := func() (*http.Response, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, c.base+path, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		// A scoped API key (preferred for a service client) takes precedence;
		// it goes in X-API-Key, not Authorization.
		if c.apiKey != "" {
			req.Header.Set("X-API-Key", c.apiKey)
		} else if c.token != "" {
			req.Header.Set("Authorization", c.token)
		}
		return c.http.Do(req)
	}
	resp, err := send()
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		switch {
		// An API key is invalid/revoked/expired — can't self-heal, fail loudly.
		case c.apiKey != "":
			return nil, 0, fmt.Errorf("PB_API_KEY is invalid, revoked, or expired: mint a new scoped key with scripts/mint-apikey.sh and update PB_API_KEY")
		// A static (impersonate) token can't be auto-regenerated without a
		// password — surface a clear, actionable error instead of silently failing.
		case c.staticToken:
			return nil, 0, fmt.Errorf("PB_TOKEN is expired or invalid: re-mint with scripts/mint-token.sh and update PB_TOKEN (or also set PB_EMAIL/PB_PASSWORD to auto-renew)")
		default:
			if err := c.auth(); err != nil {
				return nil, 0, err
			}
			if resp, err = send(); err != nil {
				return nil, 0, err
			}
		}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

func main() {
	pb := &pbClient{
		base:     envOr("PB_URL", "http://localhost:8090"),
		email:    os.Getenv("PB_EMAIL"),
		password: os.Getenv("PB_PASSWORD"),
		token:    os.Getenv("PB_TOKEN"),   // long-lived impersonate token
		apiKey:   os.Getenv("PB_API_KEY"), // preferred: a scoped, revocable API key (pbk_...)
		http:     &http.Client{Timeout: 15 * time.Second},
	}
	// Auth precedence (most to least preferred):
	//   PB_API_KEY set -> scoped key via X-API-Key (fail loudly if revoked/expired)
	//   PB_TOKEN set, no password -> static token (fail loudly on expiry)
	//   password set              -> auto-auth + auto-renew on 401
	//   nothing set               -> dev fallback to the demo superuser
	pb.staticToken = pb.token != "" && pb.password == ""
	if pb.apiKey == "" && pb.token == "" && pb.password == "" {
		pb.email, pb.password = "admin@example.com", "SuperSecret123"
	}

	s := server.NewMCPServer("pocketbase-superadmin", "1.0.0",
		server.WithToolCapabilities(true))

	// 1. Discover the field-type vocabulary (the shared contract).
	s.AddTool(
		mcp.NewTool("list_field_types",
			mcp.WithDescription("List every field type the schema-provisioning tool supports, "+
				"with each type's parameters and descriptions. Call this FIRST so you know "+
				"what kinds of fields (text, number, bool, email, select, relation) you can create.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, status, err := pb.request("GET", "/api/superadmin/field-types", nil)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// 2. Inspect the current schema.
	s.AddTool(
		mcp.NewTool("list_collections",
			mcp.WithDescription("List existing collections and their fields (the current database schema). "+
				"Use this to see what already exists before creating or modifying collections.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, status, err := pb.request("GET", "/api/collections?perPage=200", nil)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if status != 200 {
				return statusText(status, b), nil
			}
			return mcp.NewToolResultText(summarizeCollections(b)), nil
		})

	// 3. Provision schema (create collections / add fields / seed / appName).
	s.AddTool(
		mcp.NewTool("provision_schema",
			mcp.WithDescription("Create collections, add fields to existing collections, seed records, "+
				"and/or set the app name — all in one idempotent call. Safe to re-run. "+
				"The 'spec' argument is a JSON object string of the form:\n"+
				`{"appName":"...","collections":[{"name":"posts","fields":[`+
				`{"name":"title","type":"text","required":true},`+
				`{"name":"status","type":"select","values":["draft","live"]},`+
				`{"name":"author","type":"relation","collection":"users","cascadeDelete":true}]}],`+
				`"seed":{"posts":[{"title":"hello","status":"draft"}]}}`+"\n"+
				"Each collection may also carry an optional \"rules\" object to set PocketBase "+
				"access rules (RBAC) as filter strings: "+
				`{"name":"posts","fields":[...],"rules":{"list":"@request.auth.id != \"\"",`+
				`"create":"@request.auth.roles.name ?= \"editor\"","delete":"@request.auth.roles.name ?= \"admin\""}}`+". "+
				"Omit a rule to leave it unchanged (superuser-only on new collections); set it to \"\" for public. "+
				"Alternatively set \"rbac\":true on a collection to auto-generate role-permission rules "+
				"(it wires the native rules that check @request.auth.roles.permissions.token against "+
				"\"<collection>:<action>\", \"<collection>:*\", and \"*\"). "+
				"List target collections of a relation BEFORE the collections that reference them. "+
				"Use list_field_types for the exact field params."),
			mcp.WithString("spec", mcp.Required(),
				mcp.Description("A JSON object string describing collections, fields, seed records, and optional appName."))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			spec := req.GetString("spec", "")
			if !json.Valid([]byte(spec)) {
				return mcp.NewToolResultError("'spec' is not valid JSON"), nil
			}
			b, status, err := pb.request("POST", "/api/superadmin/provision", []byte(spec))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// 4. Create a record in any collection.
	s.AddTool(
		mcp.NewTool("create_record",
			mcp.WithDescription("Create a single record in a collection. 'data' is a JSON object string "+
				"whose keys are field names. For relation fields, the value is the linked record id."),
			mcp.WithString("collection", mcp.Required(), mcp.Description("Target collection name.")),
			mcp.WithString("data", mcp.Required(), mcp.Description("JSON object string of field values."))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			col := req.GetString("collection", "")
			data := req.GetString("data", "")
			if col == "" || !json.Valid([]byte(data)) {
				return mcp.NewToolResultError("need a 'collection' and a valid JSON 'data' object"), nil
			}
			b, status, err := pb.request("POST", "/api/collections/"+col+"/records", []byte(data))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// 5. Read the application settings (the admin "Settings" page).
	s.AddTool(
		mcp.NewTool("get_settings",
			mcp.WithDescription("Get the PocketBase application settings (the admin Settings page): "+
				"app name/URL (meta), SMTP/mail (smtp), S3 file storage (s3), backups, logs, rate limits. "+
				"Secret values like passwords and access keys are returned masked/empty. "+
				"Call this before update_settings to see the structure.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, status, err := pb.request("GET", "/api/settings", nil)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// 6. Update the application settings (partial merge).
	s.AddTool(
		mcp.NewTool("update_settings",
			mcp.WithDescription("Update application settings. 'patch' is a JSON object string containing ONLY "+
				"the keys to change — it is merged into the existing settings. Examples:\n"+
				`{"meta":{"appName":"Acme","appURL":"https://acme.test"}}`+"\n"+
				`{"smtp":{"enabled":true,"host":"smtp.example.com","port":587,"username":"u","password":"p","tls":true}}`+"\n"+
				`{"backups":{"cron":"0 0 * * *","cronMaxKeep":7}}`),
			mcp.WithString("patch", mcp.Required(),
				mcp.Description("JSON object string of the settings keys to merge/update."))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			patch := req.GetString("patch", "")
			if !json.Valid([]byte(patch)) {
				return mcp.NewToolResultError("'patch' is not valid JSON"), nil
			}
			b, status, err := pb.request("PATCH", "/api/settings", []byte(patch))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// ---- RBAC management (agentic; ask the user before policy decisions) ----

	// 7. Inspect the access model.
	s.AddTool(
		mcp.NewTool("list_roles",
			mcp.WithDescription("List every role and the permission tokens it grants. Tokens are "+
				"\"<collection>:<action>\" (actions: read, create, update, delete), the per-collection "+
				"wildcard \"<collection>:*\", or the global \"*\". A user or API key holds one or more "+
				"roles and gets the UNION of their permissions. Call this (with list_collections) FIRST "+
				"to understand the current access model before changing anything.")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, status, err := pb.request("GET", "/api/superadmin/roles", nil)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// 8. Create/update a role and set its permissions.
	s.AddTool(
		mcp.NewTool("manage_role",
			mcp.WithDescription("Create or update a role (by name) and set its FULL list of permission "+
				"tokens (this REPLACES the role's current permissions). Missing token records are created "+
				"automatically. Tokens: \"<collection>:<action>\", \"<collection>:*\", or \"*\".\n"+
				"⚠️ POLICY DECISION — DO NOT GUESS. This changes who can read/write data. Before granting "+
				"anything the user did not explicitly request — and ALWAYS before broad grants (\"*\", "+
				"\"<collection>:*\", or write/delete on sensitive data) — ASK THE USER to confirm the exact "+
				"collections and actions. If the request is vague (e.g. \"let viewers see everything\"), ASK "+
				"whether they mean all collections or specific ones, and read-only or write. Use list_roles "+
				"first so you replace, not erase, existing grants."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Role name, e.g. \"viewer\", \"billing-bot\".")),
			mcp.WithString("permissions", mcp.Required(),
				mcp.Description("JSON array string of the FULL desired token list, e.g. [\"projects:read\",\"invoices:read\"].")),
			mcp.WithString("description", mcp.Description("Optional human description of the role."))),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := req.GetString("name", "")
			permsStr := req.GetString("permissions", "")
			var perms []string
			if !json.Valid([]byte(permsStr)) || json.Unmarshal([]byte(permsStr), &perms) != nil {
				return mcp.NewToolResultError("'permissions' must be a JSON array of token strings"), nil
			}
			payload, _ := json.Marshal(map[string]any{
				"name": name, "description": req.GetString("description", ""), "permissions": perms,
			})
			b, status, err := pb.request("POST", "/api/superadmin/roles", payload)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	// 9. Assign roles to a user.
	s.AddTool(
		mcp.NewTool("assign_user_roles",
			mcp.WithDescription("Set which roles a user holds (by role NAME). Identify the user by "+
				"'email' or 'userId'. The user's effective permissions become the union of these roles.\n"+
				"⚠️ POLICY DECISION — DO NOT GUESS. Granting a role changes what this person can do. ASK THE "+
				"USER before assigning a powerful role (e.g. \"admin\") or otherwise changing someone's "+
				"access. This REPLACES the user's current roles, so include all roles they should keep."),
			mcp.WithString("email", mcp.Description("User email (use this OR userId).")),
			mcp.WithString("userId", mcp.Description("User record id (use this OR email).")),
			mcp.WithString("roles", mcp.Required(),
				mcp.Description("JSON array string of role names, e.g. [\"viewer\",\"editor\"].")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			rolesStr := req.GetString("roles", "")
			var roles []string
			if !json.Valid([]byte(rolesStr)) || json.Unmarshal([]byte(rolesStr), &roles) != nil {
				return mcp.NewToolResultError("'roles' must be a JSON array of role names"), nil
			}
			payload, _ := json.Marshal(map[string]any{
				"email": req.GetString("email", ""), "userId": req.GetString("userId", ""), "roles": roles,
			})
			b, status, err := pb.request("POST", "/api/superadmin/users/roles", payload)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return statusText(status, b), nil
		})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "pb-mcp error: %v\n", err)
		os.Exit(1)
	}
}

// ---- helpers ----

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// statusText returns the body as a tool result, flagged as error on non-2xx.
func statusText(status int, body []byte) *mcp.CallToolResult {
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("HTTP %d: %s", status, string(body)))
	}
	return mcp.NewToolResultText(string(body))
}

// summarizeCollections trims the verbose /api/collections payload to name + fields.
func summarizeCollections(body []byte) string {
	var parsed struct {
		Items []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			System bool   `json:"system"`
			Fields []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"fields"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return string(body)
	}
	var sb strings.Builder
	for _, c := range parsed.Items {
		if c.System {
			continue // hide _superusers etc.
		}
		fields := make([]string, 0, len(c.Fields))
		for _, f := range c.Fields {
			fields = append(fields, f.Name+":"+f.Type)
		}
		fmt.Fprintf(&sb, "%s (%s) -> %s\n", c.Name, c.Type, strings.Join(fields, ", "))
	}
	if sb.Len() == 0 {
		return "(no user collections yet)"
	}
	return sb.String()
}
