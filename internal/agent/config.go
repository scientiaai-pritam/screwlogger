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
	PollInterval         time.Duration `yaml:"-"`
	FlushInterval        time.Duration `yaml:"-"`
	MaxBufferBytes       int64         `yaml:"-"`
}

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
	if cfg.ServerURL == "" || cfg.Token == "" || cfg.DataDir == "" {
		return Config{}, fmt.Errorf("config: server_url, token and data_dir are required")
	}
	return cfg, nil
}
