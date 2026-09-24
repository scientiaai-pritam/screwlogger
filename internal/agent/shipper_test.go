package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"screwlogger/internal/agent"
	"screwlogger/internal/protocol"
)

func fill(t *testing.T, b *agent.Buffer, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := b.Append(protocol.Heartbeat{ID: string(rune('a' + i)), TS: int64(i), App: "a.exe", Category: "C", Active: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFlushPostsBatchAndAcks(t *testing.T) {
	var gotBatch protocol.IngestBatch
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBatch)
		json.NewEncoder(w).Encode(protocol.IngestResponse{Accepted: int64(len(gotBatch.Heartbeats))})
	}))
	defer srv.Close()

	p := filepath.Join(t.TempDir(), "buf.jsonl")
	buf, _ := agent.OpenBuffer(p, 1<<20)
	defer buf.Close()
	fill(t, buf, 3)

	sh := agent.NewShipper(srv.URL, "tok123", buf, srv.Client())
	n, err := sh.Flush(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if gotBatch.SchemaVersion != protocol.SchemaVersion || len(gotBatch.Heartbeats) != 3 {
		t.Fatalf("batch: %+v", gotBatch)
	}
	left, _ := buf.Unacked(10)
	if len(left) != 0 {
		t.Fatalf("buffer must be empty after ack, got %d", len(left))
	}
}

func TestFlushServerErrorAcksNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	buf, _ := agent.OpenBuffer(p, 1<<20)
	defer buf.Close()
	fill(t, buf, 2)
	sh := agent.NewShipper(srv.URL, "tok", buf, srv.Client())
	if _, err := sh.Flush(context.Background()); err == nil {
		t.Fatal("403 must return an error")
	}
	left, _ := buf.Unacked(10)
	if len(left) != 2 {
		t.Fatalf("failed flush must ack nothing, got %d", len(left))
	}
}

func TestFlushCapsBatchAt500(t *testing.T) {
	var batches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b protocol.IngestBatch
		json.NewDecoder(r.Body).Decode(&b)
		if len(b.Heartbeats) > 500 {
			t.Errorf("batch too large: %d", len(b.Heartbeats))
		}
		batches.Add(1)
		json.NewEncoder(w).Encode(protocol.IngestResponse{Accepted: int64(len(b.Heartbeats))})
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	buf, _ := agent.OpenBuffer(p, 1<<30)
	defer buf.Close()
	fill(t, buf, 1200)
	sh := agent.NewShipper(srv.URL, "tok", buf, srv.Client())
	n, err := sh.Flush(context.Background())
	if err != nil || n != 500 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestBackoffSchedule(t *testing.T) {
	sh := agent.NewShipper("http://localhost:1", "tok", nil, nil)
	if got := sh.NextInterval(nil); got != agent.HealthyFlushInterval {
		t.Fatalf("healthy interval: %v", got)
	}
	want := []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := sh.NextInterval(context.DeadlineExceeded); got != w {
			t.Fatalf("step %d: want %v got %v", i, w, got)
		}
	}
	if got := sh.NextInterval(nil); got != agent.HealthyFlushInterval {
		t.Fatalf("success must reset backoff, got %v", got)
	}
}
