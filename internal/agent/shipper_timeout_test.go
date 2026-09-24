package agent

import "testing"

// TestNewShipperDefaultClientHasTimeout guards the fix for the final-review
// Important finding: a nil client must fall back to a client WITH a timeout,
// not http.DefaultClient (whose zero Timeout lets a half-open connection hang
// Flush indefinitely, stalling the poller via the agent mutex).
func TestNewShipperDefaultClientHasTimeout(t *testing.T) {
	sh := NewShipper("http://localhost:1", "tok", nil, nil)
	if sh.client == nil {
		t.Fatal("default client must not be nil")
	}
	if sh.client.Timeout <= 0 {
		t.Fatalf("default client must have a positive timeout, got %v", sh.client.Timeout)
	}
}
