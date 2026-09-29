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

func TestRenameDevice(t *testing.T) {
	s := openTestStore(t)
	id, token, err := s.CreateDevice("PC-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameDevice(id, "Komal"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "Komal" {
		t.Fatalf("rename did not persist: %+v", rows)
	}
	// Rename is display-only: the token must keep resolving to the same device.
	got, err := s.DeviceIDByToken(token)
	if err != nil || got != id {
		t.Fatalf("token broken after rename: id=%s err=%v", got, err)
	}
}

func TestRenameDeviceUnknownID(t *testing.T) {
	s := openTestStore(t)
	if err := s.RenameDevice("deadbeef", "X"); !errors.Is(err, server.ErrUnknownDevice) {
		t.Fatalf("want ErrUnknownDevice, got %v", err)
	}
}

func TestDeleteDeviceRemovesRevokedAndHistory(t *testing.T) {
	s := openTestStore(t)
	id, _, err := s.CreateDevice("PC-OLD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertHeartbeats(id, 1729800001, []protocol.Heartbeat{
		hbWithID("h1", 1729800000), hbWithID("h2", 1729800015),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(id); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListDevices()
	if err != nil || len(rows) != 0 {
		t.Fatalf("device must be gone: rows=%+v err=%v", rows, err)
	}
	evs, err := s.ListEvents(id, 0, 1729900000, 10)
	if err != nil || len(evs) != 0 {
		t.Fatalf("heartbeats must be gone: evs=%+v err=%v", evs, err)
	}
}

func TestDeleteDeviceRefusesActive(t *testing.T) {
	s := openTestStore(t)
	id, _, err := s.CreateDevice("PC-LIVE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertHeartbeats(id, 1729800001, []protocol.Heartbeat{hbWithID("h1", 1729800000)}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(id); !errors.Is(err, server.ErrNotRevoked) {
		t.Fatalf("want ErrNotRevoked, got %v", err)
	}
	rows, err := s.ListDevices()
	if err != nil || len(rows) != 1 {
		t.Fatalf("active device must survive: rows=%+v err=%v", rows, err)
	}
	evs, err := s.ListEvents(id, 0, 1729900000, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("active device's history must survive: evs=%+v err=%v", evs, err)
	}
}

func TestDeleteDeviceUnknownID(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteDevice("deadbeef"); !errors.Is(err, server.ErrUnknownDevice) {
		t.Fatalf("want ErrUnknownDevice, got %v", err)
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
