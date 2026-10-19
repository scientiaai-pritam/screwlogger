package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

// writeReleaseBinary writes a fake agent binary and returns its path + sha256.
func writeReleaseBinary(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monsvc.exe")
	body := []byte("fake-agent-binary-contents")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return path, hex.EncodeToString(sum[:])
}

func TestAgentVersionAndDownload(t *testing.T) {
	store := openTestStore(t)
	auth, _, err := server.NewAdminAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := store.CreateDevice("PC-1")
	if err != nil {
		t.Fatal(err)
	}
	path, wantHash := writeReleaseBinary(t)
	rel, err := server.LoadAgentRelease("2026.10.19-1", path)
	if err != nil {
		t.Fatal(err)
	}
	if rel.SHA256 != wantHash {
		t.Fatalf("release hash = %s, want %s", rel.SHA256, wantHash)
	}
	mux := server.NewMux(store, auth, rel)

	// version: authorized device gets the advertised metadata.
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/version", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("version status = %d, want 200", w.Code)
	}
	var vr protocol.AgentVersionResponse
	if err := json.NewDecoder(w.Body).Decode(&vr); err != nil {
		t.Fatal(err)
	}
	if vr.Version != "2026.10.19-1" || vr.SHA256 != wantHash {
		t.Fatalf("version response = %+v", vr)
	}

	// download: body hashes to the advertised sha256.
	req = httptest.NewRequest(http.MethodGet, "/v1/agent/download", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", w.Code)
	}
	gotHash := sha256.Sum256(w.Body.Bytes())
	if hex.EncodeToString(gotHash[:]) != wantHash {
		t.Fatalf("downloaded body hash mismatch")
	}
}

func TestAgentVersionRequiresDeviceToken(t *testing.T) {
	store := openTestStore(t)
	auth, _, _ := server.NewAdminAuth("s3cret")
	path, _ := writeReleaseBinary(t)
	rel, _ := server.LoadAgentRelease("v1", path)
	mux := server.NewMux(store, auth, rel)

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/version", nil) // no token
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAgentVersion404WhenNoRelease(t *testing.T) {
	store := openTestStore(t)
	auth, _, _ := server.NewAdminAuth("s3cret")
	_, token, _ := store.CreateDevice("PC-1")
	mux := server.NewMux(store, auth, nil) // auto-update disabled

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/version", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
