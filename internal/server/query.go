package server

import (
	"database/sql"
	"fmt"

	"screwlogger/internal/protocol"
)

// Bucket is one key->seconds aggregate row for dwell usage.
type Bucket struct {
	Key     string `json:"key"`
	Seconds int64  `json:"seconds"`
}

// ActiveRatio splits dwell time into active vs idle seconds.
type ActiveRatio struct {
	ActiveSeconds int64   `json:"active_seconds"`
	IdleSeconds   int64   `json:"idle_seconds"`
	Ratio         float64 `json:"ratio"`
	TotalSeconds  int64   `json:"total_seconds"`
}

// EventRow is one raw heartbeat as exposed to the query API.
type EventRow struct {
	ID       string `json:"id"`
	TS       int64  `json:"ts"`
	App      string `json:"app"`
	Category string `json:"category"`
	Active   bool   `json:"active"`
	// Detail is the app-scoped foreground context (browser page / Photoshop
	// document), empty for apps outside the capture set. Omitted from JSON when
	// empty so existing consumers see no change for uncaptured apps.
	Detail string `json:"detail,omitempty"`
}

// FileBucket is per-file dwell time: how long a single document/page was in the
// foreground, split into active vs idle, with the efficiency ratio and the
// elapsed span (first → last seen). This is the "time per design file" row the
// creators' dashboard is built on.
type FileBucket struct {
	File          string  `json:"file"`
	Seconds       int64   `json:"seconds"`        // active + idle dwell
	ActiveSeconds int64   `json:"active_seconds"` // "hands-on" time
	IdleSeconds   int64   `json:"idle_seconds"`
	Ratio         float64 `json:"ratio"` // active / total — efficiency
	FirstSeen     int64   `json:"first_seen"`
	LastSeen      int64   `json:"last_seen"`
}

// dwellCTE is the shared window SQL for dwell queries. The single %s is the
// device filter clause (" AND device_id = :device" or ""); the <group> column
// is interpolated separately via the switch in DwellUsage.
const dwellCTE = `
WITH rng AS (
  SELECT device_id, ts, app, category, active FROM heartbeats
  WHERE ts >= :from AND ts < :to%s
),
paired AS (
  SELECT device_id, ts, app, category, active,
         LEAD(ts) OVER (PARTITION BY device_id ORDER BY ts) AS next_ts
  FROM rng
),
dwell AS (
  SELECT app, category, active,
         MIN(COALESCE(next_ts, :to), :to) - ts AS raw,
         CASE WHEN MIN(COALESCE(next_ts, :to), :to) - ts > :cap THEN :cap
              ELSE MIN(COALESCE(next_ts, :to), :to) - ts END AS secs
  FROM paired
)`

// deviceClause returns the rng WHERE fragment for a single-device query, or ""
// for a fleet-wide query (all devices, per-device gap capping).
func deviceClause(deviceID string) string {
	if deviceID == "" {
		return ""
	}
	return " AND device_id = :device"
}

// dwellArgs returns the named args shared by dwell queries, plus :device when
// a single device is requested.
func dwellArgs(deviceID string, from, to int64) []any {
	args := []any{
		sql.Named("from", from),
		sql.Named("to", to),
		sql.Named("cap", protocol.MaxHeartbeatGap),
	}
	if deviceID != "" {
		args = append(args, sql.Named("device", deviceID))
	}
	return args
}

// DwellUsage returns total dwell seconds per group, sorted by seconds DESC.
// groupBy is "category" or "app"; anything else is an error (the store maps the
// enum to a column name, never via user string concat).
func (s *Store) DwellUsage(deviceID string, from, to int64, groupBy string) ([]Bucket, error) {
	var group string
	switch groupBy {
	case "category":
		group = "category"
	case "app":
		group = "app"
	default:
		return nil, fmt.Errorf("unknown group_by %q", groupBy)
	}
	q := fmt.Sprintf(dwellCTE+`
SELECT %s AS key, SUM(secs) AS seconds FROM dwell GROUP BY %s ORDER BY seconds DESC`,
		deviceClause(deviceID), group, group)
	rows, err := s.db.Query(q, dwellArgs(deviceID, from, to)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Key, &b.Seconds); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ActiveRatio returns dwell seconds split by active vs idle over [from, to).
func (s *Store) ActiveRatio(deviceID string, from, to int64) (ActiveRatio, error) {
	q := fmt.Sprintf(dwellCTE+`
SELECT
  COALESCE(SUM(CASE WHEN active = 1 THEN secs END), 0) AS active_seconds,
  COALESCE(SUM(CASE WHEN active = 0 THEN secs END), 0) AS idle_seconds
FROM dwell`, deviceClause(deviceID))
	var ar ActiveRatio
	err := s.db.QueryRow(q, dwellArgs(deviceID, from, to)...).Scan(&ar.ActiveSeconds, &ar.IdleSeconds)
	if err != nil {
		return ActiveRatio{}, err
	}
	ar.TotalSeconds = ar.ActiveSeconds + ar.IdleSeconds
	if ar.TotalSeconds > 0 {
		ar.Ratio = float64(ar.ActiveSeconds) / float64(ar.TotalSeconds)
	}
	return ar, nil
}

// FileUsage returns per-file dwell time over [from, to), grouped by the
// heartbeat detail (document/page name), sorted by total seconds DESC. Only
// heartbeats carrying a non-empty detail are reported. app is an optional
// executable filter (e.g. "photoshop.exe" for design documents); "" includes
// every captured file. deviceID "" aggregates the fleet with per-device gap
// capping.
//
// Gap capping and window clipping mirror DwellUsage: heartbeats are paired over
// ALL apps per device (so a file's dwell ends exactly when the next foreground
// event begins, whatever app it is), then the app/detail filter is applied to
// the dwell rows. Each row's seconds are capped at protocol.MaxHeartbeatGap so a
// suspended or closed PC never fabricates time against a file.
func (s *Store) FileUsage(deviceID, app string, from, to int64) ([]FileBucket, error) {
	appClause := ""
	args := dwellArgs(deviceID, from, to)
	if app != "" {
		appClause = " AND app = :app"
		args = append(args, sql.Named("app", app))
	}
	q := fmt.Sprintf(`
WITH rng AS (
  SELECT device_id, ts, app, active, detail FROM heartbeats
  WHERE ts >= :from AND ts < :to%s
),
paired AS (
  SELECT device_id, ts, app, active, detail,
         LEAD(ts) OVER (PARTITION BY device_id ORDER BY ts) AS next_ts
  FROM rng
),
dwell AS (
  SELECT app, detail, active, ts,
         CASE WHEN MIN(COALESCE(next_ts, :to), :to) - ts > :cap THEN :cap
              ELSE MIN(COALESCE(next_ts, :to), :to) - ts END AS secs
  FROM paired
)
SELECT detail AS file,
       SUM(secs) AS seconds,
       COALESCE(SUM(CASE WHEN active = 1 THEN secs END), 0) AS active_seconds,
       COALESCE(SUM(CASE WHEN active = 0 THEN secs END), 0) AS idle_seconds,
       MIN(ts) AS first_seen,
       MAX(ts) AS last_seen
FROM dwell
WHERE detail <> ''%s
GROUP BY detail
ORDER BY seconds DESC`, deviceClause(deviceID), appClause)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileBucket
	for rows.Next() {
		var b FileBucket
		if err := rows.Scan(&b.File, &b.Seconds, &b.ActiveSeconds, &b.IdleSeconds, &b.FirstSeen, &b.LastSeen); err != nil {
			return nil, err
		}
		if b.Seconds > 0 {
			b.Ratio = float64(b.ActiveSeconds) / float64(b.Seconds)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListEvents returns raw heartbeats in [from, to), ordered by ts ASC. limit is
// clamped to [1, 10000]; values <= 0 default to 1000.
func (s *Store) ListEvents(deviceID string, from, to int64, limit int) ([]EventRow, error) {
	if limit <= 0 {
		limit = 1000
	} else if limit > 10000 {
		limit = 10000
	}
	q := `SELECT id, ts, app, category, active, detail FROM heartbeats WHERE ts >= ? AND ts < ?`
	args := []any{from, to}
	if deviceID != "" {
		q += ` AND device_id = ?`
		args = append(args, deviceID)
	}
	q += ` ORDER BY ts ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var active int
		if err := rows.Scan(&e.ID, &e.TS, &e.App, &e.Category, &active, &e.Detail); err != nil {
			return nil, err
		}
		e.Active = active != 0
		out = append(out, e)
	}
	return out, rows.Err()
}
