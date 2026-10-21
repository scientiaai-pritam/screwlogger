//go:build windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"screwlogger/internal/protocol"
)

// newTestChecker builds an UpdateChecker pointed at srv, with a stubbed launch
// that records the path it would upgrade to instead of spawning a process. The
// installed exe is a temp file holding "old-agent-bytes".
func newTestChecker(t *testing.T, srvURL, current string, launched *string) *UpdateChecker {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "monsvc.exe")
	if err := os.WriteFile(exe, []byte("old-agent-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := NewUpdateChecker(Config{ServerURL: srvURL, Token: "tok", DataDir: t.TempDir()}, exe)
	u.currentVersion = current
	u.launch = func(newExe string) error { *launched = newExe; return nil }
	return u
}

// releaseServer advertises version v for body and counts downloads.
func releaseServer(t *testing.T, v string, body *[]byte, downloads *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/version":
			sum := sha256.Sum256(*body)
			json.NewEncoder(w).Encode(protocol.AgentVersionResponse{
				Version: v, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(*body)),
			})
		case "/v1/agent/download":
			*downloads++
			w.Write(*body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
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

// The loop guard: once the advertised binary is installed, the agent must not
// upgrade again even when its stamped Version ("dev") differs from the server's
// version string — otherwise it would re-upgrade on every check forever.
func TestCheckOnceInstalledBinaryIsCurrentDespiteVersionMismatch(t *testing.T) {
	body := []byte("old-agent-bytes") // identical to the installed exe
	var downloads int
	srv := releaseServer(t, "v2", &body, &downloads)
	var launched string
	u := newTestChecker(t, srv.URL, "dev", &launched)
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched != "" || downloads != 0 {
		t.Fatalf("installed binary must be current: launched=%q downloads=%d", launched, downloads)
	}
}

// A rebuilt binary served under an unchanged version string is still a new
// release: updates must not be blocked by version strings.
func TestCheckOnceSameVersionNewBinaryUpgrades(t *testing.T) {
	body := []byte("rebuilt-agent-bytes")
	var downloads int
	srv := releaseServer(t, "v2", &body, &downloads)
	var launched string
	u := newTestChecker(t, srv.URL, "v2", &launched)
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched == "" {
		t.Fatal("a different binary under the same version string was not applied")
	}
}

// When the installed exe cannot be hashed, the version string decides.
func TestCheckOnceFallsBackToVersionWhenExeUnreadable(t *testing.T) {
	body := []byte("new-agent-bytes")
	var downloads int
	srv := releaseServer(t, "v2", &body, &downloads)
	var launched string
	u := newTestChecker(t, srv.URL, "v2", &launched)
	u.exePath = filepath.Join(t.TempDir(), "missing.exe")
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched != "" {
		t.Fatalf("same version with unreadable exe must not upgrade, got %q", launched)
	}
	u.currentVersion = "v1"
	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launched == "" {
		t.Fatal("older version with unreadable exe was not upgraded")
	}
}
