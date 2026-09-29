//go:build windows

package agent

import (
	"context"
	"log"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceHandler is the svc.Handler for monsvc. run is injectable so the
// Execute loop is testable without a real service control manager;
// sessionChange (nil-safe) fires on WTS session events so the spawner can
// reconcile immediately (spec §4.2).
type serviceHandler struct {
	run           func(ctx context.Context) error
	sessionChange func()
}

// composeServiceRun builds the service-mode agent: the pipe server publishes
// watcher samples, the spawner keeps a watcher alive, and the agent consumes
// samples (no samples → no heartbeats).
func composeServiceRun(cfg Config, now func() time.Time, pipe *PipeServer, sp *Spawner) func(context.Context) error {
	return func(ctx context.Context) error {
		fg, idle := NewWin32Sources() // required by New; unused in service mode
		a, err := New(cfg, fg, idle, now)
		if err != nil {
			return err
		}
		samples := make(chan Sample, 128)
		go pipe.Serve(ctx, samples)
		go sp.Run(ctx)
		return a.RunSamples(ctx, samples)
	}
}

// RunService runs the agent as the named Windows service (blocking). The
// service never polls Win32 itself — it runs the sample pipe server and the
// watcher spawner, and feeds pipe samples into the agent (spec §3).
func RunService(name string, cfg Config, now func() time.Time) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pipe := NewPipeServer(samplePipeName)
	sp := NewSpawner(exe, cfg.IdleThresholdSeconds, pipe)
	h := &serviceHandler{
		run:           composeServiceRun(cfg, now, pipe, sp),
		sessionChange: sp.Poke,
	}
	return svc.Run(name, h)
}

// Execute implements svc.Handler. It reports StartPending, then Running, and
// drives the agent loop until the SCM requests a stop/shutdown (graceful
// cancel + wait) or the agent exits on its own (reported as a nonzero
// svc-specific exit code so the SCM's restart-on-failure re-runs the service).
func (h *serviceHandler) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- h.run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange}

	for {
		select {
		case err := <-errCh:
			// The agent exited on its own: fatal error or an unexpected clean
			// return. Report a nonzero svc-specific exit code so the SCM's
			// restart-on-failure (Task 2's OnNonCrashFailures) re-runs it.
			if err != nil {
				log.Printf("agent stopped unexpectedly: %v", err)
			} else {
				log.Printf("agent exited unexpectedly")
			}
			return true, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.SessionChange:
				if h.sessionChange != nil {
					h.sessionChange()
				}
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-errCh: // Run returned nil after ctx cancel — clean stop
				case <-time.After(10 * time.Second): // Run hung on shutdown — give up waiting
					log.Printf("agent did not stop within 10s")
				}
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}
