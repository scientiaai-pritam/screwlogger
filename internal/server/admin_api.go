package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// AdminAPI returns the admin JSON API (every /admin/api/* route), protected by
// the admin session cookie (Decision 1). Devices, rules and API keys are managed
// here; the dashboard-data endpoints reuse the query handlers so the query API
// stays API-key-only and admin stays session-only.
func AdminAPI(store *Store, auth *AdminAuth) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /admin/api/devices", handleDeviceEnroll(store))
	api.HandleFunc("GET /admin/api/devices", handleDevices(store))
	api.HandleFunc("POST /admin/api/devices/{id}/revoke", handleDeviceRevoke(store))
	api.HandleFunc("GET /admin/api/rules", handleRulesList(store))
	api.HandleFunc("PUT /admin/api/rules", handleRuleUpsert(store))
	api.HandleFunc("DELETE /admin/api/rules", handleRuleDelete(store))
	api.HandleFunc("POST /admin/api/keys", handleKeyCreate(store))
	api.HandleFunc("GET /admin/api/keys", handleKeyList(store))
	api.HandleFunc("POST /admin/api/keys/{hash}/revoke", handleKeyRevoke(store))
	api.HandleFunc("GET /admin/api/usage", handleUsage(store))
	api.HandleFunc("GET /admin/api/active-ratio", handleActiveRatio(store))
	api.HandleFunc("GET /admin/api/events", handleEvents(store))
	return auth.RequireAdmin(api)
}

// deviceEnrollResponse is the body of a successful POST /admin/api/devices: the
// one-time plaintext token is shown exactly here, never again.
type deviceEnrollResponse struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

func handleDeviceEnroll(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		id, token, err := store.CreateDevice(body.Name)
		if err != nil {
			log.Printf("admin api: enroll device: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		writeJSON(w, deviceEnrollResponse{DeviceID: id, Token: token})
	}
}

func handleDeviceRevoke(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := store.RevokeDevice(id); err != nil {
			log.Printf("admin api: revoke device %s: %v", id, err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// ruleResponse is one categorization rule as exposed to the admin UI. The store
// field is ExePattern; the JSON key is "pattern".
type ruleResponse struct {
	Pattern   string `json:"pattern"`
	Category  string `json:"category"`
	UpdatedAt int64  `json:"updated_at"`
}

func handleRulesList(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.ListRules()
		if err != nil {
			log.Printf("admin api: list rules: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		out := make([]ruleResponse, 0, len(rows))
		for _, r := range rows {
			out = append(out, ruleResponse{Pattern: r.ExePattern, Category: r.Category, UpdatedAt: r.UpdatedAt})
		}
		writeJSON(w, out)
	}
}

func handleRuleUpsert(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Pattern        string `json:"pattern"`
			Category       string `json:"category"`
			ApplyToHistory bool   `json:"apply_to_history"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		if err := store.UpsertRule(body.Pattern, body.Category); err != nil {
			log.Printf("admin api: upsert rule: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		resp := map[string]any{"ok": true}
		if body.ApplyToHistory {
			n, err := store.Recategorize(body.Pattern, body.Category)
			if err != nil {
				log.Printf("admin api: recategorize: %v", err)
				writeErr(w, http.StatusInternalServerError, "storage error")
				return
			}
			resp["recategorized"] = n
		}
		writeJSON(w, resp)
	}
}

func handleRuleDelete(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Pattern string `json:"pattern"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		if err := store.DeleteRule(body.Pattern); err != nil {
			log.Printf("admin api: delete rule: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func handleKeyCreate(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		key, err := store.CreateAPIKey(body.Label)
		if err != nil {
			log.Printf("admin api: create key: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		writeJSON(w, map[string]string{"key": key})
	}
}

// apiKeyResponse is one API key as exposed to the admin UI — only the 8-char
// hash prefix, never the full hash or the plaintext key.
type apiKeyResponse struct {
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	RevokedAt  *int64 `json:"revoked_at"`
	HashPrefix string `json:"hash_prefix"`
}

func handleKeyList(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.ListAPIKeys()
		if err != nil {
			log.Printf("admin api: list keys: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		out := make([]apiKeyResponse, 0, len(rows))
		for _, k := range rows {
			kr := apiKeyResponse{Label: k.Label, CreatedAt: k.CreatedAt, HashPrefix: k.HashPrefix}
			if k.RevokedAt.Valid {
				v := k.RevokedAt.Int64
				kr.RevokedAt = &v
			}
			out = append(out, kr)
		}
		writeJSON(w, out)
	}
}

func handleKeyRevoke(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		n, err := store.RevokeAPIKeyByPrefix(hash)
		if err != nil {
			log.Printf("admin api: revoke key %s: %v", hash, err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		if n == 0 {
			writeErr(w, http.StatusNotFound, "unknown api key")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}
