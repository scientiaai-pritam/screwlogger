package agent_test

import (
	"path/filepath"
	"testing"

	"screwlogger/internal/agent"
	"screwlogger/internal/protocol"
)

func hb(i int) protocol.Heartbeat {
	return protocol.Heartbeat{ID: string(rune('a' + i)), TS: int64(1000 + i), App: "a.exe", Category: "C", Active: true}
}

func TestAppendAndUnacked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, err := agent.OpenBuffer(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i := 0; i < 3; i++ {
		if err := b.Append(hb(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Unacked(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "a" || got[2].ID != "c" {
		t.Fatalf("got %+v", got)
	}
}

func TestAckPrunesAndReopenKeepsUnacked(t *testing.T) {
	// Review Focus #2: crash between append and upload loses nothing.
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, _ := agent.OpenBuffer(p, 1<<20)
	for i := 0; i < 4; i++ {
		b.Append(hb(i))
	}
	if err := b.Ack(2); err != nil {
		t.Fatal(err)
	}
	b.Close()

	b2, err := agent.OpenBuffer(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	got, err := b2.Unacked(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "d" {
		t.Fatalf("after ack(2) reopen, want [c d], got %+v", got)
	}
}

func TestAckCompactsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, _ := agent.OpenBuffer(p, 1<<20)
	for i := 0; i < 4; i++ {
		b.Append(hb(i))
	}
	b.Ack(4) // everything acked → compaction must shrink file to empty
	b.Close()
	b2, _ := agent.OpenBuffer(p, 1<<20)
	defer b2.Close()
	got, _ := b2.Unacked(10)
	if len(got) != 0 {
		t.Fatalf("fully acked buffer must be empty after reopen, got %+v", got)
	}
}

func TestRotationDropsOldestWhenFull(t *testing.T) {
	// maxBytes small: one entry ~100 bytes; cap at 200 → drop oldest half on overflow.
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, _ := agent.OpenBuffer(p, 200)
	defer b.Close()
	for i := 0; i < 6; i++ {
		b.Append(hb(i))
	}
	got, _ := b.Unacked(100)
	if len(got) >= 6 {
		t.Fatalf("overflow must drop oldest entries, still have %d", len(got))
	}
	if got[len(got)-1].ID != string(rune('a'+5)) {
		t.Fatalf("newest entry must survive rotation, got %+v", got)
	}
}
