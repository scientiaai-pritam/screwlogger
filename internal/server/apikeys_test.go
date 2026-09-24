package server_test

import (
	"errors"
	"strings"
	"testing"

	"screwlogger/internal/server"
)

func TestAPIKeyLifecycle(t *testing.T) {
	s := openTestStore(t)
	token, err := s.CreateAPIKey("build-bot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "ak_") {
		t.Fatalf("token must be ak_-prefixed, got %q", token)
	}
	scopes, err := s.ResolveAPIKey(token)
	if err != nil {
		t.Fatal(err)
	}
	if scopes != "read" {
		t.Fatalf("scopes want read, got %q", scopes)
	}
}

func TestResolveAPIKeyUnknown(t *testing.T) {
	s := openTestStore(t)
	_, err := s.ResolveAPIKey("ak_bogus")
	if !errors.Is(err, server.ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
}

func TestRevokeAPIKey(t *testing.T) {
	s := openTestStore(t)
	token, err := s.CreateAPIKey("build-bot")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(token); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResolveAPIKey(token)
	if !errors.Is(err, server.ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
}

func TestListAPIKeysNoPlaintext(t *testing.T) {
	s := openTestStore(t)
	token, err := s.CreateAPIKey("build-bot")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	row := rows[0]
	if row.Label != "build-bot" {
		t.Fatalf("label want build-bot, got %q", row.Label)
	}
	if len(row.HashPrefix) != 8 {
		t.Fatalf("hash prefix want 8 chars, got %q", row.HashPrefix)
	}
	if strings.Contains(row.Label, token) || strings.Contains(row.HashPrefix, token) {
		t.Fatalf("plaintext key leaked in %+v", row)
	}
}
