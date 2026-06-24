package adminui

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// The standalone API-key admin page has been folded into the unified console
// (admin_ui.html, served at GET /admin). This route is kept only to redirect the
// old URL into the console's API-keys section, so existing links/bookmarks work.
func RegisterKeys(se *core.ServeEvent) {
	se.Router.GET("/admin/apikeys", func(e *core.RequestEvent) error {
		return e.Redirect(http.StatusFound, "/admin#keys")
	})
}
