package adminui

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// API-key management currently lives in the classic console (/admin/classic); the
// new SPA at /admin doesn't host it yet. This route redirects the old URL (and the
// SPA's "API Keys" link) to the classic console's keys section.
func RegisterKeys(se *core.ServeEvent) {
	se.Router.GET("/admin/apikeys", func(e *core.RequestEvent) error {
		return e.Redirect(http.StatusFound, "/admin/classic#keys")
	})
}
