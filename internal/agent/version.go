package agent

import "time"

// DefaultUpdateCheckInterval is how often the service polls the server for a
// newer agent build when auto-update is enabled.
const DefaultUpdateCheckInterval = 6 * time.Hour

// Version is the agent's build version, printed by -version and logged by
// auto-update. It is a var, not a const, so a release build can stamp it
// without editing source:
//
//	go build -ldflags "-X screwlogger/internal/agent.Version=2026.10.19-1" ./cmd/agent
//
// The string is informational: auto-update decides by comparing the installed
// binary's sha256 with the server's advertised hash, so a build whose Version
// does not match the server's -agent-version still updates exactly once and
// never loops. Version is compared only when no hash is available.
var Version = "dev"
