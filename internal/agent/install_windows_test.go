//go:build windows

package agent

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc/mgr"
)

func TestWriteConfig(t *testing.T) {
	t.Run("default data dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteConfig(dir, "http://srv:8080", "sl_abc", ""); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != "http://srv:8080" || cfg.Token != "sl_abc" || cfg.DataDir != `C:\ProgramData\monsvc` {
			t.Fatalf("cfg = %+v, want server/token/default data dir", cfg)
		}
	})

	t.Run("explicit data dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteConfig(dir, "http://srv:8080", "sl_abc", `C:\monsvc\data`); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != "http://srv:8080" || cfg.Token != "sl_abc" || cfg.DataDir != `C:\monsvc\data` {
			t.Fatalf("cfg = %+v, want server/token/explicit data dir", cfg)
		}
	})
}

func TestRecoveryActions(t *testing.T) {
	got := recoveryActions()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	wantDelays := []time.Duration{time.Second, 15 * time.Second, time.Minute}
	for i, a := range got {
		if a.Type != mgr.ServiceRestart {
			t.Fatalf("action %d type = %d, want mgr.ServiceRestart", i, a.Type)
		}
		if a.Delay != wantDelays[i] {
			t.Fatalf("action %d delay = %v, want %v", i, a.Delay, wantDelays[i])
		}
	}
}

func TestCleanBinaryPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"C:\Program Files\monsvc\monsvc.exe" -service`, `C:\Program Files\monsvc\monsvc.exe`},
		{`C:\monsvc\monsvc.exe -service`, `C:\monsvc\monsvc.exe`},
		{`"C:\x.exe"`, `C:\x.exe`},
	}
	for _, c := range cases {
		if got := cleanBinaryPath(c.in); got != c.want {
			t.Fatalf("cleanBinaryPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
