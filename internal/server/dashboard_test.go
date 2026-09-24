package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"screwlogger/internal/server"
)

func TestDashboardServesAdminUI(t *testing.T) {
	s := openTestStore(t)
	auth, _, err := server.NewAdminAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	mux := server.NewMux(s, auth)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// /admin/ serves the SPA shell as text/html.
	w := get("/admin/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /admin/: want text/html Content-Type, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<!doctype html") && !strings.Contains(body, "<!DOCTYPE html") {
		t.Fatalf("GET /admin/: body does not contain doctype: %q", body)
	}

	// A real asset is served.
	if w = get("/admin/app.js"); w.Code != http.StatusOK {
		t.Fatalf("GET /admin/app.js: want 200, got %d", w.Code)
	}

	// A missing asset is a 404, not a silent SPA fallback.
	if w = get("/admin/nonexistent.css"); w.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/nonexistent.css: want 404, got %d", w.Code)
	}
}
