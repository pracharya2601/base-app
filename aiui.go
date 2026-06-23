package main

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// The standalone AI-provider admin page has been folded into the unified console
// (admin_ui.html, served at GET /admin). This route is kept only to redirect the
// old URL into the console's AI-providers section, so existing links work.
func registerAIUI(se *core.ServeEvent) {
	se.Router.GET("/admin/ai", func(e *core.RequestEvent) error {
		return e.Redirect(http.StatusFound, "/admin#providers")
	})
}
