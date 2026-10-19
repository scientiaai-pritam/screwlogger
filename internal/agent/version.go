package agent

import "time"

// DefaultUpdateCheckInterval is how often the service polls the server for a
// newer agent build when auto-update is enabled.
const DefaultUpdateCheckInterval = 6 * time.Hour

// Version is the agent's build version, reported to the server's auto-update
// endpoint to decide whether a newer binary is available. It is a var, not a
// const, so a release build can stamp it without editing source:
//
//	go build -ldflags "-X screwlogger/internal/agent.Version=2026.10.19-1" ./cmd/agent
//
// The string is opaque to the agent: auto-update triggers when the server
// advertises a version different from this one, so the only requirement is that
// each distinct build carries a distinct Version and the server is told the same
// string via its -agent-version flag.
var Version = "dev"
