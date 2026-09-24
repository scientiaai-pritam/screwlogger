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
	return &Agent{
		cfg: cfg, fg: fg, idle: idle, now: now,
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
	hb := a.builder.Observe(app, active)
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
