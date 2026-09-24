package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"screwlogger/internal/protocol"
)

// Flush intervals (spec §5): healthy 60s; failures back off 15s → 60s → 5min cap.
const (
	HealthyFlushInterval = 60 * time.Second
	batchLimit           = 500
)

var BackoffSteps = []time.Duration{15 * time.Second, 60 * time.Second, 5 * time.Minute}

// Shipper uploads buffered heartbeats to the server with device-token auth.
type Shipper struct {
	serverURL string
	token     string
	buf       *Buffer
	client    *http.Client
	failures  int
}

func NewShipper(serverURL, token string, buf *Buffer, client *http.Client) *Shipper {
	if client == nil {
		client = http.DefaultClient
	}
	return &Shipper{serverURL: serverURL, token: token, buf: buf, client: client}
}

// Flush posts up to 500 unacked heartbeats; on HTTP 200 acks them and returns
// the count. Any failure acks nothing (server is idempotent, so retries are safe).
func (s *Shipper) Flush(ctx context.Context) (int, error) {
	hbs, err := s.buf.Unacked(batchLimit)
	if err != nil {
		return 0, err
	}
	if len(hbs) == 0 {
		return 0, nil
	}
	batch := protocol.IngestBatch{
		SchemaVersion: protocol.SchemaVersion,
		DeviceSentAt:  time.Now().Unix(),
		Heartbeats:    hbs,
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.serverURL+"/v1/ingest", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ingest: unexpected status %d", resp.StatusCode)
	}
	if err := s.buf.Ack(len(hbs)); err != nil {
		return 0, err
	}
	return len(hbs), nil
}

// NextInterval returns how long to wait before the next flush attempt.
// A nil error (success) resets the backoff ladder.
func (s *Shipper) NextInterval(lastError error) time.Duration {
	if lastError == nil {
		s.failures = 0
		return HealthyFlushInterval
	}
	step := s.failures
	if step >= len(BackoffSteps) {
		step = len(BackoffSteps) - 1
	}
	s.failures++
	return BackoffSteps[step]
}
