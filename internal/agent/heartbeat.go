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
	curDetail string
	lastEmit  time.Time
}

func NewHeartbeatBuilder(now func() time.Time, keepAlive time.Duration) *HeartbeatBuilder {
	return &HeartbeatBuilder{now: now, keepAlive: keepAlive}
}

// Observe feeds one poll result and returns a heartbeat if one is due, else nil.
// It is the detail-free form, kept for callers that capture no title.
func (b *HeartbeatBuilder) Observe(app string, active bool) *protocol.Heartbeat {
	return b.ObserveDetail(app, active, "")
}

// ObserveDetail is Observe with a foreground-context string (browser page /
// Photoshop document). A change in detail emits a heartbeat just as an app or
// active/idle change does, so switching tabs or documents is captured without
// waiting for the keep-alive.
func (b *HeartbeatBuilder) ObserveDetail(app string, active bool, detail string) *protocol.Heartbeat {
	now := b.now()
	due := !b.started ||
		app != b.curApp ||
		active != b.curActive ||
		detail != b.curDetail ||
		now.Sub(b.lastEmit) >= b.keepAlive
	if !due {
		return nil
	}
	b.started, b.curApp, b.curActive, b.curDetail, b.lastEmit = true, app, active, detail, now
	return &protocol.Heartbeat{TS: now.Unix(), App: app, Active: active, Detail: detail}
}
