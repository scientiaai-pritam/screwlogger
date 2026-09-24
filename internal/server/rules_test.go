package server_test

import (
	"testing"
	"time"
)

func TestUpsertRuleIdempotentAndBumps(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertRule("mes_*.exe", "Production"); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first=%+v", first)
	}

	time.Sleep(1100 * time.Millisecond)
	if err := s.UpsertRule("mes_*.exe", "Production"); err != nil {
		t.Fatal(err)
	}
	second, err := s.ListRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("upsert twice must keep one row, got %+v", second)
	}
	if second[0].Category != "Production" {
		t.Fatalf("category=%q", second[0].Category)
	}
	if second[0].UpdatedAt <= first[0].UpdatedAt {
		t.Fatalf("updated_at must bump: %d -> %d", first[0].UpdatedAt, second[0].UpdatedAt)
	}
}

func TestDeleteRule(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertRule("mes_*.exe", "Production"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule("mes_*.exe"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestRecategorizeCaseInsensitive(t *testing.T) {
	s := openTestStore(t)
	devID, _, err := s.CreateDevice("PC-1")
	if err != nil {
		t.Fatal(err)
	}
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "MES_LINE3.EXE", "Manufacturing", true),
		hbeat("h2", 1010, "chrome.exe", "Browser", true),
	)
	n, err := s.Recategorize("mes_*.exe", "Production")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 changed, got %d", n)
	}
	evs, err := s.ListEvents(devID, 1000, 1100, 10)
	if err != nil {
		t.Fatal(err)
	}
	var mesCat, chromeCat string
	for _, e := range evs {
		switch e.App {
		case "MES_LINE3.EXE":
			mesCat = e.Category
		case "chrome.exe":
			chromeCat = e.Category
		}
	}
	if mesCat != "Production" {
		t.Fatalf("mes category want Production, got %q", mesCat)
	}
	if chromeCat != "Browser" {
		t.Fatalf("chrome category must be untouched, got %q", chromeCat)
	}
}
