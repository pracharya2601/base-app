package adminui

import (
	"embed"
	"io/fs"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// The /admin console is now a Vite+Svelte SPA (source in ../frontend, built into
// ./spa via `npm run build`) embedded into the binary and served here. It looks
// native (reuses PocketBase's design tokens) and logs in CLIENT-SIDE as a
// superuser, calling the same superuser-gated endpoints as before. The single
// binary, no-secrets approach is unchanged — only the frontend toolchain is new.
//
// Routing avoids colliding with the standalone /admin/ai and /admin/apikeys pages:
//   GET /admin                  -> SPA index.html
//   GET /admin/assets/{path...} -> hashed JS/CSS bundles
//   GET /admin/classic          -> the previous single-file console (fallback)
//
// NB: spa/ is committed (built output) so `go build` works without running npm; the
// Dockerfile rebuilds it fresh.

//go:embed all:spa
var spaFS embed.FS

//go:embed admin_ui.html
var classicHTML []byte

func RegisterAdmin(se *core.ServeEvent) {
	index, _ := spaFS.ReadFile("spa/index.html")
	se.Router.GET("/admin", func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := e.Response.Write(index)
		return err
	})

	if assets, err := fs.Sub(spaFS, "spa/assets"); err == nil {
		se.Router.GET("/admin/assets/{path...}", apis.Static(assets, false))
	}

	// The previous single-file console, kept reachable during the migration.
	se.Router.GET("/admin/classic", func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := e.Response.Write(classicHTML)
		return err
	})
}
