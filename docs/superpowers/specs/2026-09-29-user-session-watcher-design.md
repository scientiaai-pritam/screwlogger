# User-Session Watcher — Design Spec

**Date:** 2026-09-29
**Status:** Approved design, pending implementation plan
**Type:** Amendment to the 2026-09-24 ScrewLogger design — replaces the in-service poller (§3.1 of the original spec) with a per-user watcher process. Server, ingest schema, rules, buffer, and shipper are unchanged.

## 1. Problem

Every service-mode deployment reports `app = unknown.exe` and `active = 0` on 100% of heartbeats.

Root cause: Windows Session 0 isolation. `monsvc` installs as a SYSTEM service (original design), and services always run in Session 0 — a kernel-enforced, non-interactive session with no desktop. Both Win32 sources the poller depends on are session-scoped:

- `GetForegroundWindow()` returns NULL in Session 0 → the agent maps every poll to `unknown.exe`.
- `GetLastInputInfo()` never advances in Session 0 (it receives no input) → idle grows forever → `active` is always false.

There is no API by which a Session 0 process can read another session's foreground window or input idle. Console mode (`-config agent.yaml`, dev path) works because it runs inside the logged-on user's session.

## 2. Constraints and decisions

| Decision | Choice | Rationale |
|---|---|---|
| Process model | Two processes: SYSTEM service + per-user watcher | Service keeps token + lossless buffer out of the user's reach; watcher holds nothing sensitive. Single-process-as-service is OS-impossible; single-process-as-user (scheduled task) exposes the ingest token to the monitored user (forgeable data) and loses SCM supervision |
| Where polling runs | Watcher, inside the user session | Only way to call session-scoped Win32 APIs with a real desktop |
| Where buffering/shipping runs | Service (unchanged) | Token stays in SYSTEM-readable `agent.yaml`; lossless buffer survives logoff/logon |
| Watcher contents | No token, no config file, no disk writes | A user who terminates and inspects the watcher finds nothing; killing it only triggers a silent respawn |
| No logon behavior | Ship nothing | No user session → no usage; device shows offline (user decision 2026-09-29) |
| Visibility | `CREATE_NO_WINDOW`, no tray, plain `monsvc.exe` process | Matches original stealth decision: unobtrusive, not forensically undetectable |
| Resource budget | Watcher ≈ old poller: 1 Hz × 2 Win32 calls + 1 pipe write/sec; ~10–15 MB RAM | Preserves success criterion "<1% sustained CPU, ~10 MB" per process; fleet total ≈ 25 MB/PC |

## 3. Architecture

```
┌──────────────────── Factory PC ────────────────────────────────┐
│ monsvc (SYSTEM service, Session 0)                             │
│   Spawner ──── CreateProcessAsUser (console session's user) ┐  │
│   Pipe server \\.\pipe\monsvc-samples (inbound)             │  │
│   Heartbeat builder ─► categorizer ─► Buffer ─► Shipper     │  │
│        ▲ (unchanged)                                (unchanged)
│        │ samples {"app","active"} @1 Hz                     ▼  │
│ ┌──────┴──────────────────────────────────────────────────┐    │
│ │ monsvc.exe -userwatch (user session, no window)         │    │
│ │   1 Hz: GetForegroundWindow + GetLastInputInfo          │    │
│ │   self-pauses when its session isn't the active console │    │
│ │   writes JSON lines to the pipe; exits if pipe is gone  │    │
│ └─────────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────┘
```

Console mode (`-config agent.yaml`) is unchanged: it keeps the ticker + direct Win32 sources for development.

## 4. Components

### 4.1 Watcher (`monsvc.exe -userwatch -idle <seconds>`)

- Loop, once per second:
  1. If `WTSGetActiveConsoleSessionId()` != its own session ID → skip the tick (self-pause; no sample for a background/disconnected session).
  2. Sample `GetForegroundWindow` → app base name (existing `ForegroundApp` logic, unchanged semantics incl. `unknown.exe` on lock screen) and `GetLastInputInfo` → `active = idle < idleThreshold`. The threshold comes from the `-idle` flag (the service passes its configured value; the watcher cannot read `agent.yaml`, which is SYSTEM/admin-only).
  3. Write one JSON line to the pipe: `{"app":"chrome.exe","active":true}`.
- Connection: retries connecting every 500 ms for up to 15 s (covers service start ordering); on write failure or dropped pipe it re-enters the retry loop and exits if the pipe does not return within 15 s. The service respawns it.
- The watcher holds no state: no buffer, no dedup, no disk access.

### 4.2 Spawner (in service)

- Triggers: service start; `SERVICE_CONTROL_SESSIONCHANGE` events — logon *and* console-connect (fast user switching produces the latter, not the former); a 30 s reconcile tick.
- For the active console session: `WTSGetActiveConsoleSessionId()` (treat `0xFFFFFFFF` as "no console user" → do nothing), `WTSQueryUserToken` (SYSTEM has the required TCB privilege), `CreateEnvironmentBlock`, then `CreateProcessAsUser` launching `<exe dir>\monsvc.exe -userwatch -idle N` with `CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT`.
- Reconciliation is driven by pipe-connection state, not PID tracking: while no watcher is connected and a console session exists, respawn at backoff 1 s → 5 s → 30 s (cap); a successful pipe connection resets the ladder. A stale duplicate watcher is harmless (the pipe server accepts newest, and the stale one exits on write failure).

### 4.3 Pipe server (in service)

- Named pipe `\\.\pipe\monsvc-samples`, inbound (service reads), byte mode.
- Explicit DACL: SYSTEM and Administrators full, the session user's SID write; everyone else excluded (deny-all root). The service builds the SDDL from the token it used to spawn.
- One active client; on a new connection any previous one is dropped (newest wins).
- Each line is decoded and handed to the agent as a sample. Malformed lines are logged and skipped. No hello/handshake — both sides ship in one binary and upgrade together.

### 4.4 Agent refactor

- Extract `Observe(app string, active bool)` — builder.Observe → UUID → categorize → buffer append (existing mutex discipline unchanged).
- `pollOnce` becomes a thin wrapper calling `Observe` (console mode keeps ticker + Win32 sources).
- Service mode runs event-driven: pipe samples call `Observe` directly; no samples → no heartbeats. The ship loop is untouched.

## 5. Behavior matrix

| State | Result |
|---|---|
| Boot, no logon | No console session → no watcher → no heartbeats (device offline) |
| Logon | Spawner launches watcher → samples flow → heartbeats as before |
| Lock screen | Session active, no foreground window → `unknown.exe`, `active=false` (original intent) |
| Fast user switching | Background session's watcher self-pauses → no samples from invisible sessions |
| Logoff | Watcher process dies → pipe closes → no heartbeats |
| User kills watcher | Service respawns silently within the backoff ladder |
| Service restart | Old watcher exits (broken pipe); new service spawns a fresh watcher on start |

## 6. Error handling

- Pipe absent at watcher start → connect-retry loop (§4.1); watcher exits after 15 s rather than spinning.
- Watcher repeatedly killed → 30 s-capped respawn ladder; bounded work per attempt, no thrash.
- Malformed pipe line → log + skip; server ingest stays untouched (local IPC only).
- Spawning fails (e.g. token API error) → logged, retried on the next ladder tick.

## 7. Testing

- Unit: sample JSON codec; `Observe` extraction preserves existing builder/categorize/buffer behavior (existing tests move to call it); watcher pause logic with an injectable console-session source; spawner state machine with injectable session-id/token/spawn fakes (mirrors existing `service_windows` test seams); pipe server end-to-end in-process (real named pipe on Windows, sample → buffer).
- Integration on the dev PC (Win11): install → dashboard shows real app names; lock → `unknown.exe`/inactive; Task-Manager kill → respawn ≤ 30 s; logoff → device goes quiet.
- Final verification on the affected Win10 PC after rollout.

## 8. Rollout

Build the new binary and run the existing upgrade path per PC: `monsvc.exe -upgrade` (stops service, replaces exe, restarts; `agent.yaml` untouched). On restart the service spawns a watcher for the current session immediately — no logoff/logon needed. Applies to the ~17 enrolled devices.

## 9. Non-goals

- RDP / disconnected-session sampling (console session only in v1; future work could enumerate sessions via `WTSEnumerateSessions`).
- Bridging samples across a service restart (a few seconds of samples are lost while the pipe is down; heartbeats resume on reconnect).
- Repairing historical `unknown.exe` heartbeats (recorded as seen; dashboards are meaningful from deployment forward).
- Forensic-grade stealth (per the original spec: documented in IT policy, unobtrusive — not undetectable).
