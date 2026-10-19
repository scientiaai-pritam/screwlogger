package server

import "net/http"

// NewMux assembles every HTTP route (ingest, health, query API, admin, and
// agent auto-update). rel may be nil, in which case the auto-update endpoints
// return 404.
func NewMux(store *Store, auth *AdminAuth, rel *AgentRelease) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/ingest", IngestHandler(store))
	mux.HandleFunc("GET /v1/agent/version", AgentVersionHandler(store, rel))
	mux.HandleFunc("GET /v1/agent/download", AgentDownloadHandler(store, rel))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/api/v1/", QueryAPI(store))
	mux.HandleFunc("POST /admin/login", auth.Login)
	mux.HandleFunc("POST /admin/logout", auth.Logout)
	mux.Handle("/admin/api/", AdminAPI(store, auth))
	mux.Handle("/admin/", Dashboard())
	return mux
}
