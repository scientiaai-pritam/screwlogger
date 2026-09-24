//go:build windows

package agent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"gopkg.in/yaml.v3"
)

// WriteConfig writes agent.yaml next to the binary. dataDir empty →
// C:\ProgramData\monsvc. It MUST NOT log the token (no log statement touches
// the token).
func WriteConfig(exeDir, serverURL, token, dataDir string) error {
	if dataDir == "" {
		dataDir = `C:\ProgramData\monsvc`
	}
	cfg := Config{
		ServerURL:            serverURL,
		Token:                token,
		DataDir:              dataDir,
		IdleThresholdSeconds: 180,
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(exeDir, "agent.yaml"), b, 0o600)
}

// InstallService registers name as an automatic, auto-recovering SYSTEM service.
func InstallService(exePath, name, displayName, desc string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.CreateService(name, exePath, mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  displayName,
		Description:  desc,
	}, "-service")
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.SetRecoveryActions(recoveryActions(), 86400); err != nil {
		return err
	}
	// Required so the handler's nonzero exit (Task 1 fatal path) also triggers
	// a restart: by default SCM recovery only fires on a crash that never
	// reports Stopped.
	return s.SetRecoveryActionsOnNonCrashFailures(true)
}

// UninstallService stops (best-effort) and deletes the named service.
func UninstallService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil && err != windows.ERROR_SERVICE_NOT_ACTIVE {
		return err
	}
	return s.Delete()
}

// StartService starts the named service.
func StartService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	return s.Start()
}

// StopService stops the named service, ignoring a not-running state.
func StopService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil && err != windows.ERROR_SERVICE_NOT_ACTIVE {
		return err
	}
	return nil
}

// UpgradeService stops, optionally replaces the binary, and restarts.
// agent.yaml (config + token) is untouched.
func UpgradeService(name, newExePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil && err != windows.ERROR_SERVICE_NOT_ACTIVE {
		return err
	}
	// Wait until actually stopped so the exe file is unlocked before copy.
	if err := waitStopped(s, 30*time.Second); err != nil {
		return err
	}

	if newExePath != "" {
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		if err := copyFile(newExePath, cleanBinaryPath(cfg.BinaryPathName)); err != nil {
			return err
		}
	}

	return s.Start()
}

// recoveryActions returns the restart ladder: 1s, 15s, 60s (Decision 5).
func recoveryActions() []mgr.RecoveryAction {
	return []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}
}

// cleanBinaryPath strips the installer-appended " -service" arg and
// surrounding quotes.
func cleanBinaryPath(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndex(s, " -service"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	return strings.Trim(s, "\"")
}

// copyFile copies src to dst, truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// waitStopped polls the service until it reports Stopped or timeout elapses.
func waitStopped(s *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err == nil && st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("service did not stop within timeout")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
