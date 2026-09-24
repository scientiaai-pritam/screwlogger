package server

import "net/http"

// NewMux assembles every HTTP route (ingest, health, query API, admin).
// The Dashboard route is added by Task 6.
func NewMux(store *Store, auth *AdminAuth) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/ingest", IngestHandler(store))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/api/v1/", QueryAPI(store))
	mux.HandleFunc("POST /admin/login", auth.Login)
	mux.HandleFunc("POST /admin/logout", auth.Logout)
	mux.Handle("/admin/api/", AdminAPI(store, auth))
	// TODO(Task 6): mux.Handle("/admin/", Dashboard(...))
	return mux
}
