package agent

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the agent's on-disk configuration. Poll/Flush intervals and
// MaxBufferBytes are code-level defaults, overridable by tests.
type Config struct {
	ServerURL            string        `yaml:"server_url"`
	Token                string        `yaml:"token"`
	DataDir              string        `yaml:"data_dir"`
	IdleThresholdSeconds int           `yaml:"idle_threshold_seconds"`
	// CaptureTitles enables the app-scoped foreground-context capture (browser
	// page / Photoshop document) described in protocol.Heartbeat.Detail. It is a
	// pointer so an absent key defaults to on (ShouldCaptureTitles); set
	// `capture_titles: false` to disable.
	CaptureTitles *bool `yaml:"capture_titles"`
	// AutoUpdate enables the agent's periodic check against the server's
	// /v1/agent/version endpoint and self-upgrade. Pointer semantics as above:
	// absent defaults to on, `auto_update: false` disables it.
	AutoUpdate          *bool         `yaml:"auto_update"`
	PollInterval        time.Duration `yaml:"-"`
	FlushInterval       time.Duration `yaml:"-"`
	MaxBufferBytes      int64         `yaml:"-"`
	UpdateCheckInterval time.Duration `yaml:"-"`
}

// ShouldCaptureTitles reports whether app-scoped title capture is on. Absent
// config (nil) defaults to on.
func (c Config) ShouldCaptureTitles() bool { return c.CaptureTitles == nil || *c.CaptureTitles }

// AutoUpdateEnabled reports whether the agent self-upgrades. Absent config (nil)
// defaults to on.
func (c Config) AutoUpdateEnabled() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

// LoadConfig reads the agent YAML config and applies defaults (spec §3.1: idle 180s).
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.IdleThresholdSeconds == 0 {
		cfg.IdleThresholdSeconds = 180
	}
	cfg.PollInterval = time.Second
	cfg.FlushInterval = HealthyFlushInterval
	cfg.MaxBufferBytes = 16 << 20
	cfg.UpdateCheckInterval = DefaultUpdateCheckInterval
	if cfg.ServerURL == "" || cfg.Token == "" || cfg.DataDir == "" {
		return Config{}, fmt.Errorf("config: server_url, token and data_dir are required")
	}
	return cfg, nil
}
