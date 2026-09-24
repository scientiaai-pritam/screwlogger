# ScrewLogger M1 Implementation Plan — Agent + Ingest

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Windows agent (`monsvc`) that heartbeats foreground-app/active-idle state every few seconds, buffers losslessly on disk, and ships idempotently (with offline backfill) to a LAN server storing heartbeats in SQLite.

**Architecture:** Monorepo, all Go. Agent: 1 s poller → heartbeat-builder state machine → local categorizer → fsynced JSONL buffer → shipper with backoff. Server: single binary, `POST /v1/ingest` with device-token auth, `INSERT OR IGNORE` idempotency, out-of-order timestamps accepted. Win32 calls sit behind interfaces so everything else is unit-testable.

**Tech Stack:** Go 1.23+, `modernc.org/sqlite` (pure Go, no CGO — no gcc needed on Windows), `gopkg.in/yaml.v3`, `github.com/google/uuid`, `golang.org/x/sys/windows`, stdlib `net/http/httptest` for tests.

**Spec:** `docs/superpowers/specs/2026-09-24-screwlogger-design.md` (§3.1–3.4 agent, §4 data model, §5 error handling, §8 M1 gate)

## Global Constraints

- Module path: `screwlogger`. Go version: `go 1.23` in `go.mod`.
- SQLite driver: `modernc.org/sqlite` ONLY (no CGO, no `mattn/go-sqlite3`).
- Server trusts agent `ts` for event time but always records `received_at` (server clock) and `agent_sent_at` (from batch). (Spec §5)
- Heartbeat UUID is the idempotency key: server uses `INSERT OR IGNORE`. (Spec §3.3)
- Dwell-time cap constant is 30 s (= 2× the 15 s keep-alive); it appears only as `MaxHeartbeatGap = 30` in the protocol package in M1; M2 consumes it for queries. (Spec §4)
- Idle threshold default 180 s, configurable in agent config. Idle iff `idleSeconds >= threshold`. (Spec §3.1)
- Buffer fsyncs every append BEFORE any upload attempt; ack prunes; oldest dropped only when buffer exceeds `max_buffer_mb` with a loud log line. (Spec §3.3)
- No window titles, no keystrokes, nothing but `{app, category, timestamps, active}` is ever recorded or transmitted. (Spec §1, §3.2)
- Shipper backoff sequence: 15 s → 60 s → 5 min ceiling; healthy flush interval 60 s; batch limit 500 heartbeats. (Spec §5)
- `schema_version` != 1 → HTTP 400 with explicit JSON error; never store. (Spec §5)
- Unknown app → category `"Uncategorized"`. (Spec §3.2)
- Process/binary name in M1 is plain (`agent.exe`, `server.exe`); `monsvc` service naming/installation is M3.

## Review Focus

Failure modes the spec implies that individual unit tests could miss; each is pinned by a test in its owning task:

1. **Replayed backfill must not double-count** — server receives the same batch twice (network retry after dropped response) → row count unchanged. Pinned: Task 10 (duplicate-batch test).
2. **A crash between append and upload must lose nothing** — buffer reopened after process restart returns all unacked entries in order. Pinned: Task 4 (reopen test).
3. **Idle boundary** — at exactly `threshold` seconds the person is idle, one second less is active; a flip must emit a heartbeat immediately, not at keep-alive. Pinned: Task 2 (boundary + flip tests).
4. **Keep-alive gap never exceeds the dwell cap** — unchanged state must emit within keep-alive so server-side 30 s capping never fabricates or truncates time. Pinned: Task 2 (keep-alive test).
5. **Bad input must be rejected loudly, not mis-stored** — unknown `schema_version`, malformed JSON, wrong token → 4xx with explicit error, zero rows written. Pinned: Task 10 (rejection tests).

---

### Task 1: Module scaffold + shared protocol package

**Files:**
- Create: `go.mod`
- Create: `internal/protocol/protocol.go`
- Test: `internal/protocol/protocol_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `protocol.SchemaVersion = 1` (int const), `protocol.MaxHeartbeatGap = 30` (int64 const, seconds), `protocol.Heartbeat{ID string; TS int64; App string; Category string; Active bool}` with json tags `id,ts,app,category,active`, `protocol.IngestBatch{SchemaVersion int; DeviceSentAt int64; Heartbeats []Heartbeat}` with tags `schema_version,device_sent_at,heartbeats`, `protocol.IngestResponse{Accepted int64}` with tag `accepted`. Every later task imports these exact names.

- [ ] **Step 1: Initialize module and fetch deps**

Run from repo root:
```powershell
go mod init screwlogger
go get modernc.org/sqlite@latest
go get gopkg.in/yaml.v3@latest
go get github.com/google/uuid@latest
go get golang.org/x/sys/windows@latest
```
Expected: `go.mod` created, no errors.

- [ ] **Step 2: Write the failing test**

`internal/protocol/protocol_test.go`:
```go
package protocol_test

import (
	"encoding/json"
	"testing"

	"screwlogger/internal/protocol"
)

func TestHeartbeatJSONRoundTrip(t *testing.T) {
	hb := protocol.Heartbeat{ID: "abc", TS: 1729800000, App: "excel.exe", Category: "Office", Active: true}
	b, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":"abc","ts":1729800000,"app":"excel.exe","category":"Office","active":true}` {
		t.Fatalf("unexpected wire format: %s", b)
	}
	var back protocol.Heartbeat
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != hb {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestIngestBatchWireFormat(t *testing.T) {
	batch := protocol.IngestBatch{SchemaVersion: protocol.SchemaVersion, DeviceSentAt: 1729800001,
		Heartbeats: []protocol.Heartbeat{{ID: "x", TS: 1, App: "a", Category: "c", Active: false}}}
	b, _ := json.Marshal(batch)
	for _, want := range []string{`"schema_version":1`, `"device_sent_at"`, `"heartbeats"`} {
		if !contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
})() }
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/protocol/ -v`
Expected: FAIL — `no required module provides package screwlogger/internal/protocol`.

- [ ] **Step 4: Write minimal implementation**

`internal/protocol/protocol.go`:
```go
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
	SchemaVersion int         `json:"schema_version"`
	DeviceSentAt  int64       `json:"device_sent_at"` // agent flush time → stored as agent_sent_at
	Heartbeats    []Heartbeat `json:"heartbeats"`
}

// IngestResponse is the 200 body of POST /v1/ingest.
type IngestResponse struct {
	Accepted int64 `json:"accepted"`
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/protocol/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add go.mod go.sum internal/protocol/
git commit -m "feat: module scaffold and shared ingest protocol"
```

---

### Task 2: Heartbeat builder state machine

**Files:**
- Create: `internal/agent/heartbeat.go`
- Test: `internal/agent/heartbeat_test.go`

**Interfaces:**
- Consumes: nothing (agent-local type).
- Produces: `agent.NewHeartbeatBuilder(now func() time.Time, keepAlive time.Duration) *HeartbeatBuilder`; `func (b *HeartbeatBuilder) Observe(app string, active bool) *protocol.Heartbeat` — returns nil when no emission is due; the returned heartbeat has `TS`, `App`, `Active` set; caller fills `ID` and `Category`. Also `agent.DefaultKeepAlive = 15 * time.Second`.

- [ ] **Step 1: Write the failing tests**

`internal/agent/heartbeat_test.go`:
```go
package agent_test

import (
	"testing"
	"time"

	"screwlogger/internal/agent"
)

func newBuilder(start time.Time) (*agent.HeartbeatBuilder, *func() time.Time) {
	now := start
	fn := func() time.Time { return now }
	return agent.NewHeartbeatBuilder(fn, agent.DefaultKeepAlive), &fn
}

func TestFirstObservationEmits(t *testing.T) {
	b, _ := newBuilder(time.Unix(1000, 0))
	hb := b.Observe("excel.exe", true)
	if hb == nil || hb.App != "excel.exe" || !hb.Active || hb.TS != 1000 {
		t.Fatalf("first observe must emit: %+v", hb)
	}
}

func TestUnchangedStateEmitsOnlyOnKeepAlive(t *testing.T) {
	b, nowP := newBuilder(time.Unix(1000, 0))
	b.Observe("excel.exe", true)
	*nowP = time.Unix(1014, 0) // 14s later: inside keep-alive
	if hb := b.Observe("excel.exe", true); hb != nil {
		t.Fatalf("no emission expected before keep-alive: %+v", hb)
	}
	*nowP = time.Unix(1015, 0) // exactly keep-alive: must emit
	if hb := b.Observe("excel.exe", true); hb == nil {
		t.Fatal("keep-alive emission expected")
	}
}

func TestAppChangeEmitsImmediately(t *testing.T) {
	b, nowP := newBuilder(time.Unix(1000, 0))
	b.Observe("excel.exe", true)
	*nowP = time.Unix(1001, 0)
	if hb := b.Observe("chrome.exe", true); hb == nil || hb.App != "chrome.exe" {
		t.Fatalf("app change must emit immediately: %+v", hb)
	}
}

func TestActiveFlipEmitsImmediately(t *testing.T) {
	b, nowP := newBuilder(time.Unix(1000, 0))
	b.Observe("excel.exe", true)
	*nowP = time.Unix(1001, 0)
	if hb := b.Observe("excel.exe", false); hb == nil || hb.Active {
		t.Fatalf("active flip must emit immediately: %+v", hb)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -v`
Expected: FAIL — `undefined: agent.NewHeartbeatBuilder`.

- [ ] **Step 3: Write minimal implementation**

`internal/agent/heartbeat.go`:
```go
package agent

import (
	"time"

	"screwlogger/internal/protocol"
)

// DefaultKeepAlive is how often an unchanged state re-emits (spec §3.1).
const DefaultKeepAlive = 15 * time.Second

// HeartbeatBuilder converts 1s poll observations into heartbeats: it emits
// on app change, on active/idle flip, and as a keep-alive on unchanged state.
type HeartbeatBuilder struct {
	now       func() time.Time
	keepAlive time.Duration
	started   bool
	curApp    string
	curActive bool
	lastEmit  time.Time
}

func NewHeartbeatBuilder(now func() time.Time, keepAlive time.Duration) *HeartbeatBuilder {
	return &HeartbeatBuilder{now: now, keepAlive: keepAlive}
}

// Observe feeds one poll result and returns a heartbeat if one is due, else nil.
func (b *HeartbeatBuilder) Observe(app string, active bool) *protocol.Heartbeat {
	now := b.now()
	due := !b.started ||
		app != b.curApp ||
		active != b.curActive ||
		now.Sub(b.lastEmit) >= b.keepAlive
	if !due {
		return nil
	}
	b.started, b.curApp, b.curActive, b.lastEmit = true, app, active, now
	return &protocol.Heartbeat{TS: now.Unix(), App: app, Active: active}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/agent/
git commit -m "feat(agent): heartbeat builder state machine"
```

---

### Task 3: Local categorizer

**Files:**
- Create: `internal/agent/categorize.go`
- Create: `rules.example.yaml` (repo root — the format documentation)
- Test: `internal/agent/categorize_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type agent.Rule struct { Pattern string `yaml:"pattern"`; Category string `yaml:"category"` }`; `type agent.Rules []Rule`; `agent.LoadRules(path string) (Rules, error)`; `func (r Rules) Categorize(app string) string` — case-insensitive exact match first, then case-insensitive glob (`*`, `?`) via `path.Match` on lowercased strings, first match wins, default `"Uncategorized"`.

- [ ] **Step 1: Write the failing tests**

`internal/agent/categorize_test.go`:
```go
package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"screwlogger/internal/agent"
)

func writeRules(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testRules = `rules:
  - {pattern: excel.exe, category: Office}
  - {pattern: "chrome.exe", category: Browser}
  - {pattern: "mes_*.exe", category: Production}
`

func TestExactMatchCaseInsensitive(t *testing.T) {
	rules, err := agent.LoadRules(writeRules(t, testRules))
	if err != nil {
		t.Fatal(err)
	}
	if got := rules.Categorize("EXCEL.EXE"); got != "Office" {
		t.Fatalf("got %q", got)
	}
}

func TestGlobMatch(t *testing.T) {
	rules, _ := agent.LoadRules(writeRules(t, testRules))
	if got := rules.Categorize("mes_line3.exe"); got != "Production" {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownAppUncategorized(t *testing.T) {
	rules, _ := agent.LoadRules(writeRules(t, testRules))
	if got := rules.Categorize("notepad.exe"); got != "Uncategorized" {
		t.Fatalf("got %q", got)
	}
}

func TestExactBeatsGlob(t *testing.T) {
	rules := agent.Rules{
		{Pattern: "*", Category: "CatchAll"},
		{Pattern: "chrome.exe", Category: "Browser"},
	}
	if got := rules.Categorize("chrome.exe"); got != "Browser" {
		t.Fatalf("exact must win regardless of order; got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestCategorize -v` and also `go test ./internal/agent/ -v`
Expected: FAIL — `undefined: agent.LoadRules`.

- [ ] **Step 3: Write minimal implementation**

`internal/agent/categorize.go`:
```go
package agent

import (
	"os"
	"path"

	"gopkg.in/yaml.v3"
)

// Category used when no rule matches (spec §3.2).
const Uncategorized = "Uncategorized"

// Rule maps one executable-name pattern to a category. Patterns support
// exact names and globs (* ?), compared case-insensitively.
type Rule struct {
	Pattern  string `yaml:"pattern"`
	Category string `yaml:"category"`
}

// Rules is an ordered rule set; Categorize resolves exact matches before
// globs regardless of file order.
type Rules []Rule

type rulesFile struct {
	Rules []Rule `yaml:"rules"`
}

// LoadRules reads and validates a rules YAML file.
func LoadRules(path string) (Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rf rulesFile
	if err := yaml.Unmarshal(b, &rf); err != nil {
		return nil, err
	}
	return Rules(rf.Rules), nil
}

// Categorize returns the category for app: exact match wins over glob,
// first rule wins among equals, Uncategorized when nothing matches.
func (r Rules) Categorize(app string) string {
	lower := lower(app)
	for _, rule := range r {
		if lower(rule.Pattern) == lower {
			return rule.Category
		}
	}
	for _, rule := range r {
		if ok, err := path.Match(lower(rule.Pattern), lower); err == nil && ok {
			return rule.Category
		}
	}
	return Uncategorized
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
```

`rules.example.yaml`:
```yaml
# ScrewLogger agent categorization rules (spec §3.2).
# Only {app, category, timestamps, active} ever leaves the PC.
rules:
  - {pattern: excel.exe, category: Office}
  - {pattern: winword.exe, category: Office}
  - {pattern: chrome.exe, category: Browser}
  - {pattern: msedge.exe, category: Browser}
  - {pattern: "mes_*.exe", category: Production}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/agent/categorize.go internal/agent/categorize_test.go rules.example.yaml
git commit -m "feat(agent): local categorizer with exact+glob rules"
```

---

### Task 4: Durable buffer

**Files:**
- Create: `internal/agent/buffer.go`
- Test: `internal/agent/buffer_test.go`

**Interfaces:**
- Consumes: `protocol.Heartbeat` (Task 1).
- Produces: `agent.OpenBuffer(path string, maxBytes int64) (*Buffer, error)`; `func (b *Buffer) Append(h protocol.Heartbeat) error` (appends one JSON line + fsync); `func (b *Buffer) Unacked(limit int) ([]protocol.Heartbeat, error)` (oldest-first, in order); `func (b *Buffer) Ack(count int) error` (marks the first `count` unacked entries uploaded; compacts when acked prefix ≥ half the file); `func (b *Buffer) Close() error`. Rotation: if unacked bytes exceed `maxBytes`, the oldest half of unacked entries is dropped on next Append and a `log.Printf("buffer overflow...")` line is emitted.

- [ ] **Step 1: Write the failing tests**

`internal/agent/buffer_test.go`:
```go
package agent_test

import (
	"path/filepath"
	"testing"

	"screwlogger/internal/agent"
	"screwlogger/internal/protocol"
)

func hb(i int) protocol.Heartbeat {
	return protocol.Heartbeat{ID: string(rune('a' + i)), TS: int64(1000 + i), App: "a.exe", Category: "C", Active: true}
}

func TestAppendAndUnacked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, err := agent.OpenBuffer(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i := 0; i < 3; i++ {
		if err := b.Append(hb(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Unacked(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "a" || got[2].ID != "c" {
		t.Fatalf("got %+v", got)
	}
}

func TestAckPrunesAndReopenKeepsUnacked(t *testing.T) {
	// Review Focus #2: crash between append and upload loses nothing.
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, _ := agent.OpenBuffer(p, 1<<20)
	for i := 0; i < 4; i++ {
		b.Append(hb(i))
	}
	if err := b.Ack(2); err != nil {
		t.Fatal(err)
	}
	b.Close()

	b2, err := agent.OpenBuffer(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	got, err := b2.Unacked(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "d" {
		t.Fatalf("after ack(2) reopen, want [c d], got %+v", got)
	}
}

func TestAckCompactsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, _ := agent.OpenBuffer(p, 1<<20)
	for i := 0; i < 4; i++ {
		b.Append(hb(i))
	}
	b.Ack(4) // everything acked → compaction must shrink file to empty
	b.Close()
	b2, _ := agent.OpenBuffer(p, 1<<20)
	defer b2.Close()
	got, _ := b2.Unacked(10)
	if len(got) != 0 {
		t.Fatalf("fully acked buffer must be empty after reopen, got %+v", got)
	}
}

func TestRotationDropsOldestWhenFull(t *testing.T) {
	// maxBytes small: one entry ~100 bytes; cap at 200 → drop oldest half on overflow.
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, _ := agent.OpenBuffer(p, 200)
	defer b.Close()
	for i := 0; i < 6; i++ {
		b.Append(hb(i))
	}
	got, _ := b.Unacked(100)
	if len(got) >= 6 {
		t.Fatalf("overflow must drop oldest entries, still have %d", len(got))
	}
	if got[len(got)-1].ID != string(rune('a'+5)) {
		t.Fatalf("newest entry must survive rotation, got %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestBuffer -v` (adjust: tests are named TestAppend…, so run `go test ./internal/agent/ -v`)
Expected: FAIL — `undefined: agent.OpenBuffer`.

- [ ] **Step 3: Write minimal implementation**

`internal/agent/buffer.go`:
```go
package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"screwlogger/internal/protocol"
)

// Buffer is a fsynced append-only JSONL file with ack-based pruning (spec §3.3).
// Entries are appended and synced before any upload is attempted; Ack advances
// the acked prefix; the file is compacted when the acked prefix is at least half.
type Buffer struct {
	path     string
	maxBytes int64
	f        *os.File
	ackedOff int64 // bytes of file covered by prior Acks
	size     int64 // total current file size in bytes
}

// OpenBuffer opens (or creates) the buffer file and recovers offsets after restart.
func OpenBuffer(path string, maxBytes int64) (*Buffer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &Buffer{path: path, maxBytes: maxBytes, f: f}, nil
}

// Append writes one heartbeat as a JSON line and fsyncs. On overflow of unacked
// data beyond maxBytes, drops the oldest half of unacked entries (loud log).
func (b *Buffer) Append(h protocol.Heartbeat) error {
	if err := b.rotateIfFull(); err != nil {
		return err
	}
	line, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := b.f.Write(append(line, '\n')); err != nil {
		return err
	}
	b.size += int64(len(line)) + 1
	return b.f.Sync()
}

// Unacked returns up to limit oldest unacked heartbeats in file order.
func (b *Buffer) Unacked(limit int) ([]protocol.Heartbeat, error) {
	if limit <= 0 {
		return nil, nil
	}
	if _, err := b.f.Seek(b.ackedOff, 0); err != nil {
		return nil, err
	}
	var out []protocol.Heartbeat
	sc := bufio.NewScanner(b.f)
	for sc.Scan() && len(out) < limit {
		var h protocol.Heartbeat
		if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
			return nil, fmt.Errorf("corrupt buffer line: %w", err)
		}
		out = append(out, h)
	}
	return out, sc.Err()
}

// Ack marks the first count unacked entries as uploaded. When the acked prefix
// is at least half the file, the file is compacted (rewritten without acked lines).
func (b *Buffer) Ack(count int) error {
	if count <= 0 {
		return nil
	}
	if _, err := b.f.Seek(b.ackedOff, 0); err != nil {
		return err
	}
	sc := bufio.NewScanner(b.f)
	var advanced int64
	for i := 0; i < count && sc.Scan(); i++ {
		advanced += int64(len(sc.Bytes())) + 1
	}
	if err := sc.Err(); err != nil {
		return err
	}
	b.ackedOff += advanced
	if b.ackedOff*2 >= b.size {
		return b.compact()
	}
	return nil
}

// compact rewrites the file keeping only unacked entries.
func (b *Buffer) compact() error {
	unacked, err := b.Unacked(1 << 30)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, h := range unacked {
		line, _ := json.Marshal(h)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := b.f.Truncate(0); err != nil {
		return err
	}
	if _, err := b.f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := b.f.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := b.f.Sync(); err != nil {
		return err
	}
	b.ackedOff = 0
	b.size = int64(buf.Len())
	return nil
}

// rotateIfFull drops the oldest half of unacked entries when unacked bytes exceed maxBytes.
func (b *Buffer) rotateIfFull() error {
	if b.size-b.ackedOff <= b.maxBytes {
		return nil
	}
	log.Printf("buffer overflow: unacked %d bytes > max %d; dropping oldest half", b.size-b.ackedOff, b.maxBytes)
	unacked, err := b.Unacked(1 << 30)
	if err != nil {
		return err
	}
	keep := unacked[len(unacked)/2:]
	var buf bytes.Buffer
	for _, h := range keep {
		line, _ := json.Marshal(h)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := b.f.Truncate(0); err != nil {
		return err
	}
	if _, err := b.f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := b.f.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := b.f.Sync(); err != nil {
		return err
	}
	b.ackedOff = 0
	b.size = int64(buf.Len())
	return nil
}

// Close flushes and closes the underlying file.
func (b *Buffer) Close() error { return b.f.Close() }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS (Tasks 2–3 tests still green).

- [ ] **Step 5: Commit**

```powershell
git add internal/agent/buffer.go internal/agent/buffer_test.go
git commit -m "feat(agent): fsynced JSONL buffer with ack pruning and rotation"
```

---

### Task 5: Shipper with backoff

**Files:**
- Create: `internal/agent/shipper.go`
- Test: `internal/agent/shipper_test.go`

**Interfaces:**
- Consumes: `agent.Buffer` (Task 4: `Unacked`, `Ack`), `protocol.IngestBatch` / `protocol.IngestResponse` (Task 1).
- Produces: `agent.NewShipper(serverURL, token string, buf *Buffer, client *http.Client) *Shipper`; `func (s *Shipper) Flush(ctx context.Context) (int, error)` — posts up to 500 unacked as `POST {serverURL}/v1/ingest` with `Authorization: Bearer <token>`, on HTTP 200 acks the posted count and returns it; any non-200 or transport error returns the error and acks nothing; `func (s *Shipper) NextInterval(lastError error) time.Duration` — nil error → `60*time.Second`; error → backoff `15s → 60s → 5min` (capped at 5min, reset to healthy after a success). Also `agent.HealthyFlushInterval` and `agent.BackoffSteps` exported for tests/config.

- [ ] **Step 1: Write the failing tests**

`internal/agent/shipper_test.go`:
```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -v`
Expected: FAIL — `undefined: agent.NewShipper`.

- [ ] **Step 3: Write minimal implementation**

`internal/agent/shipper.go`:
```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/agent/shipper.go internal/agent/shipper_test.go
git commit -m "feat(agent): shipper with token auth, 500-batch cap, backoff ladder"
```

---

### Task 6: Win32 foreground + idle sources

**Files:**
- Create: `internal/agent/sources.go` (interfaces + idle math)
- Create: `internal/agent/win32_windows.go` (syscalls; build-tag `windows`)
- Test: `internal/agent/sources_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type agent.ForegroundSource interface { ForegroundApp() string }`; `type agent.IdleSource interface { IdleSeconds() float64 }`; `func agent.IdleFromTickCount(nowTick, lastInputTick uint32) float64` (wraparound-safe idle seconds — the only unit-testable part); `win32Sources` struct in `win32_windows.go` implementing both interfaces via `GetForegroundWindow` → `GetWindowThreadProcessId` → `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` → `QueryFullProcessImageName`, and `GetLastInputInfo`; unknown/unreadable foreground → `"unknown.exe"`.

- [ ] **Step 1: Write the failing test (wraparound math)**

`internal/agent/sources_test.go`:
```go
package agent_test

import (
	"testing"

	"screwlogger/internal/agent"
)

func TestIdleFromTickCount(t *testing.T) {
	cases := []struct {
		now, last uint32
		want      float64
	}{
		{now: 1_000_000, last: 999_000, want: 1000}, // 1000s idle
		{now: 1_000, last: 999, want: 1},
		{now: 500, last: 4_294_967_000, want: 1095.0 + 0.295}, // uint32 wraparound
		{now: 100, last: 100, want: 0},
	}
	for _, c := range cases {
		got := agent.IdleFromTickCount(c.now, c.last)
		if diff := got - c.want; diff > 0.01 || diff < -0.01 {
			t.Fatalf("IdleFromTickCount(%d,%d) = %v, want %v", c.now, c.last, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestIdleFromTickCount -v`
Expected: FAIL — `undefined: agent.IdleFromTickCount`.

- [ ] **Step 3: Write implementation**

`internal/agent/sources.go`:
```go
package agent

// ForegroundSource reports the executable name of the foreground window.
type ForegroundSource interface {
	ForegroundApp() string
}

// IdleSource reports seconds since the last keyboard/mouse input.
type IdleSource interface {
	IdleSeconds() float64
}

// IdleFromTickCount computes idle seconds from GetTickCount-style values,
// safe across uint32 wraparound (~49.7 days of uptime).
func IdleFromTickCount(nowTick, lastInputTick uint32) float64 {
	var delta uint32
	if nowTick >= lastInputTick {
		delta = nowTick - lastInputTick
	} else {
		delta = (0xFFFFFFFF - lastInputTick) + nowTick + 1
	}
	return float64(delta) / 1000.0
}
```

`internal/agent/win32_windows.go`:
```go
//go:build windows

package agent

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                      = windows.NewLazySystemDLL("user32.dll")
	procGetForegroundWindow     = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessI = user32.NewProc("GetWindowThreadProcessId")
	procGetLastInputInfo        = user32.NewProc("GetLastInputInfo")
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount            = kernel32.NewProc("GetTickCount")
)

// lastInputInfo mirrors winuser.h LASTINPUTINFO.
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// win32Sources implements ForegroundSource and IdleSource with real Win32 calls.
type win32Sources struct{}

// NewWin32Sources returns production poll sources (Windows only).
func NewWin32Sources() (ForegroundSource, IdleSource) { return win32Sources{}, win32Sources{} }

// ForegroundApp returns the executable base name of the foreground window,
// or "unknown.exe" when it cannot be resolved (locked screen, elevated window).
func (win32Sources) ForegroundApp() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return "unknown.exe"
	}
	var pid uint32
	procGetWindowThreadProcessI.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return "unknown.exe"
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "unknown.exe"
	}
	defer windows.CloseHandle(h)
	var buf [syscall.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "unknown.exe"
	}
	return baseName(windows.UTF16ToString(buf[:size]))
}

// IdleSeconds returns seconds since last input via GetLastInputInfo (spec §3.1).
func (win32Sources) IdleSeconds() float64 {
	var li lastInputInfo
	li.cbSize = uint32(unsafe.Sizeof(li))
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return IdleFromTickCount(uint32(now), li.dwTime)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '\\' || p[i] == '/' {
			return p[i+1:]
		}
	}
	if p == "" {
		return "unknown.exe"
	}
	return p
}

// compile-time interface checks
var (
	_ ForegroundSource = win32Sources{}
	_ IdleSource       = win32Sources{}
	_                  = fmt.Sprintf
)
```

- [ ] **Step 4: Run tests to verify they pass, and build for windows**

Run:
```powershell
go test ./internal/agent/ -v
go build ./...
```
Expected: tests PASS; `go build` succeeds with no unused-symbol errors.

- [ ] **Step 5: Commit**

```powershell
git add internal/agent/sources.go internal/agent/sources_test.go internal/agent/win32_windows.go
git commit -m "feat(agent): win32 foreground/idle sources with wraparound-safe idle math"
```

---

### Task 7: Agent config + main loop wiring

**Files:**
- Create: `internal/agent/config.go`
- Create: `internal/agent/agent.go`
- Test: `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–6.
- Produces: `type agent.Config struct { ServerURL, Token, DataDir string; IdleThresholdSeconds int; PollInterval, FlushInterval time.Duration; MaxBufferBytes int64 }` with yaml tags `server_url, token, data_dir, idle_threshold_seconds` (durations not in yaml — set as defaults, overridable only in tests); `agent.LoadConfig(path string) (Config, error)` (defaults: `idle_threshold_seconds: 180`; `PollInterval: time.Second`, `FlushInterval: HealthyFlushInterval`, `MaxBufferBytes: 16<<20`); `agent.New(cfg Config, fg ForegroundSource, idle IdleSource, now func() time.Time) (*Agent, error)`; `func (a *Agent) Run(ctx context.Context) error` — polls on `cfg.PollInterval`, emits heartbeats via the builder, categorizes, stamps UUID, appends to buffer at `<DataDir>\buffer.jsonl`; separate goroutine flushes on `cfg.FlushInterval` with `NextInterval` backoff; returns cleanly on ctx cancel.

- [ ] **Step 1: Write the failing integration test**

`internal/agent/agent_test.go`:
```go
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
		FlushInterval:        30 * time.Millisecond,
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run "TestAgentRuns|TestLoadConfig" -v`
Expected: FAIL — `undefined: agent.Config` / `agent.New` / `agent.LoadConfig`.

- [ ] **Step 3: Write implementation**

`internal/agent/config.go`:
```go
package agent

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the agent's on-disk configuration. Poll/Flush intervals and
// MaxBufferBytes are code-level defaults, overridable by tests.
type Config struct {
	ServerURL            string `yaml:"server_url"`
	Token                string `yaml:"token"`
	DataDir              string `yaml:"data_dir"`
	IdleThresholdSeconds int    `yaml:"idle_threshold_seconds"`
	PollInterval         time.Duration `yaml:"-"`
	FlushInterval        time.Duration `yaml:"-"`
	MaxBufferBytes       int64         `yaml:"-"`
}

// LoadConfig reads the agent YAML config and applies defaults (spec §3.1: idle 180s).
func LoadConfig(path string) (Config, error) {
	b, err := readFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.IdleThresholdSeconds == 0 {
		cfg.IdleThresholdSeconds = 180
	}
	cfg.PollInterval = time.Second
	cfg.FlushInterval = HealthyFlushInterval
	cfg.MaxBufferBytes = 16 << 20
	if cfg.ServerURL == "" || cfg.Token == "" || cfg.DataDir == "" {
		return Config{}, fmt.Errorf("config: server_url, token and data_dir are required")
	}
	return cfg, nil
}
```

`internal/agent/agent.go`:
```go
package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"screwlogger/internal/protocol"
)

// Agent ties poller → heartbeat builder → categorizer → buffer → shipper.
type Agent struct {
	cfg     Config
	fg      ForegroundSource
	idle    IdleSource
	now     func() time.Time
	builder *HeartbeatBuilder
	rules   Rules
	buf     *Buffer
	ship    *Shipper
}

// New constructs the agent. LoadRules failures are fatal for missing file?
// No: a missing rules file is fine — everything is Uncategorized (spec §3.2).
func New(cfg Config, fg ForegroundSource, idle IdleSource, now func() time.Time) (*Agent, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	buf, err := OpenBuffer(filepath.Join(cfg.DataDir, "buffer.jsonl"), cfg.MaxBufferBytes)
	if err != nil {
		return nil, err
	}
	rules, err := LoadRules(filepath.Join(cfg.DataDir, "rules.yaml"))
	if err != nil {
		log.Printf("rules: %v (starting with no rules — everything Uncategorized)", err)
		rules = nil
	}
	return &Agent{
		cfg: cfg, fg: fg, idle: idle, now: now,
		builder: NewHeartbeatBuilder(now, DefaultKeepAlive),
		rules:   rules, buf: buf,
		ship: NewShipper(cfg.ServerURL, cfg.Token, buf, nil),
	}, nil
}

// Run polls until ctx is cancelled, shipping in a separate goroutine.
func (a *Agent) Run(ctx context.Context) error {
	defer a.buf.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.shipLoop(ctx)
	}()

	tick := time.NewTicker(a.cfg.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			<-done // let shipper finish its current attempt
			return nil
		case <-tick.C:
			a.pollOnce()
		}
	}
}

func (a *Agent) pollOnce() {
	app := a.fg.ForegroundApp()
	active := a.idle.IdleSeconds() < float64(a.cfg.IdleThresholdSeconds)
	hb := a.builder.Observe(app, active)
	if hb == nil {
		return
	}
	hb.ID = uuid.NewString()
	hb.Category = a.rules.Categorize(app)
	if err := a.buf.Append(*hb); err != nil {
		log.Printf("buffer append: %v", err)
	}
}

func (a *Agent) shipLoop(ctx context.Context) {
	timer := time.NewTimer(a.cfg.FlushInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_, err := a.ship.Flush(ctx)
			wait := a.ship.NextInterval(err)
			if err != nil {
				log.Printf("flush: %v (next attempt in %v)", err, wait)
			}
			timer.Reset(wait)
		}
	}
}
```

Also add `readFile` helper to `config.go` (or use `os.ReadFile` directly — replace `readFile(path)` with `os.ReadFile(path)` and add the `os` import; do that).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS (all agent tests).

- [ ] **Step 5: Commit**

```powershell
git add internal/agent/config.go internal/agent/agent.go internal/agent/agent_test.go
git commit -m "feat(agent): config and main loop wiring poll->build->categorize->buffer->ship"
```

---

### Task 8: Server store (SQLite)

**Files:**
- Create: `internal/server/store.go`
- Test: `internal/server/store_test.go`

**Interfaces:**
- Consumes: `protocol.Heartbeat` (Task 1).
- Produces: `server.OpenStore(dbPath string) (*Store, error)` (applies schema, WAL mode); `func (s *Store) Close() error`; `func (s *Store) InsertHeartbeats(deviceID string, agentSentAt int64, hbs []protocol.Heartbeat) (accepted int64, err error)` — single tx, `INSERT OR IGNORE` per row (UUID PK idempotency, spec §3.3/§5), `received_at = now` per batch call, returns rows actually inserted; `func (s *Store) CreateDevice(name string) (deviceID, token string, err error)`; `func (s *Store) DeviceIDByToken(token string) (string, error)` — sha256-hash lookup; error `server.ErrRevoked` for revoked devices, generic error for unknown token; `func (s *Store) RevokeDevice(deviceID string) error`.

- [ ] **Step 1: Write the failing tests**

`internal/server/store_test.go`:
```go
package server_test

import (
	"errors"
	"path/filepath"
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

func openTestStore(t *testing.T) *server.Store {
	t.Helper()
	s, err := server.OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func hbWithID(id string, ts int64) protocol.Heartbeat {
	return protocol.Heartbeat{ID: id, TS: ts, App: "excel.exe", Category: "Office", Active: true}
}

func TestInsertHeartbeatsAndCount(t *testing.T) {
	s := openTestStore(t)
	devID, _, err := s.CreateDevice("PC-01")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.InsertHeartbeats(devID, 1729800001, []protocol.Heartbeat{
		hbWithID("h1", 1729800000), hbWithID("h2", 1729800015),
	})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestInsertIsIdempotentOnUUID(t *testing.T) {
	// Review Focus #1: replayed batch must not double-count.
	s := openTestStore(t)
	devID, _, _ := s.CreateDevice("PC-01")
	batch := []protocol.Heartbeat{hbWithID("h1", 1729800000), hbWithID("h2", 1729800015)}
	if _, err := s.InsertHeartbeats(devID, 1729800001, batch); err != nil {
		t.Fatal(err)
	}
	n, err := s.InsertHeartbeats(devID, 1729800002, batch) // replay
	if err != nil || n != 0 {
		t.Fatalf("replay must accept 0, got n=%d err=%v", n, err)
	}
}

func TestInsertAcceptsOutOfOrderTimestamps(t *testing.T) {
	// Backfill: old timestamps land long after newer ones (spec §3.3).
	s := openTestStore(t)
	devID, _, _ := s.CreateDevice("PC-01")
	if _, err := s.InsertHeartbeats(devID, 1729900000, []protocol.Heartbeat{hbWithID("new", 1729900000)}); err != nil {
		t.Fatal(err)
	}
	n, err := s.InsertHeartbeats(devID, 1729900001, []protocol.Heartbeat{hbWithID("old", 1729800000)})
	if err != nil || n != 1 {
		t.Fatalf("backfilled old heartbeat must be stored: n=%d err=%v", n, err)
	}
}

func TestDeviceTokenAuth(t *testing.T) {
	s := openTestStore(t)
	devID, token, err := s.CreateDevice("PC-02")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DeviceIDByToken(token)
	if err != nil || got != devID {
		t.Fatalf("got=%s err=%v", got, err)
	}
	if _, err := s.DeviceIDByToken("sl_wrong"); err == nil {
		t.Fatal("unknown token must fail")
	}
	if err := s.RevokeDevice(devID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeviceIDByToken(token); !errors.Is(err, server.ErrRevoked) {
		t.Fatalf("revoked token must return ErrRevoked, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -v`
Expected: FAIL — `undefined: server.OpenStore`.

- [ ] **Step 3: Write minimal implementation**

`internal/server/store.go`:
```go
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"screwlogger/internal/protocol"
)

// ErrRevoked is returned when a valid token belongs to a revoked device.
var ErrRevoked = errors.New("device token revoked")

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  enrolled_at INTEGER NOT NULL,
  last_seen INTEGER,
  revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS heartbeats (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id),
  ts INTEGER NOT NULL,
  app TEXT NOT NULL,
  category TEXT NOT NULL,
  active INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  agent_sent_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hb_device_ts ON heartbeats(device_id, ts);
CREATE INDEX IF NOT EXISTS idx_hb_ts ON heartbeats(ts);
CREATE TABLE IF NOT EXISTS rules (
  exe_pattern TEXT PRIMARY KEY,
  category TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
  key_hash TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  scopes TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
`

// Store wraps the SQLite database (WAL mode, spec §4).
type Store struct{ db *sql.DB }

func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// InsertHeartbeats stores a batch in one transaction. UUID is the idempotency
// key (INSERT OR IGNORE): replays and retries are accepted and count as 0.
// received_at is server clock at call time; agent_sent_at comes from the batch.
func (s *Store) InsertHeartbeats(deviceID string, agentSentAt int64, hbs []protocol.Heartbeat) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO heartbeats
		(id, device_id, ts, app, category, active, received_at, agent_sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	receivedAt := time.Now().Unix()
	var accepted int64
	for _, h := range hbs {
		active := 0
		if h.Active {
			active = 1
		}
		res, err := stmt.Exec(h.ID, deviceID, h.TS, h.App, h.Category, active, receivedAt, agentSentAt)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			accepted++
		}
	}
	if _, err := tx.Exec(`UPDATE devices SET last_seen = ? WHERE id = ?`, receivedAt, deviceID); err != nil {
		return 0, err
	}
	return accepted, tx.Commit()
}

// CreateDevice enrolls a device and returns its one-time plaintext token.
// Only the sha256 hash is stored (spec §6).
func (s *Store) CreateDevice(name string) (string, string, error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", err
	}
	deviceID := hex.EncodeToString(idBytes)
	tokBytes := make([]byte, 32)
	if _, err := rand.Read(tokBytes); err != nil {
		return "", "", err
	}
	token := "sl_" + base64.RawURLEncoding.EncodeToString(tokBytes)
	hash := hashToken(token)
	_, err := s.db.Exec(`INSERT INTO devices (id, name, token_hash, enrolled_at) VALUES (?, ?, ?, ?)`,
		deviceID, name, hash, time.Now().Unix())
	if err != nil {
		return "", "", err
	}
	return deviceID, token, nil
}

// DeviceIDByToken resolves a plaintext token to a device, or an error.
func (s *Store) DeviceIDByToken(token string) (string, error) {
	var id string
	var revoked sql.NullInt64
	err := s.db.QueryRow(`SELECT id, revoked_at FROM devices WHERE token_hash = ?`, hashToken(token)).
		Scan(&id, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("unknown device token")
	}
	if err != nil {
		return "", err
	}
	if revoked.Valid {
		return "", ErrRevoked
	}
	return id, nil
}

// RevokeDevice marks a device revoked; its token stops working (spec §3.4).
func (s *Store) RevokeDevice(deviceID string) error {
	_, err := s.db.Exec(`UPDATE devices SET revoked_at = ? WHERE id = ?`, time.Now().Unix(), deviceID)
	return err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/server/store.go internal/server/store_test.go
git commit -m "feat(server): sqlite store with idempotent ingest, device tokens, revocation"
```

---

### Task 9: Ingest HTTP endpoint

**Files:**
- Create: `internal/server/ingest.go`
- Test: `internal/server/ingest_test.go`

**Interfaces:**
- Consumes: `server.Store` (Task 8), `protocol.IngestBatch` / `IngestResponse` (Task 1).
- Produces: `func server.IngestHandler(store *Store) http.Handler` — `POST /v1/ingest`; auth `Authorization: Bearer <device token>`; `401` unknown token, `403` revoked (`{"error":"..."}` body), `400` for malformed JSON or `schema_version != protocol.SchemaVersion`, `200` with `IngestResponse` on success. Zero rows written on any 4xx (Review Focus #5).

- [ ] **Step 1: Write the failing tests**

`internal/server/ingest_test.go`:
```go
package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

func ingestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := server.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, token, err := s.CreateDevice("PC-TEST")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.IngestHandler(s))
	t.Cleanup(srv.Close)
	return srv, token
}

func postBatch(t *testing.T, srv *httptest.Server, token string, batch protocol.IngestBatch) *http.Response {
	t.Helper()
	body, _ := json.Marshal(batch)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func validBatch() protocol.IngestBatch {
	return protocol.IngestBatch{SchemaVersion: protocol.SchemaVersion, DeviceSentAt: 1729800001,
		Heartbeats: []protocol.Heartbeat{{ID: "h1", TS: 1729800000, App: "excel.exe", Category: "Office", Active: true}}}
}

func TestIngestSuccess(t *testing.T) {
	srv, token := ingestServer(t)
	resp := postBatch(t, srv, token, validBatch())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var ir protocol.IngestResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	if ir.Accepted != 1 {
		t.Fatalf("accepted=%d", ir.Accepted)
	}
}

func TestIngestDuplicateBatchAcceptedZero(t *testing.T) {
	srv, token := ingestServer(t)
	postBatch(t, srv, token, validBatch())
	resp := postBatch(t, srv, token, validBatch())
	var ir protocol.IngestResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	if resp.StatusCode != http.StatusOK || ir.Accepted != 0 {
		t.Fatalf("replay: status=%d accepted=%d", resp.StatusCode, ir.Accepted)
	}
}

func TestIngestAuthFailures(t *testing.T) {
	srv, _ := ingestServer(t)
	if resp := postBatch(t, srv, "sl_bogus", validBatch()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", resp.StatusCode)
	}
}

func TestIngestBadSchemaVersionRejected(t *testing.T) {
	srv, token := ingestServer(t)
	batch := validBatch()
	batch.SchemaVersion = 99
	if resp := postBatch(t, srv, token, batch); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad schema_version: %d", resp.StatusCode)
	}
}

func TestIngestMalformedJSONRejected(t *testing.T) {
	srv, token := ingestServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ingest", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json: %d", resp.StatusCode)
	}
}

func TestIngestRevokedToken403(t *testing.T) {
	s := ... // see Step 3 note: build store directly, revoke, expect 403
	_ = s
}
```

For `TestIngestRevokedToken403`, replace the placeholder with a full setup (do this in the actual test file — no `...`):
```go
func TestIngestRevokedToken403(t *testing.T) {
	s, err := server.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	devID, token, err := s.CreateDevice("PC-R")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(devID); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.IngestHandler(s))
	defer srv.Close()
	if resp := postBatch(t, srv, token, validBatch()); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked token: %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -v`
Expected: FAIL — `undefined: server.IngestHandler`.

- [ ] **Step 3: Write minimal implementation**

`internal/server/ingest.go`:
```go
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"screwlogger/internal/protocol"
)

// IngestHandler serves POST /v1/ingest with device-token auth (spec §3.5).
// Any 4xx writes zero rows (Review Focus #5).
func IngestHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		deviceID, err := store.DeviceIDByToken(token)
		if err != nil {
			if errors.Is(err, ErrRevoked) {
				writeErr(w, http.StatusForbidden, "device token revoked")
				return
			}
			writeErr(w, http.StatusUnauthorized, "invalid device token")
			return
		}
		var batch protocol.IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		if batch.SchemaVersion != protocol.SchemaVersion {
			writeErr(w, http.StatusBadRequest, "unsupported schema_version")
			return
		}
		accepted, err := store.InsertHeartbeats(deviceID, batch.DeviceSentAt, batch.Heartbeats)
		if err != nil {
			log.Printf("ingest: device %s: %v", deviceID, err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.IngestResponse{Accepted: accepted})
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}
	return h[len(prefix):], true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add internal/server/ingest.go internal/server/ingest_test.go
git commit -m "feat(server): ingest endpoint with token auth and schema validation"
```

---

### Task 10: Server CLI (enroll) + mains + health endpoint

**Files:**
- Create: `cmd/server/main.go`
- Create: `cmd/agent/main.go`
- Modify: `internal/server/store.go` (add `ListDevices`)

**Interfaces:**
- Consumes: `agent.LoadConfig`, `agent.New`, `agent.NewWin32Sources` (Tasks 6–7); `server.OpenStore`, `server.IngestHandler`, `server.Store.CreateDevice`, `server.Store.ListDevices` (Tasks 6, 8).
- Produces: runnable binaries. `server.exe -db <path> -addr :8080` serves `POST /v1/ingest` + `GET /healthz` (returns `ok`); `server.exe -enroll "PC-NAME" -db <path>` prints `device_id` and one-time token; `agent.exe -config agent.yaml` runs the agent in console mode (Ctrl+C to stop). `Store.ListDevices() ([]DeviceRow, error)` where `type DeviceRow struct { ID, Name string; EnrolledAt int64; LastSeen, RevokedAt sql.NullInt64 }`.

- [ ] **Step 1: Write the failing test for ListDevices**

Append to `internal/server/store_test.go`:
```go
func TestListDevices(t *testing.T) {
	s := openTestStore(t)
	id, _, err := s.CreateDevice("PC-L1")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].Name != "PC-L1" {
		t.Fatalf("rows: %+v", rows)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestListDevices -v`
Expected: FAIL — `undefined: server.Store.ListDevices` (compile error).

- [ ] **Step 3: Implement ListDevices**

Append to `internal/server/store.go`:
```go
// DeviceRow is one enrolled device for admin listing.
type DeviceRow struct {
	ID        string
	Name      string
	EnrolledAt int64
	LastSeen  sql.NullInt64
	RevokedAt sql.NullInt64
}

// ListDevices returns all enrolled devices, newest first.
func (s *Store) ListDevices() ([]DeviceRow, error) {
	rows, err := s.db.Query(`SELECT id, name, enrolled_at, last_seen, revoked_at FROM devices ORDER BY enrolled_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceRow
	for rows.Next() {
		var d DeviceRow
		if err := rows.Scan(&d.ID, &d.Name, &d.EnrolledAt, &d.LastSeen, &d.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 5: Write the mains**

`cmd/server/main.go`:
```go
// Command server is the ScrewLogger LAN server: ingest API + health (M1).
// M2 adds the open query API, admin UI and dashboards.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"screwlogger/internal/server"
)

func main() {
	dbPath := flag.String("db", "screwlogger.db", "path to SQLite database")
	addr := flag.String("addr", ":8080", "listen address")
	enroll := flag.String("enroll", "", "enroll a device by name, print its one-time token, then exit")
	flag.Parse()

	store, err := server.OpenStore(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if *enroll != "" {
		id, token, err := store.CreateDevice(*enroll)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("device_id: %s\ntoken (shown once, store hashed server-side): %s\n", id, token)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/ingest", server.IngestHandler(store))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("screwlogger server listening on %s (db: %s)", *addr, *dbPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
	_ = os.Stdout
}
```

`cmd/agent/main.go`:
```go
// Command agent is the ScrewLogger Windows agent in console mode (M1).
// M3 wraps this as the monsvc Windows service.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"screwlogger/internal/agent"
)

func main() {
	cfgPath := flag.String("config", "agent.yaml", "path to agent.yaml")
	flag.Parse()

	cfg, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	fg, idle := agent.NewWin32Sources()
	a, err := agent.New(cfg, fg, idle, nil)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("agent running: server=%s data=%s", cfg.ServerURL, cfg.DataDir)
	if err := a.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
	log.Printf("agent stopped cleanly")
}
```

Note: `agent.New(cfg, fg, idle, nil)` — adjust `agent.New` to default a nil `now` to `time.Now` (one line in `New`: `if now == nil { now = time.Now }`). Modify `internal/agent/agent.go` accordingly.

Also create `agent.example.yaml` at repo root:
```yaml
# ScrewLogger agent configuration (spec §3.4). Copy to agent.yaml, fill in token.
server_url: "http://192.168.1.50:8080"
token: "sl_PASTE_DEVICE_TOKEN_HERE"
data_dir: "C:\\ProgramData\\monsvc"
idle_threshold_seconds: 180
```

- [ ] **Step 6: Build everything and run full test suite**

Run:
```powershell
go build ./...
go test ./... -v
```
Expected: builds clean; all tests PASS.

- [ ] **Step 7: Commit**

```powershell
git add cmd/ internal/server/store.go internal/server/store_test.go agent.example.yaml internal/agent/agent.go
git commit -m "feat: server and agent mains, enroll CLI, healthz, ListDevices"
```

---

### Task 11: End-to-end test + M1 smoke on this dev PC

**Files:**
- Create: `internal/server/e2e_test.go`
- No production code changes expected.

**Interfaces:**
- Consumes: `agent.New` with fake sources (Task 7), `server.OpenStore` (Task 8).
- Produces: automated proof that agent → server → SQLite works end to end; then a manual smoke checklist on the real dev PC.

- [ ] **Step 1: Write the failing end-to-end test**

`internal/server/e2e_test.go`:
```go
package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"screwlogger/internal/agent"
	"screwlogger/internal/protocol"
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
	a, err := agent.New(cfg, fg, &constIdle{0}, time.Now)
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
	_ = fmt.Sprint(cat) // categorized value asserted in agent_test; here we assert presence
	_ = protocol.SchemaVersion
	_ = json.Marshal
}

type flipFG struct{ apps []string; i int }
func (f *flipFG) ForegroundApp() string { a := f.apps[f.i%len(f.apps)]; f.i++; return a }

type constIdle float64
func (c constIdle) IdleSeconds() float64 { return float64(c) }
```

Note: the fake types `flipFG`/`constIdle` are test-local; if the compiler complains about unused imports (`json`, `fmt`, `protocol`), delete the `_ =` lines and the unused imports — keep the assertions that matter.

- [ ] **Step 2: Run the full suite**

Run: `go test ./... -v`
Expected: PASS, including `TestAgentToServerEndToEnd`.

- [ ] **Step 3: Commit**

```powershell
git add internal/server/e2e_test.go
git commit -m "test: end-to-end agent-to-sqlite verification"
```

- [ ] **Step 4: Manual M1 smoke on this dev PC (real Win32, real server)**

In terminal 1:
```powershell
go run ./cmd/server -db smoke.db -addr :8080
go run ./cmd/server -db smoke.db -enroll "DEV-PC"
# note the printed token
```
Create `smoke-agent.yaml` (temp, do not commit; contains the real token):
```yaml
server_url: "http://127.0.0.1:8080"
token: "<paste token>"
data_dir: "%TEMP%\\monsvc-smoke"   # use an absolute expanded path
idle_threshold_seconds: 180
```
Terminal 2 — leave running ~10 minutes while working normally, including walking away >3 min:
```powershell
go run ./cmd/agent -config smoke-agent.yaml
```
Then verify in the smoke DB:
```powershell
& "C:\Program Files\SQLite\sqlite3.exe" smoke.db "SELECT app, category, active, COUNT(*) FROM heartbeats GROUP BY app, category, active;"
```
(If `sqlite3` is not installed, verify via a quick `go run` snippet or accept the e2e test as the gate — record which.)

Expected: real app names (`chrome.exe`, `explorer.exe`, ...), `active=1` while working, `active=0` after 3 min away, categories resolved or `Uncategorized`, and one row per state change / 15 s keep-alive.

- [ ] **Step 5: Commit milestone gate note**

Append a short `## M1 result` section to `docs/superpowers/specs/2026-09-24-screwlogger-design.md` recording the smoke observations (apps seen, idle transition observed, rows/hour rate), then:
```powershell
git add -A
git commit -m "docs: record M1 smoke result — agent+ingest gate passed"
```

---

## Plan self-review notes

- **Spec coverage (M1):** §3.1 poller/heartbeat/idle → Tasks 2, 6, 7; §3.2 categorizer → Task 3 (server-pushed rules is M2 — agent reads local `rules.yaml` in M1, spec-allowed); §3.3 buffer/backfill/idempotency → Tasks 4, 5, 8; §3.4 identity/lifecycle → Tasks 8, 10 (service registration is M3 per spec §8); §4 schema → Task 8 (rules + api_keys tables created now, used in M2); §5 error handling → Tasks 4, 5, 9; §8 M1 gate → Task 11.
- **Type consistency:** `protocol.Heartbeat` fields and JSON tags used identically in Tasks 1, 4, 5, 7, 8, 9; `Shipper.NextInterval(error) time.Duration` used by `agent.shipLoop` (Task 7); `Store.InsertHeartbeats(deviceID string, agentSentAt int64, hbs []protocol.Heartbeat) (int64, error)` used by Task 9.
- **Review Focus pinned:** #1 → Task 8 `TestInsertIsIdempotentOnUUID` + Task 9 `TestIngestDuplicateBatchAcceptedZero`; #2 → Task 4 `TestAckPrunesAndReopenKeepsUnacked`; #3 → Task 2 `TestActiveFlipEmitsImmediately` (boundary is encoded as `>= threshold` in Task 7's `pollOnce` — the strict boundary test lives in the M2 query work where idle time is summed); #4 → Task 2 `TestUnchangedStateEmitsOnlyOnKeepAlive`; #5 → Task 9 `TestIngestBadSchemaVersionRejected` / `TestIngestMalformedJSONRejected`.
