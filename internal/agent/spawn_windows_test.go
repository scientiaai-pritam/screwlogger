//go:build windows

package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// captureLogs redirects the standard library log into a buffer for the test's
// duration — the spawner's diagnostics ARE its observable behavior on a
// silent PC, because the event log is the only record a service leaves.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

type fakePipe struct {
	connected bool
	sddl      string
	ensures   []string
}

func (f *fakePipe) Connected() bool { return f.connected }

// EnsureSDDL records the grant and models the real PipeServer's contract: a
// session-user change drops the attached client so reconciliation can spawn
// the new user's watcher.
func (f *fakePipe) EnsureSDDL(sid string) {
	if sid != f.sddl {
		f.connected = false
	}
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
	captureLogs(t) // reconcile now narrates; keep the suite output clean
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

// A service that boots before logon must leave a readable trail: the event
// log is the only witness to whether it saw the session, spawned a watcher,
// and saw the watcher attach.
func TestSpawnerLogsSessionTransitions(t *testing.T) {
	buf := captureLogs(t)
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	s := newTestSpawner(fp, time.Now, rs)

	s.ConsoleSession = func() uint32 { return noConsoleSession }
	s.reconcile() // boot, nobody logged on
	s.ConsoleSession = func() uint32 { return 7 }
	s.reconcile() // the user logs in
	s.ConsoleSession = func() uint32 { return noConsoleSession }
	s.reconcile() // logoff

	out := buf.String()
	for _, want := range []string{"no console session", "session appeared", "session gone"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestSpawnerLogsWatcherAttachAndLaunch(t *testing.T) {
	buf := captureLogs(t)
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	t0 := time.Unix(1_800_000_000, 0)
	now := t0
	s := newTestSpawner(fp, func() time.Time { return now }, rs)

	s.reconcile() // spawns a watcher
	if n := strings.Count(buf.String(), "launched watcher"); n != 1 {
		t.Fatalf("launched-watcher lines = %d, want 1:\n%s", n, buf.String())
	}
	fp.connected = true
	s.reconcile() // the watcher dials the pipe
	if !strings.Contains(buf.String(), "watcher attached") {
		t.Fatalf("log missing \"watcher attached\":\n%s", buf.String())
	}
	fp.connected = false
	now = t0.Add(time.Second)
	s.reconcile() // watcher gone, respawn attempt
	out := buf.String()
	if !strings.Contains(out, "watcher gone") {
		t.Fatalf("log missing \"watcher gone\":\n%s", out)
	}
	if n := strings.Count(out, "launched watcher"); n != 2 {
		t.Fatalf("launched-watcher lines = %d, want 2:\n%s", n, out)
	}
}

// A service parked at the login screen repeats the same QueryToken failure
// every ladder step; one copy in the event log is enough, and a CHANGED
// error must be logged again.
func TestSpawnerRepeatedIdenticalErrorsLogOnce(t *testing.T) {
	buf := captureLogs(t)
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	now := time.Unix(1_800_000_000, 0)
	s := newTestSpawner(fp, func() time.Time { return now }, rs)
	queryErr := errors.New("access is denied")
	s.QueryToken = func(uint32) (windows.Token, error) { return 0, queryErr }

	s.reconcile()
	s.reconcile()
	s.reconcile()

	if n := strings.Count(buf.String(), "query token for session"); n != 1 {
		t.Fatalf("identical error logged %d times, want 1:\n%s", n, buf.String())
	}

	queryErr = errors.New("a different failure")
	s.reconcile()
	if n := strings.Count(buf.String(), "query token for session"); n != 2 {
		t.Fatalf("changed error not logged again (count %d):\n%s", n, buf.String())
	}
}

// While a watcher is attached, reconcile must still reach EnsureSDDL with the
// console session's user SID: it is a no-op while the SID is unchanged (this
// test) and drops the stale client on a fast user switch (the next test).
// Skipping the grant while connected was the Critical review finding — the
// only EnsureSDDL call site sat behind a guard that is precisely true during
// a fast user switch, so the new console user silently got zero heartbeats.
func TestSpawnerConnectedShortCircuits(t *testing.T) {
	captureLogs(t)
	fp := &fakePipe{connected: true, sddl: testSID}
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
	if len(fp.ensures) != 1 || fp.ensures[0] != testSID {
		t.Fatalf("ensures = %v, want one no-op grant for the attached session user", fp.ensures)
	}
}

func TestSpawnerSessionUserChangeDropsAndRespawns(t *testing.T) {
	captureLogs(t)
	fp := &fakePipe{connected: true, sddl: testSID} // user A's watcher attached
	rs := &recordingSpawner{}
	s := newTestSpawner(fp, time.Now, rs)
	s.TokenUser = func(windows.Token) (string, error) { return "S-1-5-21-200", nil } // fast user switch to B

	s.reconcile()

	if len(fp.ensures) != 1 || fp.ensures[0] != "S-1-5-21-200" {
		t.Fatalf("ensures = %v, want a grant for the new session user", fp.ensures)
	}
	if len(rs.spawns) != 1 {
		t.Fatalf("spawn calls: %v, want a watcher for the new session user", rs.spawns)
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

// TestSpawnerBootSequencePinsPostLogonSpawn pins the boot contract for the
// failing fleet path: a service that started before logon and ticked through
// the login screen's failing QueryTokens for hours must still spawn a watcher
// promptly after the user logs on, must not stay ladder-blocked, and must
// narrate the whole sequence in the log — the login screen's repeats deduped
// to one line.
func TestSpawnerBootSequencePinsPostLogonSpawn(t *testing.T) {
	buf := captureLogs(t)
	fp := &fakePipe{}
	rs := &recordingSpawner{}
	boot := time.Unix(1_800_000_000, 0)
	now := boot
	logonAt := boot.Add(4 * time.Hour)
	s := newTestSpawner(fp, func() time.Time { return now }, rs)

	queryErr := errors.New("no user token")
	s.QueryToken = func(uint32) (windows.Token, error) {
		if now.Before(logonAt) {
			return 0, queryErr // login screen: session up, no user yet
		}
		return windows.Token(0), nil
	}
	s.ConsoleSession = func() uint32 {
		if now.Equal(boot) {
			return noConsoleSession // first reconcile: session manager not up yet
		}
		return 1
	}

	s.reconcile() // boot
	spawnedAt := time.Time{}
	for now.Before(logonAt.Add(90 * time.Second)) {
		now = now.Add(30 * time.Second)
		s.reconcile()
		if len(rs.spawns) > 0 && spawnedAt.IsZero() {
			spawnedAt = now
			fp.connected = true // the freshly spawned watcher dials the pipe
		}
	}

	if len(rs.spawns) == 0 {
		t.Fatal("no watcher spawned after logon")
	}
	if spawnedAt.After(logonAt.Add(30 * time.Second)) {
		t.Fatalf("first spawn at %v, want within 30s of logon at %v", spawnedAt, logonAt)
	}
	out := buf.String()
	for _, want := range []string{"no console session", "session appeared", "launched watcher"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "query token for session"); n != 1 {
		t.Fatalf("login-screen failure logged %d times across ~480 ticks, want 1", n)
	}
	if s.ladderIdx != 0 {
		t.Fatalf("ladderIdx = %d after attach, want 0", s.ladderIdx)
	}
}

// TestSpawnIntoJobChildDiesWhenJobCloses pins the watcher lifecycle contract:
// a child created by spawnIntoJob belongs to the job, so closing the job
// handle kills it. This is what keeps a service restart or -upgrade from
// leaking the previous generation's watcher into the user's session.
func TestSpawnIntoJobChildDiesWhenJobCloses(t *testing.T) {
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	defer windows.CloseHandle(job)

	// Our own primary token exempts the test from SE_ASSIGNPRIMARYTOKEN_NAME;
	// the child lands in this process's session, like a real watcher would.
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY, &tok); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer tok.Close()

	exe := systemCmdExe(t)
	pi, err := spawnIntoJob(tok, exe, `"`+exe+`" /c ping -n 30 127.0.0.1 > nul`, nil, job)
	if err != nil {
		t.Fatalf("spawnIntoJob: %v", err)
	}
	defer func() {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
	}()

	// Running, not parked suspended: the child was resumed after assignment.
	if s, err := waitState(pi.Process, 0); err != nil || s != waitTimedOut {
		t.Fatalf("child state right after spawn: state=%d err=%v, want alive (WAIT_TIMEOUT)", s, err)
	}

	// Closing the job kills the child — the service-exit path.
	if err := windows.CloseHandle(job); err != nil {
		t.Fatalf("CloseHandle(job): %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := waitState(pi.Process, 500)
		if err == nil && s == waitSignaled {
			break // child exited when the job handle closed
		}
		if time.Now().After(deadline) {
			t.Fatalf("child still running 10s after job close (state=%d err=%v)", s, err)
		}
	}
	code, err := exitCode(pi.Process)
	if err != nil {
		t.Fatalf("exitCode: %v", err)
	}
	if code == stillActive {
		t.Fatalf("child exit code = STILL_ACTIVE after signaling")
	}
}

// systemCmdExe returns cmd.exe's full path.
func systemCmdExe(t *testing.T) string {
	t.Helper()
	dir, err := windows.GetSystemDirectory()
	if err != nil || dir == "" {
		t.Fatalf("GetSystemDirectory: %v", err)
	}
	return dir + `\cmd.exe`
}

// stillActive is GetExitCodeProcess's STILL_ACTIVE (259); x/sys does not
// export the constant.
const stillActive = 259

const (
	waitSignaled = uint32(windows.WAIT_OBJECT_0)
	waitTimedOut = uint32(windows.WAIT_TIMEOUT)
)

func waitState(h windows.Handle, timeoutMs uint32) (uint32, error) {
	s, err := windows.WaitForSingleObject(h, timeoutMs)
	return uint32(s), err
}

func exitCode(h windows.Handle) (uint32, error) {
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, err
	}
	return code, nil
}
