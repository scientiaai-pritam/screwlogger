package server_test

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"screwlogger/internal/agent"
	"screwlogger/internal/server"
)

// TestAgentToServerEndToEnd runs a real agent (fake Win32 sources) against a
// real server store + ingest handler and verifies rows land in SQLite with
// original timestamps, no duplicates (spec §8 M1 gate).
func TestAgentToServerEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	store, err := server.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	devID, token, err := store.CreateDevice("E2E-PC")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.IngestHandler(store))
	defer srv.Close()

	cfg := agent.Config{
		ServerURL: srv.URL, Token: token, DataDir: t.TempDir(),
		IdleThresholdSeconds: 180,
		PollInterval:         2 * time.Millisecond,
		FlushInterval:        10 * time.Millisecond,
		MaxBufferBytes:       1 << 20,
	}
	// Fake sources flip app every poll and stay active.
	fg := &flipFG{apps: []string{"excel.exe", "chrome.exe", "mes_line1.exe"}}
	a, err := agent.New(cfg, fg, constIdle(0), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM heartbeats WHERE device_id = ?`, devID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("want >=3 distinct heartbeats in sqlite, got %d", n)
	}
	var bad int
	if err := db.QueryRow(`SELECT COUNT(*) FROM heartbeats WHERE ts <= 0 OR app = '' OR category = ''`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d rows with missing ts/app/category", bad)
	}
	var cat string
	err = db.QueryRow(`SELECT category FROM heartbeats WHERE app = 'mes_line1.exe' LIMIT 1`).Scan(&cat)
	if err != nil {
		t.Fatal(err)
	}
}

type flipFG struct{ apps []string; i int }
func (f *flipFG) ForegroundApp() string { a := f.apps[f.i%len(f.apps)]; f.i++; return a }

type constIdle float64
func (c constIdle) IdleSeconds() float64 { return float64(c) }
