//go:build windows

package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Watcher connect-retry cadence (spec §4.1): 500ms attempts, 15s budget.
const (
	watcherDialInterval = 500 * time.Millisecond
	watcherDialBudget   = 15 * time.Second
)

// WatcherDeps carries the watcher's collaborators. Nil funcs are replaced
// with production implementations by RunWatcher; tests inject fakes.
// DialInterval/DialBudget zero values mean the defaults above.
type WatcherDeps struct {
	FG             ForegroundSource
	Idle           IdleSource
	OwnSession     func() (uint32, error) // this process's session id
	ConsoleSession func() uint32          // active console session id
	Dial           func() (io.WriteCloser, error)
	DialInterval   time.Duration
	DialBudget     time.Duration
}

// RunWatcher polls the interactive session at 1 Hz and writes samples to the
// service pipe until ctx is cancelled. It self-pauses when its session is not
// the active console one (no samples for invisible sessions — spec §4.1) and
// exits when the pipe stays unreachable past the dial budget; the service's
// spawner then brings up a fresh watcher.
func RunWatcher(ctx context.Context, idleThresholdSeconds int, deps WatcherDeps) error {
	if deps.FG == nil || deps.Idle == nil {
		return fmt.Errorf("watcher: FG and Idle sources are required")
	}
	if deps.OwnSession == nil {
		deps.OwnSession = func() (uint32, error) {
			var sid uint32
			err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid)
			return sid, err
		}
	}
	if deps.ConsoleSession == nil {
		deps.ConsoleSession = windows.WTSGetActiveConsoleSessionId
	}
	if deps.Dial == nil {
		deps.Dial = dialSamplePipe
	}
	interval, budget := deps.DialInterval, deps.DialBudget
	if interval == 0 {
		interval = watcherDialInterval
	}
	if budget == 0 {
		budget = watcherDialBudget
	}

	conn, err := dialRetry(ctx, deps.Dial, interval, budget)
	if err != nil {
		if ctx.Err() != nil {
			return nil // cancelled while waiting for the pipe: clean exit
		}
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
			return nil
		case <-tick.C:
			if conn == nil {
				c, err := dialRetry(ctx, deps.Dial, interval, budget)
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				conn = c
			}
			own, err := deps.OwnSession()
			if err != nil || own != deps.ConsoleSession() {
				continue // paused: not the active console session
			}
			s := Sample{
				App:    deps.FG.ForegroundApp(),
				Active: deps.Idle.IdleSeconds() < float64(idleThresholdSeconds),
			}
			if _, err := conn.Write(EncodeSample(s)); err != nil {
				log.Printf("watcher: pipe write: %v", err)
				conn.Close()
				conn = nil
			}
		}
	}
}

// dialRetry attempts dial every interval for up to budget (bounded — Review
// Focus #1), returning the first success.
func dialRetry(ctx context.Context, dial func() (io.WriteCloser, error), interval, budget time.Duration) (io.WriteCloser, error) {
	deadline := time.Now().Add(budget)
	for {
		c, err := dial()
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("watcher: pipe unreachable for %v: %w", budget, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// dialSamplePipe opens a write handle to the service's sample pipe.
func dialSamplePipe() (io.WriteCloser, error) {
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(samplePipeName),
		windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), samplePipeName), nil
}
