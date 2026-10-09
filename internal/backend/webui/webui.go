// Package webui serves the embedded incident dashboard.
//
// The UI is a static single-page app (hash routing) that talks to the JSON
// API in internal/backend/api. It is embedded into the binary so the backend
// image ships the dashboard with no extra build step or container.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler serves the dashboard at / and its assets by filename.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed layout is fixed at build time
	}
	return http.FileServer(http.FS(sub))
}
