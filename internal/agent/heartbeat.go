package agent

import (
	"time"

	"screwlogger/internal/protocol"
)

// DefaultKeepAlive is how often an unchanged state re-emits (spec §3.1).
const DefaultKeepAlive = 15 * time.Second

// HeartbeatBuilder converts 1s poll observations into heartbeats: it emits
// on app change, on active/idle flip, and as a keep-alive on unchanged state.
type HeartbeatBuilder struct {
	now       func() time.Time
	keepAlive time.Duration
	started   bool
	curApp    string
	curActive bool
	lastEmit  time.Time
}

func NewHeartbeatBuilder(now func() time.Time, keepAlive time.Duration) *HeartbeatBuilder {
	return &HeartbeatBuilder{now: now, keepAlive: keepAlive}
}

// Observe feeds one poll result and returns a heartbeat if one is due, else nil.
func (b *HeartbeatBuilder) Observe(app string, active bool) *protocol.Heartbeat {
	now := b.now()
	due := !b.started ||
		app != b.curApp ||
		active != b.curActive ||
		now.Sub(b.lastEmit) >= b.keepAlive
	if !due {
		return nil
	}
	b.started, b.curApp, b.curActive, b.lastEmit = true, app, active, now
	return &protocol.Heartbeat{TS: now.Unix(), App: app, Active: active}
}
