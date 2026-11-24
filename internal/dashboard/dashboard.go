// Package dashboard serves the usage dashboard: one static page that reads
// tokens and usage through PocketBase's API with the superuser's own session.
// The page carries no data; without a superuser session it shows a sign-in
// form and nothing else.
package dashboard

import (
	_ "embed"
	"net/http"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

// Path is where the dashboard is served.
const Path = "/dashboard"

//go:embed index.html
var page string

// Register adds the dashboard route to the server.
func Register(se *core.ServeEvent) {
	se.Router.GET(Path, func(e *core.RequestEvent) error {
		e.Response.Header().Set("Cache-Control", "no-store")
		e.Response.Header().Set("X-Frame-Options", "DENY")
		return e.HTML(http.StatusOK, page)
	})
}
