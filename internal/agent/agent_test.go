package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"screwlogger/internal/agent"
	"screwlogger/internal/protocol"
)

type fakeFG struct{ apps []string; i int }
func (f *fakeFG) ForegroundApp() string { a := f.apps[f.i%len(f.apps)]; f.i++; return a }

type fakeIdle struct{ seconds float64 }
func (f *fakeIdle) IdleSeconds() float64 { return f.seconds }

func TestAgentRunsAndShipsHeartbeats(t *testing.T) {
	var mu sync.Mutex
	var received []protocol.Heartbeat
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")
		var b protocol.IngestBatch
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		received = append(received, b.Heartbeats...)
		mu.Unlock()
		json.NewEncoder(w).Encode(protocol.IngestResponse{Accepted: int64(len(b.Heartbeats))})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := agent.Config{
		ServerURL: srv.URL, Token: "testtoken", DataDir: dir,
		IdleThresholdSeconds: 180,
		PollInterval:         5 * time.Millisecond,
		// 75ms, not the brief's 30ms: on Windows a cold first fsync (~25ms)
		// plus timer jitter can leave only one append done when a 30ms flush
		// fires, and after a successful flush the next attempt is 60s out —
		// the test then sees a single shipped heartbeat. 75ms guarantees two
		// appends precede the first flush.
		FlushInterval:        75 * time.Millisecond,
		MaxBufferBytes:       1 << 20,
	}
	fg := &fakeFG{apps: []string{"excel.exe", "chrome.exe"}}
	a, err := agent.New(cfg, fg, &fakeIdle{seconds: 0}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 2 {
		t.Fatalf("expected >=2 heartbeats shipped, got %d", len(received))
	}
	for _, hb := range received {
		if hb.ID == "" || hb.App == "" || hb.Category == "" {
			t.Fatalf("heartbeat missing id/category: %+v", hb)
		}
	}
	if gotToken != "Bearer testtoken" {
		t.Fatalf("token header: %q", gotToken)
	}
	// Buffer must be fully acked (all shipped), file exists.
	if _, err := os.Stat(filepath.Join(dir, "buffer.jsonl")); err != nil {
		t.Fatalf("buffer file: %v", err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	os.WriteFile(p, []byte("server_url: http://10.0.0.5:8080\ntoken: sl_abc\ndata_dir: C:\\monsvc\n"), 0o600)
	cfg, err := agent.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleThresholdSeconds != 180 || cfg.PollInterval != time.Second {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.ServerURL != "http://10.0.0.5:8080" || cfg.Token != "sl_abc" {
		t.Fatalf("fields wrong: %+v", cfg)
	}
}
