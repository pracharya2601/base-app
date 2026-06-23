package main

import (
	_ "embed"

	"github.com/pocketbase/pocketbase/core"
)

// Unified admin console: a single page with sidebar navigation over BOTH the
// API-key manager and the AI-provider manager (+ a proxy test box), served at
// GET /admin. Same self-contained, single-binary, no-secrets approach as
// keysui.go / aiui.go: it logs in as a superuser CLIENT-SIDE (reusing the /_/
// dashboard session when present) and calls the same superuser-gated endpoints.
//
// The older standalone pages (/admin/apikeys, /admin/ai) still work for
// back-compat; this is the combined home.

//go:embed admin_ui.html
var adminUIHTML []byte

func registerAdminUI(se *core.ServeEvent) {
	se.Router.GET("/admin", func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := e.Response.Write(adminUIHTML)
		return err
	})
}
