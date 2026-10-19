//go:build windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"screwlogger/internal/protocol"
)

// newTestChecker builds an UpdateChecker pointed at srv, with a stubbed launch
// that records the path it would upgrade to instead of spawning a process.
func newTestChecker(t *testing.T, srvURL, current string, launched *string) *UpdateChecker {
	t.Helper()
	u := NewUpdateChecker(Config{ServerURL: srvURL, Token: "tok", DataDir: t.TempDir()}, `C:\monsvc\monsvc.exe`)
	u.currentVersion = current
	u.launch = func(newExe string) error { *launched = newExe; return nil }
	return u
}

func TestCheckOnceNoReleaseIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "none", http.StatusNotFound)
	}))
	defer srv.Close()
	var launched string
	u := newTestChecker(t, srv.URL, "dev", &launched)
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched != "" {
		t.Fatalf("no release must not launch upgrade, got %q", launched)
	}
}

func TestCheckOnceSameVersionIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(protocol.AgentVersionResponse{Version: "v2"})
	}))
	defer srv.Close()
	var launched string
	u := newTestChecker(t, srv.URL, "v2", &launched) // already running v2
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched != "" {
		t.Fatalf("matching version must not launch upgrade, got %q", launched)
	}
}

func TestCheckOnceDownloadsVerifiesAndLaunches(t *testing.T) {
	body := []byte("new-agent-bytes")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/version":
			json.NewEncoder(w).Encode(protocol.AgentVersionResponse{
				Version: "v2", SHA256: hash, Size: int64(len(body)),
			})
		case "/v1/agent/download":
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var launched string
	u := newTestChecker(t, srv.URL, "v1", &launched)
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched == "" {
		t.Fatal("upgrade was not launched after a newer verified release")
	}
}

func TestCheckOnceRejectsBadHash(t *testing.T) {
	body := []byte("new-agent-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/version":
			json.NewEncoder(w).Encode(protocol.AgentVersionResponse{
				Version: "v2", SHA256: "deadbeef", Size: int64(len(body)),
			})
		case "/v1/agent/download":
			w.Write(body)
		}
	}))
	defer srv.Close()
	var launched string
	u := newTestChecker(t, srv.URL, "v1", &launched)
	if err := u.checkOnce(context.Background()); err == nil {
		t.Fatal("expected an error on sha256 mismatch")
	}
	if launched != "" {
		t.Fatalf("a corrupt download must not launch upgrade, got %q", launched)
	}
}
