# ScrewLogger

A low-overhead Windows **usage / productivity monitor** for factory PCs. It records how much
time is spent in each application and whether the user was actively working or idle, and
reports that to a server on the LAN.

> **Not a keylogger.** ScrewLogger captures no keystrokes, no window titles, and no file paths.
> The only data that ever leaves a PC is `{app, category, active, ts}` — which application is in
> the foreground, a locally-resolved category, an active/idle flag, and a timestamp.

## Architecture

```
┌─────────────────────────┐          heartbeats (Bearer token)        ┌──────────────────────────┐
│  Agent (Windows PC)     │ ────────────────────────────────────────▶ │  Server (LAN)            │
│  foreground/idle sources │    POST /v1/ingest                       │  ingest → SQLite (WAL)   │
│  heartbeat builder       │  ◀────────────────────────────────────── │  device tokens (sha256)  │
│  categorizer (rules.yaml)│              ack + backoff               │  enroll / healthz        │
│  durable JSONL buffer    │                                          │  (query API: M2)         │
│  shipper                 │                                          └──────────────────────────┘
└─────────────────────────┘
```

- **Heartbeat model** (ActivityWatch-style): an event is emitted on app change, active/idle
  flip, or a 15s keep-alive; the server reconstructs dwell time. A 30s gap cap means a PC
  that's off or suspended never fabricates time.
- **Lossless + idempotent**: heartbeats are buffered to disk (fsync per append) and replayed
  with their original timestamps; the server dedupes by UUID, so retries and offline backfill
  are safe.
- **All Go, single dependency-free SQLite** (`modernc.org/sqlite`, pure Go — no CGO).

## Repository layout

```
cmd/agent/          agent entrypoint (console mode + -install/-uninstall/-upgrade/-service)
cmd/server/         server entrypoint (ingest API + healthz)
internal/agent/     sources, heartbeat builder, categorizer, buffer, shipper, config, loop, service, installer
internal/protocol/  wire types (Heartbeat, IngestBatch, IngestResponse)
internal/server/    store (SQLite), ingest handler
docs/superpowers/   design spec + implementation plan
.superpowers/sdd/   subagent-development ledger, task briefs, reviews
```

## Requirements

- **Go 1.23** (pinned `go 1.23.0` in `go.mod`)
- The **agent** runs on Windows only (Win32 foreground/idle APIs). The server builds anywhere.

## Build

```bash
go build ./cmd/server   # server
go build ./cmd/agent    # agent (on Windows)
```

Or cross-compile the agent for a target PC from any machine:

```bash
GOOS=windows GOARCH=amd64 go build -o monsvc.exe ./cmd/agent
```

## Quick start

### 1. Run the server

```bash
./server -db screwlogger.db -addr :8080
```

### 2. Enroll a PC (get its one-time token)

```bash
./server -db screwlogger.db -enroll PC-01
# prints: device_id: <id>
#         token (shown once, store hashed server-side): sl_...
```

### 3. Configure and run the agent

Copy `agent.example.yaml` → `agent.yaml` and fill in the token:

```yaml
server_url: "http://192.168.1.50:8080"
token: "sl_PASTE_DEVICE_TOKEN_HERE"
data_dir: "C:\\ProgramData\\monsvc"
idle_threshold_seconds: 180
```

```bash
monsvc.exe -config agent.yaml
```

The agent polls the foreground window + idle state (default 1s), buffers heartbeats, and
flushes to the server every 60s (backing off 15s → 60s → 5min on failures).

Alternatively, install it as the `monsvc` service instead of running it in the
foreground:

```bash
# from an elevated (Administrator) prompt:
monsvc.exe -install -server http://192.168.1.50:8080 -token sl_...
# writes agent.yaml next to the binary and registers the monsvc service (auto-start)
```

### 4. Categorize apps (optional)

Drop a `rules.yaml` in the agent's `data_dir` to map executables to categories (case-insensitive
glob; exact match wins). Without it, everything reports as `Uncategorized`.

## Deployment

- **Install**: enroll the PC on the server first to get its `sl_` token (see Quick start), then
  from an elevated prompt run
  `monsvc.exe -install -server http://<server>:8080 -token sl_...`. The service runs as SYSTEM
  under the plain name `monsvc`, auto-starts at boot, and auto-restarts on failure (1s / 15s / 60s
  ladder).
- **Upgrade**: `monsvc.exe -upgrade -new C:\path\new\monsvc.exe` stops the service, replaces the
  binary, and restarts it — `agent.yaml` and the device token are untouched. Manual fallback: stop
  the service, replace the exe by hand, then start it again.
- **Uninstall**: `monsvc.exe -uninstall` (from an elevated prompt) stops and removes the service.
- **Data dir**: `-data-dir` defaults to `C:\ProgramData\monsvc`.

## API

| Method | Path          | Auth    | Description                                   |
|--------|---------------|---------|-----------------------------------------------|
| POST   | `/v1/ingest`  | Bearer  | Ingest a heartbeat batch (idempotent by UUID) |
| GET    | `/healthz`    | none    | Liveness check                                |

The open **query API** for other applications to fetch aggregated data is planned for M2.

## Privacy & deployment

The agent runs as a SYSTEM service (`monsvc`) — quiet (no tray icon, no Start-menu entry),
auto-start, and documented in employee policy. Tokens are stored server-side only as SHA-256
hashes and can be revoked. Device identity is assigned at install time via enrollment.

## Status

**M3 (this branch):** Windows service + installer (monsvc SYSTEM service, -install/-upgrade/-uninstall) complete.

**M1:** agent + ingest pipeline complete, full test suite green, final review clean.

Follow-ups tracked for M2: manual Win32 dev-PC smoke test, an open query API + dashboard, and a
set of deferred review minors (see `.superpowers/sdd/.../final-review-*.diff`).
