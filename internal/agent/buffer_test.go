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
	got, err := b.Unacked(100)
	if err != nil {
		t.Fatalf("buffer must parse cleanly after rotation: %v", err)
	}
	if len(got) >= 6 {
		t.Fatalf("overflow must drop oldest entries, still have %d", len(got))
	}
	if got[len(got)-1].ID != string(rune('a'+5)) {
		t.Fatalf("newest entry must survive rotation, got %+v", got)
	}
	// After rotation the file was rewritten via temp+rename and the fd reopened
	// with O_APPEND; a further append must land at the new EOF and still parse.
	if err := b.Append(hb(6)); err != nil {
		t.Fatal(err)
	}
	got2, err := b.Unacked(100)
	if err != nil {
		t.Fatalf("buffer must parse cleanly after post-rotation append: %v", err)
	}
	if got2[len(got2)-1].ID != string(rune('a'+6)) {
		t.Fatalf("post-rotation append must survive, got %+v", got2)
	}
}

func TestAppendAfterPartialScanDoesNotClobber(t *testing.T) {
	// Regression: bufio.Scanner read-ahead leaves the fd offset mid-file once
	// >64KB is buffered. Without O_APPEND, the next Append wrote at that offset
	// and destroyed buffered heartbeats ("corrupt buffer line" on next read).
	p := filepath.Join(t.TempDir(), "buf.jsonl")
	b, err := agent.OpenBuffer(p, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i := 0; i < 1200; i++ {
		if err := b.Append(hb(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Unacked(500); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(hb(1200)); err != nil {
		t.Fatal(err)
	}
	got, err := b.Unacked(2000)
	if err != nil {
		t.Fatalf("buffer corrupted after append following partial scan: %v", err)
	}
	if len(got) != 1201 {
		t.Fatalf("want 1201 unacked entries, got %d", len(got))
	}
	if got[0].ID != "a" || got[1200].ID != string(rune('a'+1200)) {
		t.Fatalf("order corrupted: first=%q last=%q", got[0].ID, got[1200].ID)
	}
}
