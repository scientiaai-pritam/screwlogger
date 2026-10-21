//go:build windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"screwlogger/internal/protocol"
)

// detachedProcess is CREATE_NEW_PROCESS_GROUP's sibling DETACHED_PROCESS, not
// exported by x/sys/windows. The updater child must outlive this service
// process: it stops monsvc (this very process), swaps the binary, and restarts
// it, so it cannot be tied to our lifetime.
const detachedProcess = 0x00000008

// updateHTTPTimeout bounds the version check and the binary download. The
// download timeout is generous because the agent binary is several MB over a
// LAN; the version check is a tiny JSON body but shares the client.
const updateHTTPTimeout = 10 * time.Minute

// updateInitialDelay staggers the first check off service start so a fleet
// rebooting together does not stampede the server, and so a broken new build
// cannot wedge the boot path — the service is fully up and shipping before it
// ever considers replacing itself.
const updateInitialDelay = 2 * time.Minute

// UpdateChecker periodically asks the server which agent release it advertises
// and, when it is not the binary already installed, downloads it (sha256-
// verified) and launches it as a detached `-upgrade` process that swaps the
// service binary and restarts it. It runs only in service mode.
type UpdateChecker struct {
	serverURL      string
	token          string
	currentVersion string
	exePath        string // installed service binary (upgrade target)
	dataDir        string // download staging lives under here
	interval       time.Duration
	client         *http.Client

	// launch is the detached-upgrade spawner; a field so tests can intercept it
	// instead of actually launching a process.
	launch func(newExe string) error
}

// NewUpdateChecker builds the checker from service config and the running
// binary's path. Version is this build's agent.Version.
func NewUpdateChecker(cfg Config, exePath string) *UpdateChecker {
	interval := cfg.UpdateCheckInterval
	if interval <= 0 {
		interval = DefaultUpdateCheckInterval
	}
	return &UpdateChecker{
		serverURL:      cfg.ServerURL,
		token:          cfg.Token,
		currentVersion: Version,
		exePath:        exePath,
		dataDir:        cfg.DataDir,
		interval:       interval,
		client:         &http.Client{Timeout: updateHTTPTimeout},
		launch:         launchUpgrade,
	}
}

// Run checks once after the initial delay and then every interval until ctx is
// cancelled. A check failure is logged and retried on the next tick — a server
// that is down or has no release configured never disturbs sampling.
func (u *UpdateChecker) Run(ctx context.Context) {
	timer := time.NewTimer(updateInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := u.checkOnce(ctx); err != nil {
				log.Printf("auto-update: %v", err)
			}
			timer.Reset(u.interval)
		}
	}
}

// checkOnce performs one version check and, if an upgrade is warranted, applies
// it. It returns nil when there is nothing to do (no release, or already current).
func (u *UpdateChecker) checkOnce(ctx context.Context) error {
	rel, err := u.fetchVersion(ctx)
	if err != nil {
		return err
	}
	if rel.Version == "" || u.isCurrent(rel) {
		return nil // server advertises no release, or we already run it
	}
	log.Printf("auto-update: server advertises %s (running %s); downloading", rel.Version, u.currentVersion)
	newExe, err := u.download(ctx, rel)
	if err != nil {
		return fmt.Errorf("download %s: %w", rel.Version, err)
	}
	log.Printf("auto-update: verified %s at %s; launching self-upgrade", rel.Version, newExe)
	if err := u.launch(newExe); err != nil {
		return fmt.Errorf("launch upgrade: %w", err)
	}
	// The detached child will stop this service shortly; nothing more to do.
	return nil
}

// isCurrent reports whether the advertised release is the binary already
// installed. It compares the installed exe's sha256 against the advertised hash,
// not version strings: a build whose stamped Version differs from the server's
// -agent-version (an unstamped "dev" build, a typo) would otherwise look out of
// date forever and re-upgrade on every check — a fleet-wide restart loop. By
// hash, the release is applied once and then matches, whatever its version
// string says. The version string is only a fallback for when there is no hash
// to compare (none advertised, or the installed exe cannot be read).
func (u *UpdateChecker) isCurrent(rel protocol.AgentVersionResponse) bool {
	if rel.SHA256 != "" {
		got, err := fileSHA256(u.exePath)
		if err == nil {
			return strings.EqualFold(got, rel.SHA256)
		}
		log.Printf("auto-update: hash installed exe: %v (falling back to version compare)", err)
	}
	return rel.Version == u.currentVersion
}

// fileSHA256 returns the hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchVersion GETs /v1/agent/version with the device token.
func (u *UpdateChecker) fetchVersion(ctx context.Context) (protocol.AgentVersionResponse, error) {
	var out protocol.AgentVersionResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.serverURL+"/v1/agent/version", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	resp, err := u.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return out, nil // no release configured server-side: not an error
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("version check: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("version check: decode: %w", err)
	}
	return out, nil
}

// download streams /v1/agent/download to a staging file, verifies its sha256
// against the advertised hash, and returns the staging path. A hash mismatch
// deletes the file and errors — a corrupt or tampered binary is never launched.
func (u *UpdateChecker) download(ctx context.Context, rel protocol.AgentVersionResponse) (string, error) {
	stageDir := filepath.Join(u.dataDir, "update")
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return "", err
	}
	// Name by version so repeated attempts overwrite one file rather than piling up.
	dst := filepath.Join(stageDir, "monsvc-"+safeVersion(rel.Version)+".exe")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.serverURL+"/v1/agent/download", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: status %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return "", err
	}
	if rel.Size != 0 && n != rel.Size {
		os.Remove(dst)
		return "", fmt.Errorf("download: size %d, advertised %d", n, rel.Size)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if rel.SHA256 != "" && got != rel.SHA256 {
		os.Remove(dst)
		return "", fmt.Errorf("download: sha256 %s, advertised %s", got, rel.SHA256)
	}
	return dst, nil
}

// launchUpgrade starts the downloaded binary as a detached process that runs the
// existing `-upgrade -new <self>` path: it stops monsvc (releasing the lock on
// the installed exe), copies itself over the installed path, and restarts the
// service. Detached + new-process-group so it survives this process exiting
// under the very stop it issues.
func launchUpgrade(newExe string) error {
	cmd := exec.Command(newExe, "-upgrade", "-new", newExe)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
	return cmd.Start()
}

// safeVersion strips path separators and the like from a version string so it is
// safe to embed in a filename. The version is server-controlled, but a defensive
// scrub keeps a stray value from escaping the staging dir.
func safeVersion(v string) string {
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', ' ':
			return '_'
		}
		return r
	}
	out := make([]rune, 0, len(v))
	for _, r := range v {
		out = append(out, repl(r))
	}
	if len(out) == 0 {
		return "new"
	}
	return string(out)
}
