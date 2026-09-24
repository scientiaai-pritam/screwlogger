package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"screwlogger/internal/protocol"
)

// maxIngestBody caps the request body at 1 MiB — far above the shipper's
// 500-heartbeat batch (~75 KiB), enough headroom for backfill bursts.
const maxIngestBody = 1 << 20

// IngestHandler serves POST /v1/ingest with device-token auth (spec §3.5).
// Any 4xx writes zero rows (Review Focus #5).
func IngestHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		deviceID, err := store.DeviceIDByToken(token)
		if err != nil {
			if errors.Is(err, ErrRevoked) {
				writeErr(w, http.StatusForbidden, "device token revoked")
				return
			}
			if errors.Is(err, ErrUnknownToken) {
				writeErr(w, http.StatusUnauthorized, "invalid device token")
				return
			}
			log.Printf("ingest: auth: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxIngestBody)
		var batch protocol.IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		if batch.SchemaVersion != protocol.SchemaVersion {
			writeErr(w, http.StatusBadRequest, "unsupported schema_version")
			return
		}
		accepted, err := store.InsertHeartbeats(deviceID, batch.DeviceSentAt, batch.Heartbeats)
		if err != nil {
			log.Printf("ingest: device %s: %v", deviceID, err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.IngestResponse{Accepted: accepted})
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}
	return h[len(prefix):], true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
