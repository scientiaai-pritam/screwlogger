package agent_test

import (
	"testing"

	"screwlogger/internal/agent"
)

func TestIdleFromTickCount(t *testing.T) {
	// Ticks are GetTickCount milliseconds; expected values are exact.
	// (Brief's original wants assumed 1 tick = 1s and were arithmetically
	// unsatisfiable — see task report.)
	cases := []struct {
		now, last uint32
		want      float64
	}{
		{now: 1_000_000, last: 999_000, want: 1},     // 1000ms idle = 1s
		{now: 1_000, last: 999, want: 0.001},         // 1ms idle
		{now: 500, last: 4_294_967_000, want: 0.796}, // uint32 wraparound: 296+500ms
		{now: 100, last: 100, want: 0},
	}
	for _, c := range cases {
		got := agent.IdleFromTickCount(c.now, c.last)
		if diff := got - c.want; diff > 0.01 || diff < -0.01 {
			t.Fatalf("IdleFromTickCount(%d,%d) = %v, want %v", c.now, c.last, got, c.want)
		}
	}
}
