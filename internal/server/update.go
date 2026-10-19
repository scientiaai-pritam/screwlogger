package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	"screwlogger/internal/protocol"
)

// AgentRelease is the agent binary the server advertises for auto-update. It is
// loaded once at startup from -agent-binary/-agent-version; the file is read on
// each download (http.ServeFile), so replacing the binary on disk and restarting
// the server is how a new version is rolled out. A nil *AgentRelease means
// auto-update is disabled and the endpoints return 404.
type AgentRelease struct {
	Version string
	Path    string
	SHA256  string
	Size    int64
}

// LoadAgentRelease hashes the binary at path and records its size so the agent
// can verify the download. version is the opaque build string the agent compares
// against its own agent.Version.
func LoadAgentRelease(version, path string) (*AgentRelease, error) {
	if version == "" {
		return nil, errors.New("agent release: version is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	return &AgentRelease{
		Version: version,
		Path:    path,
		SHA256:  hex.EncodeToString(h.Sum(nil)),
		Size:    n,
	}, nil
}

// AgentVersionHandler serves GET /v1/agent/version: the advertised release
// metadata, device-token authenticated. 404 when no release is configured.
func AgentVersionHandler(store *Store, rel *AgentRelease) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authDevice(store, w, r) {
			return
		}
		if rel == nil {
			writeErr(w, http.StatusNotFound, "no agent release configured")
			return
		}
		writeJSON(w, protocol.AgentVersionResponse{
			Version: rel.Version,
			SHA256:  rel.SHA256,
			Size:    rel.Size,
		})
	}
}

// AgentDownloadHandler serves GET /v1/agent/download: the raw binary, device-
// token authenticated. 404 when no release is configured.
func AgentDownloadHandler(store *Store, rel *AgentRelease) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authDevice(store, w, r) {
			return
		}
		if rel == nil {
			writeErr(w, http.StatusNotFound, "no agent release configured")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, rel.Path)
	}
}

// authDevice authenticates a device bearer token (the same credential the agent
// uses for ingest), writing the appropriate error and returning false on
// failure. Auto-update is thus restricted to enrolled, non-revoked devices.
func authDevice(store *Store, w http.ResponseWriter, r *http.Request) bool {
	token, ok := bearerToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return false
	}
	if _, err := store.DeviceIDByToken(token); err != nil {
		switch {
		case errors.Is(err, ErrRevoked):
			writeErr(w, http.StatusForbidden, "device token revoked")
		case errors.Is(err, ErrUnknownToken):
			writeErr(w, http.StatusUnauthorized, "invalid device token")
		default:
			log.Printf("agent update: auth: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
		}
		return false
	}
	return true
}
