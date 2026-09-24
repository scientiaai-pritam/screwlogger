package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
)

// offlineAfter is how many seconds a device may go without a heartbeat before
// it is reported as "offline" (2 × HealthyFlushInterval). Server-side only: the
// server package must not depend on internal/agent.
const offlineAfter = 120

// QueryAPI returns the read-only query API (GET /api/v1/*), protected by
// API-key Bearer auth with CORS (Decision 3, spec §3.5).
func QueryAPI(store *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/devices", handleDevices(store))
	mux.HandleFunc("GET /api/v1/usage", handleUsage(store))
	mux.HandleFunc("GET /api/v1/active-ratio", handleActiveRatio(store))
	mux.HandleFunc("GET /api/v1/events", handleEvents(store))
	return requireAPIKey(store, mux)
}

// requireAPIKey authenticates the Bearer API key and sets CORS headers on every
// response, including errors. OPTIONS preflight is answered with 204 and no
// auth (Decision 3).
func requireAPIKey(store *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORS(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unknown api key")
			return
		}
		if _, err := store.ResolveAPIKey(token); err != nil {
			switch {
			case errors.Is(err, ErrUnknownKey):
				writeErr(w, http.StatusUnauthorized, "unknown api key")
			case errors.Is(err, ErrRevoked):
				writeErr(w, http.StatusForbidden, "api key revoked")
			default:
				log.Printf("query api: auth: %v", err)
				writeErr(w, http.StatusInternalServerError, "storage error")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type deviceResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	EnrolledAt int64  `json:"enrolled_at"`
	LastSeen   *int64 `json:"last_seen"`
	RevokedAt  *int64 `json:"revoked_at"`
	Status     string `json:"status"`
}

type usageResponse struct {
	GroupBy string   `json:"group_by"`
	Buckets []Bucket `json:"buckets"`
}

type eventsResponse struct {
	Events []EventRow `json:"events"`
}

func handleDevices(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.ListDevices()
		if err != nil {
			log.Printf("query api: list devices: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		now := time.Now().Unix()
		out := make([]deviceResponse, 0, len(rows))
		for _, d := range rows {
			dr := deviceResponse{ID: d.ID, Name: d.Name, EnrolledAt: d.EnrolledAt, Status: deviceStatus(d, now)}
			if d.LastSeen.Valid {
				v := d.LastSeen.Int64
				dr.LastSeen = &v
			}
			if d.RevokedAt.Valid {
				v := d.RevokedAt.Int64
				dr.RevokedAt = &v
			}
			out = append(out, dr)
		}
		writeJSON(w, out)
	}
}

func deviceStatus(d DeviceRow, now int64) string {
	if d.RevokedAt.Valid {
		return "revoked"
	}
	if !d.LastSeen.Valid || now-d.LastSeen.Int64 > offlineAfter {
		return "offline"
	}
	return "active"
}

func handleUsage(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		groupBy := q.Get("group_by")
		if groupBy == "" {
			groupBy = "category"
		}
		if groupBy != "category" && groupBy != "app" {
			writeErr(w, http.StatusBadRequest, "invalid group_by")
			return
		}
		from, to, ok := parseWindow(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid from/to")
			return
		}
		buckets, err := store.DwellUsage(q.Get("device"), from, to, groupBy)
		if err != nil {
			log.Printf("query api: usage: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		if buckets == nil {
			buckets = []Bucket{}
		}
		writeJSON(w, usageResponse{GroupBy: groupBy, Buckets: buckets})
	}
}

func handleActiveRatio(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, to, ok := parseWindow(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid from/to")
			return
		}
		ar, err := store.ActiveRatio(q.Get("device"), from, to)
		if err != nil {
			log.Printf("query api: active ratio: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		writeJSON(w, ar)
	}
}

func handleEvents(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, to, ok := parseWindow(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid from/to")
			return
		}
		limit := 0 // absent -> ListEvents applies its 1000 default
		if lv := q.Get("limit"); lv != "" {
			n, err := strconv.ParseInt(lv, 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid limit")
				return
			}
			if n < 1 || n > 10000 {
				writeErr(w, http.StatusBadRequest, "limit out of range")
				return
			}
			limit = int(n)
		}
		rows, err := store.ListEvents(q.Get("device"), from, to, limit)
		if err != nil {
			log.Printf("query api: events: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		if rows == nil {
			rows = []EventRow{}
		}
		writeJSON(w, eventsResponse{Events: rows})
	}
}

// parseWindow reads from/to as unix seconds; absent or empty values default to
// 0 and now respectively. A present-but-non-integer value is an error (Review
// Focus #5: reject bad params, never silently default).
func parseWindow(r *http.Request) (from, to int64, ok bool) {
	var err error
	if from, err = intParam(r, "from", 0); err != nil {
		return 0, 0, false
	}
	if to, err = intParam(r, "to", time.Now().Unix()); err != nil {
		return 0, 0, false
	}
	return from, to, true
}

func intParam(r *http.Request, name string, def int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	return strconv.ParseInt(v, 10, 64)
}
