package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// sessionCookieName is the admin session cookie (Decision 1).
	sessionCookieName = "sl_admin"
	// sessionTTL is how long an admin session stays valid (Decision 1).
	sessionTTL = 12 * time.Hour
)

// AdminAuth owns the bcrypt-hashed admin password and the in-memory session
// table (Decision 1). The password is never stored or compared in plaintext.
type AdminAuth struct {
	hash     []byte
	mu       sync.Mutex
	sessions map[string]time.Time
}

// NewAdminAuth bcrypt-hashes the password. If plaintext is empty, a random one
// is generated (crypto/rand) and returned as the second value so main can log
// it once at startup. The returned plaintext is always the effective password.
func NewAdminAuth(plaintext string) (*AdminAuth, string, error) {
	if plaintext == "" {
		buf := make([]byte, 18) // 18 bytes -> 24 base64 chars
		if _, err := rand.Read(buf); err != nil {
			return nil, "", err
		}
		plaintext = base64.RawURLEncoding.EncodeToString(buf)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", err
	}
	return &AdminAuth{hash: hash, sessions: make(map[string]time.Time)}, plaintext, nil
}

// Login verifies the password and, on success, mints a 32-byte session token
// and sets the sl_admin cookie (HttpOnly, SameSite=Lax, Path=/). Wrong password
// returns a generic 401 — no user enumeration (Review Focus #3).
func (a *AdminAuth) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if err := bcrypt.CompareHashAndPassword(a.hash, []byte(body.Password)); err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		writeErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	session := base64.RawURLEncoding.EncodeToString(token)
	a.mu.Lock()
	a.sessions[session] = time.Now().Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// Logout deletes the session (if any) and clears the cookie.
func (a *AdminAuth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

// RequireAdmin guards a route with the admin session cookie. Missing, unknown,
// or expired sessions return 401; expired sessions are deleted on lookup
// (Review Focus #4).
func (a *AdminAuth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		a.mu.Lock()
		expiry, ok := a.sessions[c.Value]
		if ok && time.Now().After(expiry) {
			delete(a.sessions, c.Value)
			ok = false
		}
		a.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
