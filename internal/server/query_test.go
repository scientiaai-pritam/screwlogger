package server_test

import (
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

func hbeat(id string, ts int64, app, category string, active bool) protocol.Heartbeat {
	return protocol.Heartbeat{ID: id, TS: ts, App: app, Category: category, Active: active}
}

func insertHBs(t *testing.T, s *server.Store, devID string, hbs ...protocol.Heartbeat) {
	t.Helper()
	if _, err := s.InsertHeartbeats(devID, 0, hbs); err != nil {
		t.Fatal(err)
	}
}

func TestDwellUsageCapsGap(t *testing.T) {
	s := openTestStore(t)
	devID, _, err := s.CreateDevice("PC-1")
	if err != nil {
		t.Fatal(err)
	}
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1100, "excel.exe", "Office", true), // 100 s gap
	)
	buckets, err := s.DwellUsage(devID, 1000, 1120, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets=%+v", buckets)
	}
	// h1 -> h2 gap is 100 s, capped at 30; h2 -> to is 20 s.
	if buckets[0].Key != "excel.exe" || buckets[0].Seconds != 50 {
		t.Fatalf("want excel.exe=50, got %+v", buckets[0])
	}
}

func TestDwellUsageClipsTrailingHeartbeat(t *testing.T) {
	s := openTestStore(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID, hbeat("h1", 1000, "excel.exe", "Office", true))
	buckets, err := s.DwellUsage(devID, 1000, 1005, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets=%+v", buckets)
	}
	// trailing heartbeat 5 s before `to` yields 5 s (window clip, not cap).
	if buckets[0].Seconds != 5 {
		t.Fatalf("want 5, got %d", buckets[0].Seconds)
	}
}

func TestDwellUsageGroupsByCategory(t *testing.T) {
	s := openTestStore(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1010, "word.exe", "Office", true),
	)
	buckets, err := s.DwellUsage(devID, 1000, 1020, "category")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets=%+v", buckets)
	}
	if buckets[0].Key != "Office" || buckets[0].Seconds != 20 {
		t.Fatalf("want Office=20, got %+v", buckets[0])
	}
}

func TestActiveRatioSplitsAndEmpty(t *testing.T) {
	s := openTestStore(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1010, "excel.exe", "Office", false),
	)
	ar, err := s.ActiveRatio(devID, 1000, 1020)
	if err != nil {
		t.Fatal(err)
	}
	if ar.ActiveSeconds != 10 || ar.IdleSeconds != 10 || ar.TotalSeconds != 20 {
		t.Fatalf("ar=%+v", ar)
	}
	if ar.Ratio != 0.5 {
		t.Fatalf("ratio want 0.5, got %v", ar.Ratio)
	}

	empty, err := s.ActiveRatio(devID, 5000, 6000)
	if err != nil {
		t.Fatal(err)
	}
	if empty.TotalSeconds != 0 || empty.Ratio != 0.0 {
		t.Fatalf("empty range: %+v", empty)
	}
}

func TestListEventsOrderAndLimit(t *testing.T) {
	s := openTestStore(t)
	devID, _, _ := s.CreateDevice("PC-1")
	// inserted out of order to prove ORDER BY ts ASC.
	insertHBs(t, s, devID,
		hbeat("h3", 1030, "excel.exe", "Office", false),
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1010, "excel.exe", "Office", true),
	)
	rows, err := s.ListEvents(devID, 1000, 1100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].TS != 1000 || rows[1].TS != 1010 {
		t.Fatalf("want ASC order, got %+v", rows)
	}
	if !rows[0].Active {
		t.Fatalf("h1 should be active, got %+v", rows[0])
	}

	all, err := s.ListEvents(devID, 1000, 1100, 0) // default limit
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("default limit: want 3, got %d", len(all))
	}
}

func TestDwellUsageFleetAcrossDevices(t *testing.T) {
	s := openTestStore(t)
	devA, _, _ := s.CreateDevice("PC-A")
	devB, _, _ := s.CreateDevice("PC-B")
	insertHBs(t, s, devA,
		hbeat("a1", 1000, "excel.exe", "Office", true),
		hbeat("a2", 1100, "excel.exe", "Office", true), // 100 s gap -> 30 capped + 10 clip
	)
	insertHBs(t, s, devB,
		hbeat("b1", 1000, "excel.exe", "Office", true),
		hbeat("b2", 1005, "excel.exe", "Office", true), // 5 s + 105 capped -> 5 + 30
	)
	// Per-device capping: devA = 30 + 10 = 40, devB = 5 + 30 = 35, total 75.
	// Without PARTITION BY device_id the interleaved LEAD would yield 45.
	buckets, err := s.DwellUsage("", 1000, 1110, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets=%+v", buckets)
	}
	if buckets[0].Seconds != 75 {
		t.Fatalf("want 75, got %d", buckets[0].Seconds)
	}
}
