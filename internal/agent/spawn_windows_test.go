//go:build windows

package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type fakePipe struct {
	connected bool
	sddl      string
	ensures   []string
}

func (f *fakePipe) Connected() bool { return f.connected }
func (f *fakePipe) EnsureSDDL(sid string) {
	f.sddl = sid
	f.ensures = append(f.ensures, sid)
}

type recordingSpawner struct {
	spawns [][]string
	err    error
}

func (r *recordingSpawner) spawn(_ windows.Token, exe string, args []string) error {
	r.spawns = append(r.spawns, args)
	return r.err
}

const testSID = "S-1-5-21-100"

func newTestSpawner(fp *fakePipe, now func() time.Time, rs *recordingSpawner) *Spawner {
	s := NewSpawner(`C:\ProgramData\monsvc\monsvc.exe`, 180, nil)
	s.Pipe = fp
	s.ConsoleSession = func() uint32 { return 7 }
	s.QueryToken = func(uint32) (windows.Token, error) { return windows.Token(0), nil }
	s.TokenUser = func(windows.Token) (string, error) { return testSID, nil }
	s.Spawn = rs.spawn
	s.Now = now
	return s
}

func TestSpawnerLaunchesWatcher(t *testing.T) {
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	t0 := time.Unix(1_800_000_000, 0)
	s := newTestSpawner(fp, func() time.Time { return t0 }, rs)

	s.reconcile()

	if len(rs.spawns) != 1 {
		t.Fatalf("spawn calls: %v", rs.spawns)
	}
	want := []string{"-userwatch", "-idle", "180"}
	for i, a := range want {
		if rs.spawns[0][i] != a {
			t.Fatalf("args = %v, want %v", rs.spawns[0], want)
		}
	}
	if fp.sddl != testSID {
		t.Fatalf("EnsureSDDL never saw %q (got %q)", testSID, fp.sddl)
	}
	if !s.nextAttempt.Equal(t0.Add(time.Second)) || s.ladderIdx != 1 {
		t.Fatalf("after first attempt: nextAttempt=%v ladderIdx=%d", s.nextAttempt, s.ladderIdx)
	}
}

func TestSpawnerNoConsoleSession(t *testing.T) {
	for _, sid := range []uint32{noConsoleSession, 0} {
		fp := &fakePipe{}
		rs := &recordingSpawner{}
		s := newTestSpawner(fp, time.Now, rs)
		s.ConsoleSession = func() uint32 { return sid }
		s.reconcile()
		if len(rs.spawns) != 0 || len(fp.ensures) != 0 {
			t.Fatalf("session %d: spawned %v ensures %v", sid, rs.spawns, fp.ensures)
		}
	}
}

func TestSpawnerConnectedShortCircuits(t *testing.T) {
	fp := &fakePipe{connected: true}
	rs := &recordingSpawner{}
	s := newTestSpawner(fp, time.Now, rs)
	s.ladderIdx = 2 // ladder must reset once a watcher is attached
	s.reconcile()
	if len(rs.spawns) != 0 {
		t.Fatalf("spawned while connected: %v", rs.spawns)
	}
	if s.ladderIdx != 0 {
		t.Fatalf("ladderIdx = %d, want reset to 0", s.ladderIdx)
	}
}

func TestSpawnerBackoffLadder(t *testing.T) {
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	now := time.Unix(1_800_000_000, 0)
	s := newTestSpawner(fp, func() time.Time { return now }, rs)

	s.reconcile() // attempt 1: next in 1s, idx 1
	s.reconcile() // too early: no spawn
	if len(rs.spawns) != 1 {
		t.Fatalf("spawned inside backoff: %v", rs.spawns)
	}
	now = now.Add(time.Second)
	s.reconcile() // attempt 2: next in 5s, idx 2
	now = now.Add(4 * time.Second)
	s.reconcile() // still inside 5s gap
	if len(rs.spawns) != 2 {
		t.Fatalf("spawned inside 5s gap: %v", rs.spawns)
	}
	now = now.Add(time.Second)
	s.reconcile() // attempt 3: next in 30s, idx capped at 2
	now = now.Add(29 * time.Second)
	s.reconcile() // inside 30s cap
	if len(rs.spawns) != 3 {
		t.Fatalf("spawned inside 30s cap: %v", rs.spawns)
	}
	now = now.Add(time.Second)
	s.reconcile() // attempt 4 allowed, idx stays capped
	if len(rs.spawns) != 4 {
		t.Fatalf("ladder did not release after cap: %v", rs.spawns)
	}
	if s.ladderIdx != 2 {
		t.Fatalf("ladderIdx = %d, want capped 2", s.ladderIdx)
	}
}

func TestSpawnerTokenErrorConsumesLadder(t *testing.T) {
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	now := time.Unix(1_800_000_000, 0)
	s := newTestSpawner(fp, func() time.Time { return now }, rs)
	s.QueryToken = func(uint32) (windows.Token, error) { return 0, errors.New("access denied") }

	s.reconcile()
	if len(rs.spawns) != 0 || len(fp.ensures) != 0 {
		t.Fatalf("token error must not spawn or grant DACL: %v %v", rs.spawns, fp.ensures)
	}
	if s.ladderIdx != 1 {
		t.Fatalf("ladderIdx = %d, want 1 (consumed)", s.ladderIdx)
	}
}

func TestSpawnerPokeIsNonBlocking(t *testing.T) {
	s := NewSpawner("x", 180, nil)
	s.Pipe = &fakePipe{}
	s.ConsoleSession = func() uint32 { return noConsoleSession }
	s.QueryToken = func(uint32) (windows.Token, error) { return 0, nil }
	s.TokenUser = func(windows.Token) (string, error) { return testSID, nil }
	s.Spawn = func(windows.Token, string, []string) error { return nil }
	s.Now = time.Now
	s.Poke()
	s.Poke() // must not block or panic when the channel is full
	if len(s.poke) != 1 {
		t.Fatalf("poke channel len = %d, want 1 (deduped)", len(s.poke))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	s.Poke() // Run must consume it and keep going
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit on cancel")
	}
}
