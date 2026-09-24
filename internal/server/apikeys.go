package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// ErrUnknownKey is returned when an API key matches no stored key.
var ErrUnknownKey = errors.New("unknown api key")

// APIKeyRow is one API key as exposed to the admin UI. HashPrefix is the first
// 8 hex chars of the sha256 hash, display only — never the full hash or the
// plaintext key.
type APIKeyRow struct {
	Label      string
	CreatedAt  int64
	RevokedAt  sql.NullInt64
	HashPrefix string
}

// CreateAPIKey stores a sha256-hashed read-only key and returns the one-time
// plaintext "ak_" key.
func (s *Store) CreateAPIKey(label string) (string, error) {
	tokBytes := make([]byte, 32)
	if _, err := rand.Read(tokBytes); err != nil {
		return "", err
	}
	token := "ak_" + base64.RawURLEncoding.EncodeToString(tokBytes)
	_, err := s.db.Exec(`INSERT INTO api_keys (key_hash, label, scopes, created_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), label, "read", time.Now().Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// ResolveAPIKey returns the scopes for a plaintext key, or ErrUnknownKey /
// ErrRevoked.
func (s *Store) ResolveAPIKey(token string) (string, error) {
	var scopes string
	var revoked sql.NullInt64
	err := s.db.QueryRow(`SELECT scopes, revoked_at FROM api_keys WHERE key_hash = ?`, hashToken(token)).
		Scan(&scopes, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownKey
	}
	if err != nil {
		return "", err
	}
	if revoked.Valid {
		return "", ErrRevoked
	}
	return scopes, nil
}

// RevokeAPIKey marks an API key revoked by hash; its key stops working.
func (s *Store) RevokeAPIKey(token string) error {
	_, err := s.db.Exec(`UPDATE api_keys SET revoked_at = ? WHERE key_hash = ?`,
		time.Now().Unix(), hashToken(token))
	return err
}

// ListAPIKeys returns all API keys, newest first, without any plaintext.
func (s *Store) ListAPIKeys() ([]APIKeyRow, error) {
	rows, err := s.db.Query(`SELECT label, created_at, revoked_at, key_hash FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyRow
	for rows.Next() {
		var r APIKeyRow
		var hash string
		if err := rows.Scan(&r.Label, &r.CreatedAt, &r.RevokedAt, &hash); err != nil {
			return nil, err
		}
		r.HashPrefix = hash
		if len(r.HashPrefix) > 8 {
			r.HashPrefix = r.HashPrefix[:8]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
