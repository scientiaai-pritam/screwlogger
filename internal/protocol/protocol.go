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
