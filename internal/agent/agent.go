package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Agent ties poller → heartbeat builder → categorizer → buffer → shipper.
type Agent struct {
	cfg     Config
	fg      ForegroundSource
	idle    IdleSource
	title   TitleSource // optional; nil when fg exposes no window title
	now     func() time.Time
	builder *HeartbeatBuilder
	rules   Rules
	buf     *Buffer
	ship    *Shipper

	// mu serializes ALL Buffer access. Buffer is not safe for concurrent use
	// (no internal lock): it shares one file descriptor and mutable offsets
	// (ackedOff/size, Seek-based scans, compaction rewrites that close and
	// reopen the fd). Run polls Append on the main goroutine while shipLoop
	// calls Flush → Unacked/Ack (and possibly compact/rewrite) on another, so
	// every buf touch — Append, Flush, Close — happens under this mutex. The
	// lock is held across Flush's HTTP round trip: that blocks polling for the
	// request duration, which is the price of keeping Unacked→Ack atomic
	// w.r.t. appends (an Append during a compaction rewrite would target a
	// closed fd). Flushes are at most once per FlushInterval, so pauses are
	// rare and bounded by the HTTP client timeout.
	mu sync.Mutex
}

// New constructs the agent. LoadRules failures are fatal for missing file?
// No: a missing rules file is fine — everything is Uncategorized (spec §3.2).
func New(cfg Config, fg ForegroundSource, idle IdleSource, now func() time.Time) (*Agent, error) {
	if now == nil {
		now = time.Now // Task 10's cmd entry calls New with a nil clock
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	buf, err := OpenBuffer(filepath.Join(cfg.DataDir, "buffer.jsonl"), cfg.MaxBufferBytes)
	if err != nil {
		return nil, err
	}
	rules, err := LoadRules(filepath.Join(cfg.DataDir, "rules.yaml"))
	if err != nil {
		log.Printf("rules: %v (starting with no rules — everything Uncategorized)", err)
		rules = nil
	}
	title, _ := fg.(TitleSource) // nil for sources without a title (tests, non-Win32)
	return &Agent{
		cfg: cfg, fg: fg, idle: idle, title: title, now: now,
		builder: NewHeartbeatBuilder(now, DefaultKeepAlive),
		rules:   rules, buf: buf,
		ship: NewShipper(cfg.ServerURL, cfg.Token, buf, nil),
	}, nil
}

// Run polls until ctx is cancelled, shipping in a separate goroutine.
func (a *Agent) Run(ctx context.Context) error {
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.buf.Close()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.shipLoop(ctx)
	}()

	tick := time.NewTicker(a.cfg.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			<-done // let shipper finish its current attempt
			return nil
		case <-tick.C:
			a.pollOnce()
		}
	}
}

func (a *Agent) pollOnce() {
	app := a.fg.ForegroundApp()
	active := a.idle.IdleSeconds() < float64(a.cfg.IdleThresholdSeconds)
	a.Observe(app, active, a.captureDetail(app))
}

// captureDetail reads the foreground window title and extracts the app-scoped
// context string (browser page / Photoshop document) — but only when capture is
// enabled, the source exposes a title, and app is in the capture set. Otherwise
// it returns "", so no other app's window title is ever read or shipped.
func (a *Agent) captureDetail(app string) string {
	if a.title == nil || !a.cfg.ShouldCaptureTitles() || !ShouldCaptureTitle(app) {
		return ""
	}
	return ExtractDetail(app, a.title.ForegroundTitle())
}

// Observe feeds one foreground/active reading through the heartbeat builder,
// categorizer, and buffer. Console mode calls it per tick; service mode calls
// it per pipe sample. detail is the (possibly empty) foreground-context string.
func (a *Agent) Observe(app string, active bool, detail string) {
	hb := a.builder.ObserveDetail(app, active, detail)
	if hb == nil {
		return
	}
	hb.ID = uuid.NewString()
	hb.Category = a.rules.Categorize(app)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.buf.Append(*hb); err != nil {
		log.Printf("buffer append: %v", err)
	}
}

// RunSamples consumes watcher samples until ctx is cancelled, shipping in a
// separate goroutine (service mode). Samples arrive over the local pipe; no
// samples — watcher dead, no user logged on — means no heartbeats, so the
// device reports offline (spec §5).
func (a *Agent) RunSamples(ctx context.Context, samples <-chan Sample) error {
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.buf.Close()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.shipLoop(ctx)
	}()
	for {
		select {
		case <-ctx.Done():
			<-done // let the shipper finish its current attempt
			return nil
		case s := <-samples:
			a.Observe(s.App, s.Active, s.Detail)
		}
	}
}

func (a *Agent) shipLoop(ctx context.Context) {
	timer := time.NewTimer(a.cfg.FlushInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.mu.Lock()
			_, err := a.ship.Flush(ctx)
			a.mu.Unlock()
			wait := a.ship.NextInterval(err)
			if err != nil {
				log.Printf("flush: %v (next attempt in %v)", err, wait)
			}
			timer.Reset(wait)
		}
	}
}
