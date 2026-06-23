package main

import (
	_ "embed"

	"github.com/pocketbase/pocketbase/core"
)

// A tiny, self-contained admin page for minting/listing/revoking API keys, so
// you don't have to curl. It is embedded in the binary (single-binary ethos) and
// served at GET /admin/apikeys.
//
// Security: the page itself holds NO secrets and adds NO new privileged path. It
// logs in as a superadmin CLIENT-SIDE (auth-with-password -> JWT held in the
// browser tab) and then calls the SAME superuser-gated endpoints a curl would
// (/api/superadmin/apikeys, /scopes, /roles). Without valid superadmin creds the
// page can do nothing, so serving the HTML unauthenticated is safe.

//go:embed apikeys_ui.html
var apiKeysUIHTML []byte

// registerKeysUI serves the embedded API-key admin page.
func registerKeysUI(se *core.ServeEvent) {
	se.Router.GET("/admin/apikeys", func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := e.Response.Write(apiKeysUIHTML)
		return err
	})
}
