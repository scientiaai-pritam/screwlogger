# User-Session Watcher Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the Session 0 blindness (100% `unknown.exe`/`active=0` from service installs) by moving Win32 polling into a small watcher process that the monsvc service spawns inside the active console session, feeding samples to the unchanged builder → categorize → buffer → shipper pipeline over a local named pipe.

**Architecture:** Two processes on each PC: the SYSTEM service (token, lossless buffer, shipper, pipe server, spawner) and a token-free `monsvc.exe -userwatch` process in the user session (1 Hz foreground/idle polling, JSON samples over `\\.\pipe\monsvc-samples`). Console mode is unchanged for development. Server and ingest protocol are untouched.

**Tech Stack:** Go (existing module), `golang.org/x/sys/windows` v0.30+ (verified: `WTSGetActiveConsoleSessionId`, `WTSQueryUserToken`, `ProcessIdToSessionId`, `CreateProcessAsUser`, `CreateNamedPipe`, `ConnectNamedPipe`, `CancelIoEx`, `GetTokenUser` (method on `Token`), `SecurityDescriptorFromString`, `PIPE_ACCESS_INBOUND`, `PIPE_UNLIMITED_INSTANCES`, `CREATE_NO_WINDOW`), lazy `userenv.dll` procs for `CreateEnvironmentBlock`/`DestroyEnvironmentBlock`.

**Spec:** `docs/superpowers/specs/2026-09-29-user-session-watcher-design.md` — read it together with this plan; behavior matrices and non-goals live there.

## Global Constraints

- Server code (`internal/server`), protocol schema, and ingest API must not change.
- Watcher code paths must never read `agent.yaml`, the token, or the disk: no config file, no writes, no buffer.
- Watcher starts with no window: `CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT`; no tray icon.
- Pipe name exactly `\\.\pipe\monsvc-samples`; DACL exactly `D:P(A;;GA;;;SY)(A;;GA;;;BA)` plus `(A;;GW;;;<session-user-SID>)`.
- Backoff ladder exactly 1 s → 5 s → 30 s (cap); spawner reconcile tick 30 s; watcher connect retry 500 ms interval / 15 s budget; poll 1 Hz; `-idle` threshold passed by the service from `cfg.IdleThresholdSeconds`.
- New Win32 files carry `//go:build windows` and live in `internal/agent` next to their siblings; `sample.go` is platform-neutral.
- No logon / no console session → spawn nothing, ship nothing (spec §5).
- Console mode (`-config agent.yaml`) behavior is unchanged.
- All tests use stdlib `testing` (no testify), matching existing test style; tests for windows-only files use internal package `agent`, agent-level tests keep the external `agent_test` package.
- Resource budget: watcher ≈ old poller (~10–15 MB, <1% CPU) — no polling tighter than 1 Hz anywhere.

## Review Focus

Failure modes the spec implies but that bite in production; each is pinned by a test in its owning task:

1. **Watcher spins while the pipe is missing** (service down, upgrading) — must retry bounded and exit, never busy-loop. Pinned: Task 4 `TestWatcherExitsWhenPipeUnreachable` (dial budget → return) and `TestWatcherReconnectsAfterWriteFailure` (write-failure → re-dial, bounded interval).
2. **Stale watcher blocks a new session's data** (fast user switch: old watcher still connected, new user's watcher can't attach) — a session-user-SID change must drop the current client so reconciliation can spawn the new watcher. Pinned: Task 3 `TestPipeEnsureSDDLDropsClient` + Task 5 `TestSpawnerLaunchesWatcher` (EnsureSDDL called before spawn).
3. **Garbage on the pipe must never crash the service or reach ingest** — malformed lines logged and skipped, later good lines flow. Pinned: Task 3 `TestPipeMalformedLinesSkipped`.
4. **A user repeatedly killing the watcher must not thrash the CPU** — respawn gaps grow 1 s → 5 s → 30 s and cap. Pinned: Task 5 `TestSpawnerBackoffLadder`.
5. **Service shutdown must close the buffer exactly once and stop cleanly** — `RunSamples` cancel drains the shipper and closes the buffer; heartbeats already on disk are intact. Pinned: Task 2 `TestRunSamplesStopsOnCancel`.

---

### Task 1: Sample codec (`internal/agent/sample.go`)

**Files:**
- Create: `internal/agent/sample.go`
- Test: `internal/agent/sample_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Sample struct { App string; Active bool }`, `func EncodeSample(s Sample) []byte`, `func DecodeSample(line []byte) (Sample, error)` — used by Tasks 3, 4, and 6.

- [ ] **Step 1: Write the failing test**

```go
package agent_test

import (
	"strings"
	"testing"

	"screwlogger/internal/agent"
)

func TestEncodeSampleIsJSONLine(t *testing.T) {
	b := agent.EncodeSample(agent.Sample{App: "chrome.exe", Active: true})
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("encoded sample lacks trailing newline: %q", b)
	}
	if string(b) != `{"app":"chrome.exe","active":true}`+"\n" {
		t.Fatalf("unexpected encoding: %q", b)
	}
}

func TestEncodeSampleEscapesApp(t *testing.T) {
	b := agent.EncodeSample(agent.Sample{App: `we"irdé.exe`, Active: false})
	s, err := agent.DecodeSample(b)
	if err != nil {
		t.Fatalf("decode own encoding: %v", err)
	}
	if s.App != `we"irdé.exe` || s.Active {
		t.Fatalf("roundtrip mismatch: %+v", s)
	}
}

func TestDecodeSampleRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":        nil,
		"blank":        []byte(""),
		"garbage":      []byte("hello\n"),
		"missing app":  []byte(`{"active":true}`),
		"empty app":    []byte(`{"app":"","active":true}`),
		"wrong type":   []byte(`{"app":5}`),
	}
	for name, in := range cases {
		if s, err := agent.DecodeSample(in); err == nil {
			t.Fatalf("%s: expected error, got %+v", name, s)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestEncodeSample -v` (and `-run TestDecodeSample`)
Expected: FAIL — `undefined: agent.EncodeSample`

- [ ] **Step 3: Write minimal implementation**

```go
package agent

import (
	"encoding/json"
	"fmt"
)

// Sample is one in-session observation the watcher ships to the service over
// the local pipe (spec §4.1).
type Sample struct {
	App    string `json:"app"`
	Active bool   `json:"active"`
}

// EncodeSample renders one sample as a JSON line (trailing newline included).
func EncodeSample(s Sample) []byte {
	b, _ := json.Marshal(s) // Sample has no marshal failure mode
	return append(b, '\n')
}

// DecodeSample parses one JSON line. Empty lines, malformed JSON, and samples
// without an app name are rejected; the caller logs and skips them.
func DecodeSample(line []byte) (Sample, error) {
	var s Sample
	if len(line) == 0 {
		return s, fmt.Errorf("empty sample line")
	}
	if err := json.Unmarshal(line, &s); err != nil {
		return s, fmt.Errorf("decode sample: %w", err)
	}
	if s.App == "" {
		return s, fmt.Errorf("sample without app")
	}
	return s, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run "TestEncodeSample|TestDecodeSample" -v`
Expected: PASS (all three)

- [ ] **Step 5: Commit**

```bash
git add internal/agent/sample.go internal/agent/sample_test.go
git commit -m "feat(agent): sample codec for watcher-to-service pipe"
```

---

### Task 2: Agent refactor — extract `Observe`, add `RunSamples`

**Files:**
- Modify: `internal/agent/agent.go` (replace `pollOnce` body; add `Observe` and `RunSamples`)
- Test: `internal/agent/agent_test.go` (append two tests)

**Interfaces:**
- Consumes: `Sample` (Task 1), existing `Agent` internals (`builder`, `rules`, `buf`, `mu`, `shipLoop`).
- Produces: `func (a *Agent) Observe(app string, active bool)` and `func (a *Agent) RunSamples(ctx context.Context, samples <-chan Sample) error` — used by Task 6's service wiring.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/agent_test.go` (fakes `fakeFG`/`fakeIdle` already exist in this file):

```go
func TestRunSamplesBuildsAndShipsHeartbeats(t *testing.T) {
	var mu sync.Mutex
	var received []protocol.Heartbeat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b protocol.IngestBatch
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		received = append(received, b.Heartbeats...)
		mu.Unlock()
		json.NewEncoder(w).Encode(protocol.IngestResponse{Accepted: int64(len(b.Heartbeats))})
	}))
	defer srv.Close()

	a, err := agent.New(agent.Config{
		ServerURL: srv.URL, Token: "tok", DataDir: t.TempDir(),
		IdleThresholdSeconds: 180,
		FlushInterval:        75 * time.Millisecond,
		MaxBufferBytes:       1 << 20,
	}, &fakeFG{apps: []string{"never.exe"}}, &fakeIdle{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	samples := make(chan agent.Sample, 8)
	samples <- agent.Sample{App: "excel.exe", Active: true}
	samples <- agent.Sample{App: "chrome.exe", Active: false}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := a.RunSamples(ctx, samples); err != nil {
		t.Fatalf("RunSamples: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 2 {
		t.Fatalf("want >=2 shipped heartbeats, got %d", len(received))
	}
	if received[0].App != "excel.exe" || !received[0].Active || received[0].ID == "" || received[0].Category == "" {
		t.Fatalf("first heartbeat wrong: %+v", received[0])
	}
	found := false
	for _, hb := range received {
		if hb.App == "chrome.exe" && !hb.Active {
			found = true
		}
	}
	if !found {
		t.Fatalf("chrome.exe inactive heartbeat missing: %+v", received)
	}
}

func TestRunSamplesStopsOnCancel(t *testing.T) {
	a, err := agent.New(agent.Config{
		ServerURL: "http://127.0.0.1:1", Token: "tok", DataDir: t.TempDir(),
		IdleThresholdSeconds: 180,
		FlushInterval:        time.Hour,
		MaxBufferBytes:       1 << 20,
	}, &fakeFG{apps: []string{"x.exe"}}, &fakeIdle{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	samples := make(chan agent.Sample)
	done := make(chan error, 1)
	go func() { done <- a.RunSamples(ctx, samples) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSamples after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSamples did not return after cancel")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestRunSamples -v`
Expected: FAIL — `a.RunSamples undefined (type *agent.Agent has no field or method RunSamples)`

- [ ] **Step 3: Implement**

In `internal/agent/agent.go`, replace the existing `pollOnce` function with:

```go
// Observe feeds one foreground/active reading through the heartbeat builder,
// categorizer, and buffer. Console mode calls it per tick; service mode calls
// it per pipe sample.
func (a *Agent) Observe(app string, active bool) {
	hb := a.builder.Observe(app, active)
	if hb == nil {
		return
	}
	hb.ID = uuid.NewString()
	hb.Category = a.rules.Categorize(app)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.buf.Append(*hb); err != nil {
		log.Printf("buffer append: %v", err)
	}
}

func (a *Agent) pollOnce() {
	a.Observe(a.fg.ForegroundApp(), a.idle.IdleSeconds() < float64(a.cfg.IdleThresholdSeconds))
}
```

And add `RunSamples` after `Run`:

```go
// RunSamples consumes watcher samples until ctx is cancelled, shipping in a
// separate goroutine (service mode). Samples arrive over the local pipe; no
// samples — watcher dead, no user logged on — means no heartbeats, so the
// device reports offline (spec §5).
func (a *Agent) RunSamples(ctx context.Context, samples <-chan Sample) error {
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.buf.Close()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.shipLoop(ctx)
	}()
	for {
		select {
		case <-ctx.Done():
			<-done // let the shipper finish its current attempt
			return nil
		case s := <-samples:
			a.Observe(s.App, s.Active)
		}
	}
}
```

- [ ] **Step 4: Run full agent package tests**

Run: `go test ./internal/agent/ -v`
Expected: PASS — new tests plus the untouched `TestAgentRunsAndShipsHeartbeats` (proves `Observe` extraction preserved console-mode behavior)

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_test.go
git commit -m "refactor(agent): extract Observe and add sample-driven RunSamples"
```

---

### Task 3: Pipe server (`internal/agent/pipe_windows.go`)

**Files:**
- Create: `internal/agent/pipe_windows.go`
- Test: `internal/agent/pipe_windows_test.go`

**Interfaces:**
- Consumes: `Sample`, `EncodeSample`, `DecodeSample` (Task 1).
- Produces: `const samplePipeName = "\\\\.\\pipe\\monsvc-samples"`, `type PipeServer` with `func NewPipeServer(name string) *PipeServer`, `func (p *PipeServer) Connected() bool`, `func (p *PipeServer) EnsureSDDL(userSID string)`, `func (p *PipeServer) Serve(ctx context.Context, out chan<- Sample)`. Used by Tasks 5 and 6.

Note: these tests create real named pipes and dial them; they require the test process to hold an admin-group token for the `BA` ACE to permit dialing (true on this dev machine).

- [ ] **Step 1: Write the failing tests**

```go
//go:build windows

package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func testPipeName() string {
	return fmt.Sprintf(`\\.\pipe\monsvc-test-%d-%d`, os.Getpid(), time.Now().UnixNano()%1_000_000)
}

// dialPipe opens a write handle to a pipe server, retrying while the server
// brings its pending instance up.
func dialPipe(t *testing.T, name string) io.WriteCloser {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(name),
			windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			return os.NewFile(uintptr(h), name)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipe %s never became available: %v", name, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readSample(t *testing.T, out <-chan Sample) Sample {
	t.Helper()
	select {
	case s := <-out:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no sample within 5s")
		return Sample{}
	}
}

func waitConnected(t *testing.T, p *PipeServer, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Connected() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Connected() still %v, want %v", p.Connected(), want)
}

func startTestPipe(t *testing.T) (*PipeServer, chan Sample, string) {
	t.Helper()
	name := testPipeName()
	p := NewPipeServer(name)
	out := make(chan Sample, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Serve(ctx, out)
	return p, out, name
}

func TestPipeRoundtrip(t *testing.T) {
	p, out, name := startTestPipe(t)
	c := dialPipe(t, name)
	waitConnected(t, p, true)
	if _, err := c.Write(EncodeSample(Sample{App: "chrome.exe", Active: true})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "chrome.exe" || !s.Active {
		t.Fatalf("sample = %+v", s)
	}
	if _, err := c.Write(EncodeSample(Sample{App: "excel.exe", Active: false})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "excel.exe" || s.Active {
		t.Fatalf("sample = %+v", s)
	}
}

func TestPipeMalformedLinesSkipped(t *testing.T) {
	_, out, name := startTestPipe(t)
	c := dialPipe(t, name)
	bad := []byte("garbage\n" + "\n" + `{"nope":1}` + "\n")
	if _, err := c.Write(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(EncodeSample(Sample{App: "good.exe", Active: true})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "good.exe" {
		t.Fatalf("expected good.exe to survive garbage, got %+v", s)
	}
}

func TestPipeNewestWins(t *testing.T) {
	p, out, name := startTestPipe(t)
	c1 := dialPipe(t, name)
	waitConnected(t, p, true)

	c2 := dialPipe(t, name) // second client: server must drop c1
	if _, err := c2.Write(EncodeSample(Sample{App: "new.exe", Active: true})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "new.exe" {
		t.Fatalf("sample = %+v", s)
	}

	// c1 must become unwritable once the server dropped it (poll: the write
	// may land in an OS buffer once before the connection breaks).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := c1.Write(EncodeSample(Sample{App: "old.exe"})); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old client still writable after newest-wins drop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPipeEnsureSDDLDropsClient(t *testing.T) {
	p, _, name := startTestPipe(t)
	_ = dialPipe(t, name)
	waitConnected(t, p, true)

	// A valid SID that is not ours: the DACL is replaced (not merged), the
	// current client is dropped, and the server accepts again under the new
	// DACL (this process's admin-group ACE still permits dialing).
	p.EnsureSDDL("S-1-5-99-12345")
	waitConnected(t, p, false)
	c := dialPipe(t, name)
	waitConnected(t, p, true)
	_ = c
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestPipe -v`
Expected: FAIL — `undefined: NewPipeServer`

- [ ] **Step 3: Write the implementation**

```go
//go:build windows

package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// samplePipeName is the local pipe in-session watchers write samples to.
const samplePipeName = `\\.\pipe\monsvc-samples`

// baseSD denies everyone by default and grants full access to SYSTEM and
// Administrators; EnsureSDDL appends a GENERIC_WRITE ACE for the console
// session user's SID (spec §4.3).
const baseSD = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"

// PipeServer accepts watcher connections (newest wins) and publishes decoded
// samples to a channel. All state is guarded by mu; Serve owns the accept
// loop, EnsureSDDL may run on the spawner's goroutine.
type PipeServer struct {
	name string

	mu        sync.Mutex
	sddl      string
	pending   windows.Handle // instance waiting in ConnectNamedPipe, 0 when none
	client    io.ReadCloser
	connected bool
	dirty     bool // DACL changed; Serve must drop the pending instance
}

// NewPipeServer creates the server; the DACL starts as baseSD (no user ACE)
// until EnsureSDDL names the session user.
func NewPipeServer(name string) *PipeServer {
	return &PipeServer{name: name, sddl: baseSD}
}

// Connected reports whether a watcher is currently attached.
func (p *PipeServer) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connected
}

// EnsureSDDL grants GENERIC_WRITE to userSID. A DACL change drops the current
// client and the pending instance so the watcher for the new session can
// attach (a stale watcher is denied on reconnect and exits; the spawner then
// launches the new session's watcher — spec §4.3, Review Focus #2).
func (p *PipeServer) EnsureSDDL(userSID string) {
	if userSID == "" {
		return
	}
	sddl := baseSD + fmt.Sprintf("(A;;GW;;;%s)", userSID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if sddl == p.sddl {
		return
	}
	p.sddl = sddl
	p.dirty = true
	p.connected = false
	if p.client != nil {
		p.client.Close()
		p.client = nil
	}
	if p.pending != 0 {
		// Unblocks ConnectNamedPipe; Serve closes the handle and recreates.
		windows.CancelIoEx(p.pending, nil)
	}
}

// Serve accepts clients until ctx is cancelled, publishing decoded samples
// to out. Exactly one pending pipe instance exists at a time; on accept, any
// previous client is closed (newest wins).
func (p *PipeServer) Serve(ctx context.Context, out chan<- Sample) {
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.connected = false
		if p.client != nil {
			p.client.Close()
			p.client = nil
		}
		if p.pending != 0 {
			windows.CancelIoEx(p.pending, nil)
			windows.CloseHandle(p.pending)
			p.pending = 0
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		sddl, dirty := p.sddl, p.dirty
		p.dirty = false
		p.mu.Unlock()

		if dirty && p.pendingHandle() != 0 {
			// Drop the stale-DACL instance; recreate below.
			p.mu.Lock()
			h := p.pending
			p.pending = 0
			p.mu.Unlock()
			windows.CloseHandle(h)
		}
		pending := p.pendingHandle()
		if pending == 0 {
			h, err := createPipeInstance(p.name, sddl)
			if err != nil {
				log.Printf("pipe create: %v (retry in 1s)", err)
				if !sleepCtx(ctx, time.Second) {
					return
				}
				continue
			}
			// Store the pending handle and re-check dirty atomically: an
			// EnsureSDDL between the sddl read and the create must not leave
			// a stale-DACL instance waiting forever.
			p.mu.Lock()
			if p.dirty {
				p.mu.Unlock()
				windows.CloseHandle(h)
				continue
			}
			p.pending = h
			p.mu.Unlock()
			pending = h
		}

		connCh := make(chan error, 1)
		go func() { connCh <- windows.ConnectNamedPipe(pending, nil) }()
		var connErr error
		select {
		case <-ctx.Done():
			windows.CancelIoEx(pending, nil)
			p.mu.Lock()
			p.pending = 0
			p.mu.Unlock()
			windows.CloseHandle(pending)
			return
		case connErr = <-connCh:
		}
		p.mu.Lock()
		p.pending = 0
		p.mu.Unlock()
		windows.CloseHandle(pending) // handle is consumed either way

		if connErr != nil {
			if ctx.Err() != nil {
				return
			}
			continue // canceled by EnsureSDDL or spurious; recreate
		}

		p.mu.Lock()
		if p.dirty {
			// The DACL changed while this instance waited: this client (or
			// connect) belongs to the old session — drop and recreate.
			c := os.NewFile(uintptr(pending), p.name)
			c.Close()
			p.mu.Unlock()
			continue
		}
		if p.client != nil {
			p.client.Close() // newest wins
		}
		c := os.NewFile(uintptr(pending), p.name)
		p.client = c
		p.connected = true
		p.mu.Unlock()
		go p.readClient(c, out)
	}
}

func (p *PipeServer) pendingHandle() windows.Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending
}

// readClient decodes lines until the client goes away; malformed lines are
// logged and skipped (Review Focus #3). On disconnect it clears connected
// state unless a newer client already replaced it.
func (p *PipeServer) readClient(c io.ReadCloser, out chan<- Sample) {
	defer func() {
		p.mu.Lock()
		if p.client == c {
			p.client = nil
			p.connected = false
		}
		p.mu.Unlock()
		c.Close()
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), 4096)
	for sc.Scan() {
		s, err := DecodeSample(sc.Bytes())
		if err != nil {
			log.Printf("pipe: %v", err)
			continue
		}
		select {
		case out <- s:
		default:
			log.Printf("pipe: sample dropped (consumer slow)")
		}
	}
}

// createPipeInstance opens one server-side pipe instance with the given DACL.
func createPipeInstance(name, sddl string) (windows.Handle, error) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, fmt.Errorf("sddl: %w", err)
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateNamedPipe(windows.StringToUTF16Ptr(name),
		windows.PIPE_ACCESS_INBOUND,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		windows.PIPE_UNLIMITED_INSTANCES, 512, 512, 0, sa)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// sleepCtx waits for d or ctx cancellation; reports whether d elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestPipe -v`
Expected: PASS (all four). If a dial/connect timing issue flakes, rerun once before investigating.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/pipe_windows.go internal/agent/pipe_windows_test.go
git commit -m "feat(agent): sample pipe server with newest-wins and session-user DACL"
```

---

### Task 4: Watcher (`internal/agent/watcher_windows.go`)

**Files:**
- Create: `internal/agent/watcher_windows.go`
- Test: `internal/agent/watcher_windows_test.go`

**Interfaces:**
- Consumes: `Sample`, `EncodeSample` (Task 1), `ForegroundSource`/`IdleSource` (existing), `samplePipeName` (Task 3).
- Produces: `type WatcherDeps struct { FG ForegroundSource; Idle IdleSource; OwnSession func() (uint32, error); ConsoleSession func() uint32; Dial func() (io.WriteCloser, error); DialInterval, DialBudget time.Duration }` and `func RunWatcher(ctx context.Context, idleThresholdSeconds int, deps WatcherDeps) error`. Used by Task 6.

- [ ] **Step 1: Write the failing tests**

```go
//go:build windows

package agent

import (
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
```

Add `"bytes"` to the test imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestWatcher -v`
Expected: FAIL — `undefined: RunWatcher`

- [ ] **Step 3: Write the implementation**

```go
//go:build windows

package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Watcher connect-retry cadence (spec §4.1): 500ms attempts, 15s budget.
const (
	watcherDialInterval = 500 * time.Millisecond
	watcherDialBudget   = 15 * time.Second
)

// WatcherDeps carries the watcher's collaborators. Nil funcs are replaced
// with production implementations by RunWatcher; tests inject fakes.
// DialInterval/DialBudget zero values mean the defaults above.
type WatcherDeps struct {
	FG             ForegroundSource
	Idle           IdleSource
	OwnSession     func() (uint32, error) // this process's session id
	ConsoleSession func() uint32          // active console session id
	Dial           func() (io.WriteCloser, error)
	DialInterval   time.Duration
	DialBudget     time.Duration
}

// RunWatcher polls the interactive session at 1 Hz and writes samples to the
// service pipe until ctx is cancelled. It self-pauses when its session is not
// the active console one (no samples for invisible sessions — spec §4.1) and
// exits when the pipe stays unreachable past the dial budget; the service's
// spawner then brings up a fresh watcher.
func RunWatcher(ctx context.Context, idleThresholdSeconds int, deps WatcherDeps) error {
	if deps.FG == nil || deps.Idle == nil {
		return fmt.Errorf("watcher: FG and Idle sources are required")
	}
	if deps.OwnSession == nil {
		deps.OwnSession = func() (uint32, error) {
			var sid uint32
			err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid)
			return sid, err
		}
	}
	if deps.ConsoleSession == nil {
		deps.ConsoleSession = windows.WTSGetActiveConsoleSessionId
	}
	if deps.Dial == nil {
		deps.Dial = dialSamplePipe
	}
	interval, budget := deps.DialInterval, deps.DialBudget
	if interval == 0 {
		interval = watcherDialInterval
	}
	if budget == 0 {
		budget = watcherDialBudget
	}

	conn, err := dialRetry(ctx, deps.Dial, interval, budget)
	if err != nil {
		if ctx.Err() != nil {
			return nil // cancelled while waiting for the pipe: clean exit
		}
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
			return nil
		case <-tick.C:
			if conn == nil {
				c, err := dialRetry(ctx, deps.Dial, interval, budget)
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				conn = c
			}
			own, err := deps.OwnSession()
			if err != nil || own != deps.ConsoleSession() {
				continue // paused: not the active console session
			}
			s := Sample{
				App:    deps.FG.ForegroundApp(),
				Active: deps.Idle.IdleSeconds() < float64(idleThresholdSeconds),
			}
			if _, err := conn.Write(EncodeSample(s)); err != nil {
				log.Printf("watcher: pipe write: %v", err)
				conn.Close()
				conn = nil
			}
		}
	}
}

// dialRetry attempts dial every interval for up to budget (bounded — Review
// Focus #1), returning the first success.
func dialRetry(ctx context.Context, dial func() (io.WriteCloser, error), interval, budget time.Duration) (io.WriteCloser, error) {
	deadline := time.Now().Add(budget)
	for {
		c, err := dial()
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("watcher: pipe unreachable for %v: %w", budget, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// dialSamplePipe opens a write handle to the service's sample pipe.
func dialSamplePipe() (io.WriteCloser, error) {
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(samplePipeName),
		windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), samplePipeName), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestWatcher -v`
Expected: PASS (all five). `TestWatcherPausesOffConsoleSession` sleeps 1.2 s by design.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/watcher_windows.go internal/agent/watcher_windows_test.go
git commit -m "feat(agent): in-session watcher with self-pause and bounded dial retry"
```

---

### Task 5: Spawner (`internal/agent/spawn_windows.go`)

**Files:**
- Create: `internal/agent/spawn_windows.go`
- Test: `internal/agent/spawn_windows_test.go`

**Interfaces:**
- Consumes: `PipeServer.Connected`/`EnsureSDDL` (Task 3).
- Produces: `type samplePipe interface { Connected() bool; EnsureSDDL(userSID string) }` (satisfied by `*PipeServer`), `var spawnLadder = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}`, `const noConsoleSession = 0xFFFFFFFF`, `type Spawner struct { ExePath string; IdleSeconds int; Pipe samplePipe; ConsoleSession func() uint32; QueryToken func(uint32) (windows.Token, error); TokenUser func(windows.Token) (string, error); Spawn func(windows.Token, string, []string) error; Now func() time.Time; ... }`, `func NewSpawner(exePath string, idleSeconds int, pipe *PipeServer) *Spawner`, `func (s *Spawner) Poke()`, `func (s *Spawner) Run(ctx context.Context)`. Used by Task 6.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestSpawner -v`
Expected: FAIL — `undefined: NewSpawner`

- [ ] **Step 3: Write the implementation**

```go
//go:build windows

package agent

import (
	"context"
	"fmt"
	"log"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// noConsoleSession is WTSGetActiveConsoleSessionId's "no session" value.
const noConsoleSession = 0xFFFFFFFF

// spawnLadder is the watcher respawn backoff (spec §4.2): 1s, 5s, 30s cap.
// Review Focus #4: the cap keeps a user repeatedly killing the watcher from
// thrashing the CPU.
var spawnLadder = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// samplePipe is what the spawner needs from the pipe server.
type samplePipe interface {
	Connected() bool
	EnsureSDDL(userSID string)
}

// Spawner keeps a watcher alive inside the active console session. On a 30s
// tick — immediately when Poked (session-change events) — it checks whether a
// watcher is attached and, backoff permitting, launches one as the session
// user. Reconciliation is driven by pipe-connection state, not PIDs (spec §4.2).
type Spawner struct {
	ExePath     string
	IdleSeconds int
	Pipe        samplePipe

	ConsoleSession func() uint32
	QueryToken     func(uint32) (windows.Token, error)
	TokenUser      func(windows.Token) (string, error)
	Spawn          func(windows.Token, string, []string) error
	Now            func() time.Time

	nextAttempt time.Time
	ladderIdx   int
	poke        chan struct{}
}

// NewSpawner fills production collaborators; tests override fields.
func NewSpawner(exePath string, idleSeconds int, pipe *PipeServer) *Spawner {
	return &Spawner{
		ExePath:        exePath,
		IdleSeconds:    idleSeconds,
		Pipe:           pipe,
		ConsoleSession: windows.WTSGetActiveConsoleSessionId,
		QueryToken:     windows.WTSQueryUserToken,
		TokenUser:      tokenUserSID,
		Spawn:          spawnWatcherProcess,
		Now:            time.Now,
		poke:           make(chan struct{}, 1),
	}
}

// Poke requests an immediate reconcile (WTS session change). Non-blocking.
func (s *Spawner) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is cancelled. The first reconcile happens
// immediately: on service start (e.g. after -upgrade or reboot-with-autologon)
// a watcher must come up without waiting out the first 30s tick (spec §4.2).
func (s *Spawner) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	s.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.poke:
		case <-tick.C:
		}
		s.reconcile()
	}
}

func (s *Spawner) reconcile() {
	if s.Pipe.Connected() {
		s.ladderIdx = 0 // a watcher is attached; ladder resets
		s.nextAttempt = time.Time{}
		return
	}
	sid := s.ConsoleSession()
	if sid == noConsoleSession || sid == 0 {
		return // nobody on the console: ship nothing (spec §5)
	}
	now := s.Now()
	if now.Before(s.nextAttempt) {
		return
	}
	tok, err := s.QueryToken(sid)
	if err != nil {
		log.Printf("spawner: query token for session %d: %v", sid, err)
		s.consumeLadder(now)
		return
	}
	defer tok.Close()
	sidStr, err := s.TokenUser(tok)
	if err != nil {
		log.Printf("spawner: token user: %v", err)
		s.consumeLadder(now)
		return
	}
	// Grant the session user pipe-write access before the watcher exists.
	s.Pipe.EnsureSDDL(sidStr)
	if err := s.Spawn(tok, s.ExePath, []string{"-userwatch", "-idle", fmt.Sprintf("%d", s.IdleSeconds)}); err != nil {
		log.Printf("spawner: launch watcher: %v", err)
	}
	s.consumeLadder(now)
}

// consumeLadder schedules the next attempt per the backoff ladder.
func (s *Spawner) consumeLadder(now time.Time) {
	s.nextAttempt = now.Add(spawnLadder[s.ladderIdx])
	if s.ladderIdx < len(spawnLadder)-1 {
		s.ladderIdx++
	}
}

// tokenUserSID resolves the account SID string of a user token.
func tokenUserSID(tok windows.Token) (string, error) {
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

var (
	userenv                     = windows.NewLazySystemDLL("userenv.dll")
	procCreateEnvironmentBlock  = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock = userenv.NewProc("DestroyEnvironmentBlock")
)

// spawnWatcherProcess launches exe with args in the token's session and with
// no window (spec §2, §4.2). The child is not tracked: pipe-connection state
// drives reconciliation, so its handles are closed immediately.
func spawnWatcherProcess(tok windows.Token, exe string, args []string) error {
	env, err := createEnvironmentBlock(tok)
	if err != nil {
		return fmt.Errorf("environment block: %w", err)
	}
	defer destroyEnvironmentBlock(env)

	line := `"` + exe + `"`
	for _, a := range args {
		line += " " + syscall.EscapeArg(a)
	}
	cl, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return err
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(tok, exePtr, cl, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		env, nil, si, &pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Process)
	windows.CloseHandle(pi.Thread)
	return nil
}

// createEnvironmentBlock builds the user's environment (TEMP, APPDATA, …).
func createEnvironmentBlock(tok windows.Token) (*uint16, error) {
	var env *uint16
	r, _, callErr := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&env)), uintptr(tok), 1)
	if r == 0 {
		return nil, callErr
	}
	return env, nil
}

func destroyEnvironmentBlock(env *uint16) {
	procDestroyEnvironmentBlock.Call(uintptr(unsafe.Pointer(env)))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestSpawner -v`
Expected: PASS (all six)

- [ ] **Step 5: Commit**

```bash
git add internal/agent/spawn_windows.go internal/agent/spawn_windows_test.go
git commit -m "feat(agent): session-aware watcher spawner with backoff ladder"
```

---

### Task 6: Service wiring + CLI (`service_windows.go`, `cmd/agent/main.go`)

**Files:**
- Modify: `internal/agent/service_windows.go` (handler gains `sessionChange`; `RunService` builds pipe + spawner; `defaultRun` replaced by `composeServiceRun`)
- Modify: `cmd/agent/main.go` (`-userwatch`, `-idle` flags; `runService` drops the fg/idle sources; new `runWatcher`)
- Test: `internal/agent/service_windows_test.go` (update accepts assertion; add session-change test)

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: `func RunService(name string, cfg Config, now func() time.Time) error` — **signature changes** (fg/idle params removed; `defaultRun` is gone). `cmd/agent` calls it with `agent.RunService("monsvc", cfg, time.Now)`.

- [ ] **Step 1: Update the failing tests**

In `internal/agent/service_windows_test.go`, change the Running-status assertion in `TestExecuteGracefulStop` to also require `AcceptSessionChange`:

```go
	if s := <-statusCh; s.State != svc.Running || s.Accepts&(svc.AcceptStop|svc.AcceptShutdown|svc.AcceptSessionChange) != svc.AcceptStop|svc.AcceptShutdown|svc.AcceptSessionChange {
		t.Fatalf("second status = %+v, want Running accepting Stop|Shutdown|SessionChange", s)
	}
```

And append:

```go
func TestExecuteSessionChangePokes(t *testing.T) {
	pokes := 0
	h := &serviceHandler{
		run: func(ctx context.Context) error { <-ctx.Done(); return nil },
		sessionChange: func() { pokes++ },
	}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8)
	done := make(chan struct{})
	go func() { h.Execute(nil, reqCh, statusCh); close(done) }()
	<-statusCh // StartPending
	<-statusCh // Running

	reqCh <- svc.ChangeRequest{Cmd: svc.SessionChange, EventType: svc.SessionRemoteConnect}
	if pokes != 1 {
		t.Fatalf("pokes = %d, want 1", pokes)
	}
	reqCh <- svc.Stop
	<-done
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestExecute -v`
Expected: FAIL — Running status lacks `AcceptSessionChange`; `TestExecuteSessionChangePokes` cannot build (`sessionChange` field undefined)

- [ ] **Step 3: Implement the service changes**

In `internal/agent/service_windows.go`:

```go
// serviceHandler is the svc.Handler for monsvc. run is injectable so the
// Execute loop is testable without a real service control manager;
// sessionChange (nil-safe) fires on WTS session events so the spawner can
// reconcile immediately (spec §4.2).
type serviceHandler struct {
	run           func(ctx context.Context) error
	sessionChange func()
}
```

In `Execute`, change the Running status and add the session-change case:

```go
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange}

	for {
		select {
		case err := <-errCh:
			// (unchanged body)
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.SessionChange:
				if h.sessionChange != nil {
					h.sessionChange()
				}
			case svc.Stop, svc.Shutdown:
				// (unchanged body)
			}
		}
	}
```

Replace `defaultRun` and `RunService` with:

```go
// RunService runs the agent as the named Windows service (blocking). The
// service never polls Win32 itself — it runs the sample pipe server and the
// watcher spawner, and feeds pipe samples into the agent (spec §3).
func RunService(name string, cfg Config, now func() time.Time) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pipe := NewPipeServer(samplePipeName)
	sp := NewSpawner(exe, cfg.IdleThresholdSeconds, pipe)
	h := &serviceHandler{
		run:           composeServiceRun(cfg, now, pipe, sp),
		sessionChange: sp.Poke,
	}
	return svc.Run(name, h)
}

// composeServiceRun builds the service-mode agent: the pipe server publishes
// watcher samples, the spawner keeps a watcher alive, and the agent consumes
// samples (no samples → no heartbeats).
func composeServiceRun(cfg Config, now func() time.Time, pipe *PipeServer, sp *Spawner) func(context.Context) error {
	return func(ctx context.Context) error {
		fg, idle := NewWin32Sources() // required by New; unused in service mode
		a, err := New(cfg, fg, idle, now)
		if err != nil {
			return err
		}
		samples := make(chan Sample, 128)
		go pipe.Serve(ctx, samples)
		go sp.Run(ctx)
		return a.RunSamples(ctx, samples)
	}
}
```

Add `"os"` to the file's imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run TestExecute -v`
Expected: PASS

- [ ] **Step 5: Update `cmd/agent/main.go`**

Add the flags:

```go
	userwatch := flag.Bool("userwatch", false, "run the in-session watcher (spawned by the monsvc service)")
	idle := flag.Int("idle", 180, "idle threshold in seconds (with -userwatch)")
```

Add the switch case before `case *service:`:

```go
	case *userwatch:
		runWatcher(*idle)
```

Replace `runService` (drops the Win32 sources — the service no longer polls):

```go
// runService is the production path, invoked by the SCM. Config is read from
// agent.yaml next to the binary, not from -config. The service spawns the
// in-session watcher and ships its samples (spec §3).
func runService() {
	exe, _ := os.Executable()
	cfg, err := agent.LoadConfig(filepath.Join(filepath.Dir(exe), "agent.yaml"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := agent.RunService("monsvc", cfg, time.Now); err != nil {
		log.Fatalf("service: %v", err)
	}
}
```

Add `runWatcher`:

```go
// runWatcher is the in-session production path: launched by the monsvc service
// inside the logged-on user's session, it polls foreground/idle and streams
// samples to the service over the local pipe (spec §4.1). It holds no config
// file, no token, and writes nothing to disk.
func runWatcher(idleSeconds int) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fg, idle := agent.NewWin32Sources()
	err := agent.RunWatcher(ctx, idleSeconds, agent.WatcherDeps{FG: fg, Idle: idle})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("watcher: %v", err)
	}
}
```

- [ ] **Step 6: Build and run the full suite**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go build ./...`, `go test ./...`
Expected: all clean and PASS

- [ ] **Step 7: Commit**

```bash
git add internal/agent/service_windows.go internal/agent/service_windows_test.go cmd/agent/main.go
git commit -m "feat(agent): wire service to pipe+spawner, add -userwatch mode"
```

---

### Task 7: Live integration on this PC (Win11)

**Files:**
- Build: `agent.exe` (repo root, replaces the stale binary — it is untracked build output)
- Read: `agent.yaml` (repo root) for the server URL; **never print or commit the token**

**Interfaces:**
- Consumes: the built binary; the running monitor-server on this machine (dashboard `http://localhost:8998/admin`).

- [ ] **Step 1: Full test suite + build**

Run: `go test ./... && go vet ./... && go build -o agent.exe ./cmd/agent`
Expected: PASS, binary built

- [ ] **Step 2: Install the service with a fresh device token**

The server's device tokens are stored hashed, so the operator (user) creates a device via `/admin` (e.g. name `watcher-test-win11`) and provides the `sl_...` token. Then, from an **elevated** PowerShell in the repo root:

```powershell
.\agent.exe -install -server "<server URL from agent.yaml>" -token "<sl_... token>"
```

Expected: `installed and started monsvc (server=...)`; `Get-Service monsvc` shows Running; `Get-CimInstance Win32_Process -Filter "Name='monsvc.exe'" | Select ProcessId,SessionId,CommandLine` shows **two** processes — the service in session 0 (no window) and the watcher in the console session (SessionId matches `quser`'s active session).

- [ ] **Step 3: Verify data on the dashboard/DB**

Work in some apps for ~1 minute, then check the SQLite DB:

```bash
sqlite3 -header swdigital.db "SELECT app, active, COUNT(*) FROM heartbeats WHERE device_id=(SELECT id FROM devices WHERE name='watcher-test-win11') GROUP BY app, active;"
```

Expected: real app names (`WindowsTerminal.exe`, browser, etc.) with `active=1`. This is the acceptance check for the original bug.

- [ ] **Step 4: Verify resilience behaviors**

1. Kill the in-session watcher in Task Manager → within ~30 s a new watcher appears (spawner ladder) and samples resume.
2. Win+L lock for ~1 min, unlock → DB shows `unknown.exe` / `active=0` rows during the lock, real names after.
3. (Optional, destructive to the session) Log off/on → heartbeats stop at logoff, resume at logon.

- [ ] **Step 5: Report and plan rollout**

Report results to the user. The ~17 enrolled PCs get the fix via the existing upgrade path (`agent.exe -upgrade` with the new binary pushed); no config or token changes, no logoff needed. Rollout itself is a separate step owned by the user.

- [ ] **Step 6: Commit (nothing expected)**

No code changes in this task unless integration uncovered a fix — if so, fix + test + commit before reporting.
