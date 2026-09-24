//go:build windows

package agent

import (
	"context"
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceHandler is the svc.Handler for monsvc. run is injectable so the
// Execute loop is testable without a real service control manager.
type serviceHandler struct {
	run func(ctx context.Context) error
}

// defaultRun builds the agent the same way console mode does, then runs it.
func defaultRun(cfg Config, fg ForegroundSource, idle IdleSource, now func() time.Time) func(context.Context) error {
	return func(ctx context.Context) error {
		a, err := New(cfg, fg, idle, now)
		if err != nil {
			return err
		}
		return a.Run(ctx)
	}
}

// RunService runs the agent as the named Windows service (blocking).
func RunService(name string, cfg Config, fg ForegroundSource, idle IdleSource, now func() time.Time) error {
	return svc.Run(name, &serviceHandler{run: defaultRun(cfg, fg, idle, now)})
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

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

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
