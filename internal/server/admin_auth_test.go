package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doAdminLogin(a *AdminAuth, password string) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Login(w, req)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "sl_admin" {
			return c
		}
	}
	t.Fatalf("no sl_admin cookie; Set-Cookie=%v", w.Header()["Set-Cookie"])
	return nil
}

func TestNewAdminAuthGeneratesAndVerifies(t *testing.T) {
	a, plaintext, err := NewAdminAuth("")
	if err != nil {
		t.Fatal(err)
	}
	if plaintext == "" {
		t.Fatal("generated plaintext must be non-empty")
	}
	w := doAdminLogin(a, plaintext)
	if w.Code != http.StatusOK {
		t.Fatalf("generated password must verify: got %d (%s)", w.Code, w.Body.String())
	}
	sessionCookie(t, w)
}

func TestAdminLoginCorrectPassword(t *testing.T) {
	a, _, err := NewAdminAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	w := doAdminLogin(a, "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("correct password: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	c := sessionCookie(t, w)
	if !c.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite want Lax, got %v", c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("cookie Path want /, got %q", c.Path)
	}

	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid session: want 200, got %d", rec.Code)
	}
}

func TestAdminLoginWrongPassword(t *testing.T) {
	a, _, err := NewAdminAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	w := doAdminLogin(a, "wrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "sl_admin" {
			t.Fatal("wrong password must not set a cookie")
		}
	}
}

func TestRequireAdminNoCookieAndForged(t *testing.T) {
	a, _, _ := NewAdminAuth("s3cret")
	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: want 401, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "sl_admin", Value: "forged"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie: want 401, got %d", rec.Code)
	}
}

func TestAdminLogoutClearsAccess(t *testing.T) {
	a, _, _ := NewAdminAuth("s3cret")
	c := sessionCookie(t, doAdminLogin(a, "s3cret"))

	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	a.Logout(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(c)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: want 401, got %d", rec2.Code)
	}
}

func TestSessionExpiry(t *testing.T) {
	a, _, _ := NewAdminAuth("s3cret")
	a.mu.Lock()
	a.sessions["expired-token"] = time.Now().Add(-time.Hour)
	a.mu.Unlock()

	h := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "sl_admin", Value: "expired-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: want 401, got %d", rec.Code)
	}

	a.mu.Lock()
	_, exists := a.sessions["expired-token"]
	a.mu.Unlock()
	if exists {
		t.Fatal("expired session should be deleted on lookup")
	}
}
