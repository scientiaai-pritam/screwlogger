// Package protocol defines the wire contract between agent and server.
// It is the single source of truth for the ingest API shape.
package protocol

const (
	// SchemaVersion is the current ingest batch schema. Server rejects others (spec §5).
	SchemaVersion = 1
	// MaxHeartbeatGap is the dwell-time cap in seconds: 2x the agent keep-alive.
	// Heartbeat gaps longer than this are considered real absence (PC off, buffer drop).
	MaxHeartbeatGap = int64(30)
)

// Heartbeat is one observed state of a device. ID is a client UUID and the
// server's idempotency key (spec §3.3).
type Heartbeat struct {
	ID       string `json:"id"`
	TS       int64  `json:"ts"` // original event time, unix seconds
	App      string `json:"app"`
	Category string `json:"category"`
	Active   bool   `json:"active"`
	// Detail is an optional, app-scoped foreground context string: the browser
	// page/site for a browser, or the open document name for Photoshop. Empty
	// for every other app, and empty when title capture is disabled. Adding it
	// is backward-compatible — it is omitted from the wire when empty, and an
	// older server simply ignores the field (schema_version is unchanged).
	Detail string `json:"detail,omitempty"`
}

// AgentVersionResponse is the body of GET /v1/agent/version: the agent release
// the server currently advertises for auto-update. An agent whose own build
// version differs fetches GET /v1/agent/download and self-upgrades.
type AgentVersionResponse struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// IngestBatch is the body of POST /v1/ingest.
type IngestBatch struct {
	SchemaVersion int          `json:"schema_version"`
	DeviceSentAt  int64        `json:"device_sent_at"` // agent flush time → stored as agent_sent_at
	Heartbeats    []Heartbeat `json:"heartbeats"`
}

// IngestResponse is the 200 body of POST /v1/ingest.
type IngestResponse struct {
	Accepted int64 `json:"accepted"`
}
