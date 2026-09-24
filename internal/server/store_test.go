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
