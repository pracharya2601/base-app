package adminui

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// AI-provider management currently lives in the classic console (/admin/classic);
// the new SPA at /admin doesn't host it yet. This route redirects the old URL (and
// the SPA's "AI Providers" link) to the classic console's providers section.
func RegisterAIUI(se *core.ServeEvent) {
	se.Router.GET("/admin/ai", func(e *core.RequestEvent) error {
		return e.Redirect(http.StatusFound, "/admin/classic#providers")
	})
}
