package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var webFS embed.FS

// Dashboard serves the embedded admin UI at /admin and /admin/... (spec §3.5).
// The SPA shell is public; it shows the login view until an sl_admin session
// exists, then the JSON API under /admin/api/* (session-gated) drives it.
func Dashboard() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/admin/", fileServer)
}
