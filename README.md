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
│  durable JSONL buffer    │                                          │  (query API + admin UI) │
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
cmd/server/         server entrypoint (ingest, query API, admin UI)
internal/agent/     sources, heartbeat builder, categorizer, buffer, shipper, config, loop, service, installer
internal/protocol/  wire types (Heartbeat, IngestBatch, IngestResponse)
internal/server/    store (SQLite), ingest + query API + admin API + embedded dashboard
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

### Server flags

| Flag                | Description                                                       |
|---------------------|-------------------------------------------------------------------|
| `-db <path>`        | SQLite database file (default `screwlogger.db`)                   |
| `-addr <addr>`      | Listen address (default `:8080`)                                  |
| `-enroll <name>`    | Enroll a device by name, print its one-time `sl_` token, then exit |
| `-apikey <label>`   | Create a read-only API key with this label, print it once, then exit |
| `-admin-password`   | Admin password (empty generates a random one, logged once)        |

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

### Ingest & health

| Method | Path          | Auth    | Description                                   |
|--------|---------------|---------|-----------------------------------------------|
| POST   | `/v1/ingest`  | Bearer  | Ingest a heartbeat batch (idempotent by UUID) |
| GET    | `/healthz`    | none    | Liveness check                                |

### Query API (read-only)

An open, read-only API for other LAN applications to fetch aggregated usage data. Every route
requires `Authorization: Bearer ak_<key>` — API keys are read-only, stored SHA-256-hashed, and
created via the admin UI or the `-apikey <label>` flag. CORS `*` is enabled for `/api/v1/*`.

| Method | Path                  | Query params                          | Description                                              |
|--------|-----------------------|---------------------------------------|----------------------------------------------------------|
| GET    | `/api/v1/devices`     | —                                     | Enrolled devices (id, name, status, timestamps)          |
| GET    | `/api/v1/usage`       | `device`, `from`, `to`, `group_by`    | Dwell seconds bucketed by `category` (default) or `app`  |
| GET    | `/api/v1/active-ratio`| `device`, `from`, `to`                | Active vs idle seconds and ratio in the window           |
| GET    | `/api/v1/events`      | `device`, `from`, `to`, `limit`       | Raw heartbeats (`id`, `ts`, `app`, `category`, `active`) |

`device` may be empty for a fleet-wide query; `from`/`to` are unix seconds (default `0`/now);
`limit` is clamped to `[1, 10000]` (default 1000). Responses never contain device tokens or API
keys.

## Admin UI & dashboards

Run the server with an admin password, then open the dashboard:

```bash
./server -db screwlogger.db -admin-password 'change-me'
# open http://localhost:8080/admin/ and log in
```

If `-admin-password` is empty, a random password is generated and logged once at startup. The
dashboard is a single embedded page (no CDN — Chart.js is vendored) with four panels:

- **Devices** — enrolled PCs (name, last seen, status) plus an enroll form that shows the
  one-time `sl_` token once, and a revoke button.
- **Rules** — edit `pattern → category` rules with an "apply to history" action that re-maps
  existing heartbeats.
- **API keys** — create (shows the `ak_…` key once) and revoke read-only API keys.
- **Dashboards** — per-device or fleet dwell-by-category, dwell-by-app, active/idle ratio, and an
  hourly timeline.

## Privacy & deployment

The agent runs as a SYSTEM service (`monsvc`) — quiet (no tray icon, no Start-menu entry),
auto-start, and documented in employee policy. Tokens are stored server-side only as SHA-256
hashes and can be revoked. Device identity is assigned at install time via enrollment.

## Status

- **M1:** agent + ingest pipeline complete, full test suite green, final review clean.
- **M2:** query API + admin UI + dashboards complete.
- **M3:** Windows service + installer (monsvc SYSTEM service, -install/-upgrade/-uninstall) complete.

Next: manual Win32 dev-PC smoke test, then pilot (5–10 factory PCs) → fleet rollout.
