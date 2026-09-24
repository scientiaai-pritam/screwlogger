package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	_ "modernc.org/sqlite"

	"screwlogger/internal/protocol"
)

// ErrRevoked is returned when a valid token belongs to a revoked device.
var ErrRevoked = errors.New("device token revoked")

// ErrUnknownToken is returned when a token matches no enrolled device.
var ErrUnknownToken = errors.New("unknown device token")

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
		if n, err := res.RowsAffected(); err == nil && n > 0 {
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
		return "", ErrUnknownToken
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

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
