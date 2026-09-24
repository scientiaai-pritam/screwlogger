# ScrewLogger — Design Spec

**Date:** 2026-09-24
**Status:** Approved design, pending implementation plan
**Type:** Greenfield project — two deliverables: `monitor-agent` (Windows service) and `monitor-server` (LAN server with dashboards and open query API)

## 1. Purpose

Track how much time is spent in which categories of applications on factory PCs, and how much of that time a person was actively working versus idle — a productivity signal, not surveillance. Keystroke contents and screenshots are explicitly out of scope. Window titles are captured only on the PC, used for local categorization, and never leave the machine.

Success criteria:

- Agent CPU usage is unmeasurable at idle (<1% sustained), memory footprint ~10 MB.
- Dashboard shows per-device and fleet-wide time per category and active/idle ratio.
- An agent offline for days backfills its history with original timestamps, losslessly, idempotently.
- Other applications on the LAN can pull the data via an authenticated read-only REST API.
- Deployment per PC is: create identity on the server, run installer with token. No other manual steps.

## 2. Constraints and decisions

| Decision | Choice | Rationale |
|---|---|---|
| Stack | All Go | One language, two static `.exe` files, no runtime deps on PCs or server; fastest to build and deploy |
| Server storage | SQLite (WAL mode) | LAN box, hundreds of PCs max; zero database-server install; migrate to Postgres later if ever needed |
| Stealth level | Quiet service | Runs as SYSTEM, no tray icon, not user-stoppable, plain process name `monsvc`; documented in employee IT policy — unobtrusive, not forensically undetectable |
| Data granularity | App name + locally-resolved category + active flag | No window titles, no keystrokes leave the PC |
| Network | Agents on LAN, server on same LAN, behind firewall | Plain HTTP by default; HTTPS-ready via config |
| Fleet size | Tens to low hundreds of PCs, mostly online | Sizes SQLite comfortably |

## 3. Architecture

```
┌─────────────────────── Factory PC (×N) ───────────────────────┐
│  monsvc (Windows service, SYSTEM, no tray)                    │
│                                                               │
│  Poller (1s) ──► Heartbeat builder ──► Local categorizer      │
│   GetForegroundWindow → app exe        rules (pushed, cached) │
│   GetLastInputInfo → active/idle                              │
│                          │                                    │
│                          ▼                                    │
│              Buffer (rotating append-only JSONL, fsynced)     │
│                          │                                    │
│              Shipper (60s flush, exponential backoff)         │
└──────────────────────────┼────────────────────────────────────┘
                           │ POST /v1/ingest  (device token)
┌──────────────────────────▼────── LAN server ──────────────────┐
│  monitor-server.exe (single Go binary, one port, e.g. :8080)  │
│   POST /v1/ingest          → SQLite (WAL)                     │
│   GET  /api/v1/*           ← other apps w/ read API keys      │
│   /admin                   → enrollment, rules editor,        │
│                              dashboards (embedded Chart.js)   │
└───────────────────────────────────────────────────────────────┘
```

### 3.1 Agent: poller and heartbeat builder

- Polls once per second. Each poll is two Win32 calls (`GetForegroundWindow`, then process name lookup; `GetLastInputInfo`) — microseconds of work.
- Emits **heartbeats**, not per-second records: a heartbeat is emitted when the focused app changes, when active/idle state flips, or every 15 s as keep-alive on an unchanged state.
- **Idle threshold:** no keyboard or mouse input for >180 s (configurable) → heartbeat marked `active=false`. This is how "walked away" is distinguished from "app open but person elsewhere."
- Win32 calls sit behind a small interface so the state machine is unit-testable with fakes.

### 3.2 Agent: local categorizer

- Rules map executable names/patterns to categories (`excel.exe → Office`, `chrome.exe → Browser`, factory/MES software → `Production`).
- Rules are fetched from the server, cached locally, applied on-PC. Unknown apps fall into category `Uncategorized`; the server-side rules editor lets admins re-map after the fact (historical heartbeats keep the agent-resolved category, so rules updates apply to new data; a server-side re-categorization pass may re-map history — see §5).
- Only `{app, category, timestamps, active}` is ever recorded or transmitted.

### 3.3 Agent: buffer (backfill mechanism)

- Every heartbeat is appended to a rotating JSONL file (fsync on append) *before* any upload is attempted.
- Shipper flushes every 60 s; on success the server returns an ack cursor and the buffer prunes acknowledged entries.
- Offline for days → on reconnect the agent replays buffered heartbeats; the server stores them with original timestamps.
- Idempotency: each heartbeat carries a client-generated UUID; server does `INSERT OR IGNORE`, so retries and replays never double-count.
- Buffer rotates at 500 MB (configurable), dropping oldest only when full, logging loudly when that happens.

### 3.4 Agent: identity and lifecycle

- Identity = `device_id` + per-device token, created by an admin on the server (UI or API), delivered at install time via a config file next to the binary (or a one-line installer command).
- Revoked token → agent receives 403, stops shipping, keeps buffering, retries daily; re-issuing a token recovers the history.
- Service restarts on failure (standard Windows service recovery); auto-starts at boot; no tray icon, no Start-menu entry, not in Add/Remove Programs; visible in Task Manager as `monsvc`.

### 3.5 Server

Single Go binary, one HTTP port, three faces:

1. **Ingest API** (`POST /v1/ingest`, device-token auth): batches of heartbeats; accepts out-of-order timestamps (backfill); idempotent by heartbeat UUID; returns ack cursor. Batch carries `schema_version`; unknown versions are rejected loudly (HTTP 4xx with explicit error), never mis-stored.
2. **Open query API** (`GET /api/v1/*`, read-only API-key auth, CORS enabled) for other applications:
   - `GET /api/v1/devices` — enrolled devices, last seen, status
   - `GET /api/v1/usage?device=&from=&to=&group_by=category|app` — dwell time aggregates
   - `GET /api/v1/active-ratio?device=&from=&to=` — active vs idle split
   - `GET /api/v1/events?device=&from=&to=&limit=` — raw heartbeats
   - API keys stored hashed, scopes read-only, shown once at creation.
3. **Admin UI + dashboards**: device enrollment (issue identity/token), category rules editor, reports — time per category/app per device or fleet-wide, active/idle ratio, hourly/daily timelines (embedded Chart.js).

## 4. Data model (SQLite)

```sql
devices (
  id TEXT PRIMARY KEY, name TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  enrolled_at INTEGER NOT NULL, last_seen INTEGER, revoked_at INTEGER
)

heartbeats (
  id TEXT PRIMARY KEY,           -- client UUID (idempotency)
  device_id TEXT NOT NULL REFERENCES devices(id),
  ts INTEGER NOT NULL,           -- original event time, unix seconds
  app TEXT NOT NULL,
  category TEXT NOT NULL,
  active INTEGER NOT NULL,       -- 1 = human present, 0 = idle
  received_at INTEGER NOT NULL,  -- server clock
  agent_sent_at INTEGER NOT NULL -- agent flush time (backfill-lag visibility)
);
CREATE INDEX idx_hb_device_ts ON heartbeats(device_id, ts);
CREATE INDEX idx_hb_ts ON heartbeats(ts);

rules (exe_pattern TEXT PRIMARY KEY, category TEXT NOT NULL, updated_at INTEGER NOT NULL);

api_keys (
  key_hash TEXT PRIMARY KEY, label TEXT NOT NULL,
  scopes TEXT NOT NULL, created_at INTEGER NOT NULL, revoked_at INTEGER
);
```

**Dwell-time computation:** each heartbeat covers the interval until the next heartbeat for that device; an interval is capped at a max-duration constant (e.g. 2× keep-alive period) so gaps (PC off, buffer dropped) never fabricate time. Aggregation is SQL at query time; a daily-rollup table may be added later if dashboards get slow — not now (YAGNI), and the query API abstracts it.

## 5. Error handling

| Failure | Behavior |
|---|---|
| Agent crash / reboot | Buffer is fsynced JSONL; nothing after the last server ack is lost |
| Server unreachable | Shipper backs off 15 s → 60 s → 5 min ceiling; buffer keeps growing; oldest dropped only at 500 MB rotation with a loud log |
| Duplicate delivery | UUID primary key → `INSERT OR IGNORE` |
| Clock skew | Reports trust agent `ts`; server records `received_at`; admin UI flags devices with anomalous lag (`received_at − agent_sent_at`) or skew |
| Revoked token | Agent gets 403, stops shipping, buffers locally, retries daily |
| Schema drift | `schema_version` in every batch; unknown → explicit rejection |
| History re-categorization | Server-side rules change may trigger a re-mapping pass over stored heartbeats (admin-invoked, explicit) |

## 6. Security posture

- Service runs as SYSTEM with standard service ACLs; ordinary users cannot stop it; visible in Task Manager as `monsvc`.
- Plain HTTP on the LAN by default; config switch to HTTPS when a cert exists for the server box.
- Device tokens and API keys stored hashed; raw values displayed once at creation.
- Query API keys are read-only scopes.
- The system is documented in the employer's IT/employee policy; design goal is unobtrusive, not undetectable.

## 7. Testing

- **Unit:** heartbeat-builder state machine (focus change, idle flip, 15 s keep-alive), categorizer rule matching, shipper retry/backoff math. Win32 calls behind an interface, faked in tests.
- **Integration (server):** ingest idempotency; out-of-order backfill; query endpoints (usage, active-ratio, events); auth matrix (device token vs API key vs revoked vs unknown schema version).
- **Smoke (one real PC):** install agent → work normally 30 min including walking away → dashboard dwell times and active/idle ratio within tolerance; disconnect network 1 h → reconnect → backfill lands with original timestamps, no duplicates.

## 8. Rollout milestones

1. **M1 — agent + ingest:** agent runs on one dev PC, heartbeats land in SQLite with backfill/idempotency working.
2. **M2 — query API + admin UI + dashboards:** smoke test from §7 passes end-to-end.
3. **M3 — installer hardening:** service registration script, config-with-token installer, auto-start, upgrade path → pilot 5–10 factory PCs → fleet rollout.

## 9. Parity with ActivityWatch — and where ScrewLogger must be better

The bar: everything ActivityWatch does that matters for this use case, ScrewLogger does; everything ActivityWatch does badly for *fleet* use, ScrewLogger does natively.

| Capability | ActivityWatch | ScrewLogger requirement |
|---|---|---|
| Foreground app + dwell time | ✅ `aw-watcher-window` | ✅ Same heartbeat model (§3.1) |
| AFK / idle detection | ✅ `aw-watcher-afk` (180 s) | ✅ Same mechanism, configurable threshold |
| Category rules (categorize apps) | ✅ client-side, per-device manual config | ✅ **Better:** managed centrally on server, pushed to all agents (§3.2) |
| Web dashboard | ✅ local, per-device | ✅ **Better:** fleet-wide dashboards + per-device drill-down, per-category/app time, active/idle ratio, hourly/daily timelines |
| REST API for third parties | ⚠️ local-only, per-device | ✅ **Better:** central open query API with issued API keys, CORS, read-only scopes (§3.5) |
| Multi-device data collection | ❌ explicitly unsupported upstream; aw-sync + shared-folder workaround | ✅ **Better:** native central ingest — this is the core design |
| Offline resilience | ⚠️ local DB keeps data, no central backfill story | ✅ **Better:** lossless buffer + idempotent replay with original timestamps (§3.3) |
| Fleet management (enroll/revoke devices) | ❌ none | ✅ Device enrollment, token revocation, last-seen/lag monitoring |
| Low footprint | ✅ tens of MB | ✅ Par: ~10 MB RAM, sub-1% CPU |
| Browser per-tab/per-site tracking | ✅ via browser extension | ➖ Optional later milestone; app-level categorization covers the current requirement. Extension would follow ActivityWatch's model (local resolution → category only) |
| Editor/IDE/terminal watchers | ✅ community plugins | ➖ Out of scope until asked |

Parity gaps are tracked as backlog items (browser extension = post-M3), not scope creep now.

## 10. Out of scope

- Keystroke contents, screenshots, window-title transmission, network-traffic capture.
- Covert/anti-forensics features (watchdog anti-kill arms race, rootkit-style hiding).
- Postgres/multi-server scaling, SSO on the admin UI, alerting — revisit only when needed.
