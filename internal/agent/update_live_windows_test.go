//go:build windows

package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"screwlogger/internal/server"
)

// TestLiveAutoUpdateSwapsBinary drives the real auto-update client against the
// real server routes end to end: fetch version → download → sha256+size verify →
// apply. It is gated behind SL_LIVE_UPDATE (and the two binary paths) so it only
// runs when explicitly invoked with pre-built v1/v2 agents; a normal `go test`
// skips it.
//
// What is real here: server.AgentVersionHandler / AgentDownloadHandler with
// device-token auth, and UpdateChecker.fetchVersion / download / checkOnce
// (including the integrity gate). Only the SCM stop/replace/restart is stood in
// for by copyFile — the one piece that needs the installed Windows service and
// is covered separately by the spawn/upgrade tests.
func TestLiveAutoUpdateSwapsBinary(t *testing.T) {
	if os.Getenv("SL_LIVE_UPDATE") == "" {
		t.Skip("set SL_LIVE_UPDATE=1 (with SL_V2_EXE, SL_INSTALL_EXE) to run the live update test")
	}
	v2 := os.Getenv("SL_V2_EXE")
	install := os.Getenv("SL_INSTALL_EXE")
	if v2 == "" || install == "" {
		t.Fatal("SL_V2_EXE and SL_INSTALL_EXE must point to the release and installed binaries")
	}

	// Real server: a configured release + an enrolled device token.
	store, err := server.OpenStore(t.TempDir() + "/live.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, token, err := store.CreateDevice("PC-LIVE")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := server.LoadAgentRelease("update-v2", v2)
	if err != nil {
		t.Fatal(err)
	}
	auth, _, err := server.NewAdminAuth("pw")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.NewMux(store, auth, rel))
	defer srv.Close()
	t.Logf("server advertises version=%s sha256=%s size=%d", rel.Version, rel.SHA256, rel.Size)

	// Confirm the installed binary is v1 before the update.
	if got := runVersion(t, install); got != "update-v1" {
		t.Fatalf("installed version before update = %q, want update-v1", got)
	}
	t.Logf("installed binary before update reports: %s", runVersion(t, install))

	// Real client. launch is replaced by the swap the service upgrade performs
	// (copy the verified download over the installed path); SCM stop/start is
	// the only omitted step.
	u := NewUpdateChecker(Config{ServerURL: srv.URL, Token: token, DataDir: t.TempDir()}, install)
	u.currentVersion = "update-v1"
	var launchedFrom string
	u.launch = func(newExe string) error {
		launchedFrom = newExe
		return copyFile(newExe, install)
	}

	if err := u.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if launchedFrom == "" {
		t.Fatal("update was not applied (launch never called)")
	}
	t.Logf("downloaded + verified; applied from staged file: %s", launchedFrom)

	// The installed binary now reports v2 — the swap took effect.
	if got := runVersion(t, install); got != "update-v2" {
		t.Fatalf("installed version after update = %q, want update-v2", got)
	}
	t.Logf("installed binary after update reports: %s", runVersion(t, install))
}

func runVersion(t *testing.T, exe string) string {
	t.Helper()
	out, err := exec.Command(exe, "-version").Output()
	if err != nil {
		t.Fatalf("run %s -version: %v", exe, err)
	}
	return strings.TrimSpace(string(out))
}
