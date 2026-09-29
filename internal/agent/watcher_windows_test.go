//go:build windows

package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type seqFG struct{ apps []string; i int }

func (f *seqFG) ForegroundApp() string { a := f.apps[f.i%len(f.apps)]; f.i++; return a }

type constIdle float64

func (c constIdle) IdleSeconds() float64 { return float64(c) }

// memConn records writes; failNext writes fail first (simulates a dropped pipe).
type memConn struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	failNext int
}

func (m *memConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		return 0, errors.New("pipe broken")
	}
	return m.buf.Write(p)
}

func (m *memConn) Close() error { return nil }

func (m *memConn) lines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := strings.TrimSuffix(m.buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func watcherDeps(conn *memConn) WatcherDeps {
	return WatcherDeps{
		FG:             &seqFG{apps: []string{"chrome.exe"}},
		Idle:           constIdle(10),
		OwnSession:     func() (uint32, error) { return 1, nil },
		ConsoleSession: func() uint32 { return 1 },
		Dial: func() (io.WriteCloser, error) {
			if conn == nil {
				return nil, errors.New("no pipe")
			}
			return conn, nil
		},
		DialInterval: 5 * time.Millisecond,
		DialBudget:   50 * time.Millisecond,
	}
}

func waitLines(t *testing.T, conn *memConn, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lines := conn.lines(); len(lines) >= n {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wanted %d sample lines, got %d", n, len(conn.lines()))
	return nil
}

func TestWatcherWritesSamples(t *testing.T) {
	conn := &memConn{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWatcher(ctx, 180, watcherDeps(conn)) }()

	lines := waitLines(t, conn, 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatcher: %v", err)
	}
	s, err := DecodeSample([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode %q: %v", lines[0], err)
	}
	if s.App != "chrome.exe" || !s.Active {
		t.Fatalf("sample = %+v", s)
	}
}

func TestWatcherPausesOffConsoleSession(t *testing.T) {
	conn := &memConn{}
	deps := watcherDeps(conn)
	deps.OwnSession = func() (uint32, error) { return 2, nil } // switched-out session
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWatcher(ctx, 180, deps) }()

	time.Sleep(1200 * time.Millisecond) // >1 tick: at least one poll cycle ran
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatcher: %v", err)
	}
	if lines := conn.lines(); len(lines) != 0 {
		t.Fatalf("paused watcher wrote samples: %v", lines)
	}
}

func TestWatcherExitsWhenPipeUnreachable(t *testing.T) {
	deps := watcherDeps(nil) // Dial always fails
	start := time.Now()
	err := RunWatcher(context.Background(), 180, deps)
	if err == nil {
		t.Fatal("expected error when pipe is unreachable")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("watcher hung %v past dial budget", elapsed)
	}
}

func TestWatcherReconnectsAfterWriteFailure(t *testing.T) {
	first := &memConn{failNext: 1}
	second := &memConn{}
	calls := 0
	deps := watcherDeps(first)
	deps.Dial = func() (io.WriteCloser, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWatcher(ctx, 180, deps) }()

	waitLines(t, second, 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatcher: %v", err)
	}
}

func TestWatcherExitOnCancelBeforeDial(t *testing.T) {
	slow := make(chan struct{})
	deps := watcherDeps(nil)
	deps.Dial = func() (io.WriteCloser, error) {
		<-slow // never ready
		return nil, errors.New("no pipe")
	}
	// Unblock dial on cancel so dialRetry's select can observe ctx.Done.
	go func() { <-time.After(50 * time.Millisecond); close(slow) }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWatcher(ctx, 180, deps) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel should exit cleanly, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWatcher ignored cancel")
	}
}
