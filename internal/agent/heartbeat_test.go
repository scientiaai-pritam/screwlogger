package agent_test

import (
	"testing"
	"time"

	"screwlogger/internal/agent"
)

func newBuilder(start time.Time) (*agent.HeartbeatBuilder, *time.Time) {
	now := start
	fn := func() time.Time { return now }
	return agent.NewHeartbeatBuilder(fn, agent.DefaultKeepAlive), &now
}

func TestFirstObservationEmits(t *testing.T) {
	b, _ := newBuilder(time.Unix(1000, 0))
	hb := b.Observe("excel.exe", true)
	if hb == nil || hb.App != "excel.exe" || !hb.Active || hb.TS != 1000 {
		t.Fatalf("first observe must emit: %+v", hb)
	}
}

func TestUnchangedStateEmitsOnlyOnKeepAlive(t *testing.T) {
	b, nowP := newBuilder(time.Unix(1000, 0))
	b.Observe("excel.exe", true)
	*nowP = time.Unix(1014, 0) // 14s later: inside keep-alive
	if hb := b.Observe("excel.exe", true); hb != nil {
		t.Fatalf("no emission expected before keep-alive: %+v", hb)
	}
	*nowP = time.Unix(1015, 0) // exactly keep-alive: must emit
	if hb := b.Observe("excel.exe", true); hb == nil {
		t.Fatal("keep-alive emission expected")
	}
}

func TestAppChangeEmitsImmediately(t *testing.T) {
	b, nowP := newBuilder(time.Unix(1000, 0))
	b.Observe("excel.exe", true)
	*nowP = time.Unix(1001, 0)
	if hb := b.Observe("chrome.exe", true); hb == nil || hb.App != "chrome.exe" {
		t.Fatalf("app change must emit immediately: %+v", hb)
	}
}

func TestActiveFlipEmitsImmediately(t *testing.T) {
	b, nowP := newBuilder(time.Unix(1000, 0))
	b.Observe("excel.exe", true)
	*nowP = time.Unix(1001, 0)
	if hb := b.Observe("excel.exe", false); hb == nil || hb.Active {
		t.Fatalf("active flip must emit immediately: %+v", hb)
	}
}
