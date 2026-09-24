//go:build windows

package agent

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/windows/svc"
)

// execResult wraps the Execute return tuple so it can be read from a channel
// without a goroutine writing two values into the same select.
type execResult struct {
	svcSpecific bool
	exitCode    uint32
}

func TestExecuteGracefulStop(t *testing.T) {
	h := &serviceHandler{run: func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8) // buffered so handler sends never block
	resCh := make(chan execResult, 1)
	go func() {
		specific, code := h.Execute(nil, reqCh, statusCh)
		resCh <- execResult{specific, code}
	}()

	// StartPending, then Running accepting Stop|Shutdown.
	if s := <-statusCh; s.State != svc.StartPending {
		t.Fatalf("first status = %v, want StartPending", s.State)
	}
	if s := <-statusCh; s.State != svc.Running || s.Accepts&(svc.AcceptStop|svc.AcceptShutdown) != svc.AcceptStop|svc.AcceptShutdown {
		t.Fatalf("second status = %+v, want Running accepting Stop|Shutdown", s)
	}

	// Interrogate echoes the current status back (Running).
	reqCh <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	if s := <-statusCh; s.State != svc.Running {
		t.Fatalf("interrogate echo = %v, want Running", s.State)
	}

	// Stop → StopPending, then Stopped, then (false, 0).
	reqCh <- svc.ChangeRequest{Cmd: svc.Stop}
	if s := <-statusCh; s.State != svc.StopPending {
		t.Fatalf("stop status = %v, want StopPending", s.State)
	}
	if s := <-statusCh; s.State != svc.Stopped {
		t.Fatalf("final status = %v, want Stopped", s.State)
	}

	res := <-resCh
	if res.svcSpecific || res.exitCode != 0 {
		t.Fatalf("result = %+v, want svcSpecific=false exitCode=0", res)
	}
}

// driveExecute starts Execute with the given run and returns its exit tuple.
func driveExecute(t *testing.T, run func(context.Context) error) execResult {
	t.Helper()
	h := &serviceHandler{run: run}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8) // buffered so handler sends never block
	resCh := make(chan execResult, 1)
	go func() {
		specific, code := h.Execute(nil, reqCh, statusCh)
		resCh <- execResult{specific, code}
	}()
	return <-resCh
}

func TestExecuteFatalExit(t *testing.T) {
	res := driveExecute(t, func(ctx context.Context) error {
		return errors.New("agent died")
	})
	if !res.svcSpecific || res.exitCode != 1 {
		t.Fatalf("result = %+v, want svcSpecific=true exitCode=1", res)
	}
}

func TestExecuteCleanExitStillNonzero(t *testing.T) {
	res := driveExecute(t, func(ctx context.Context) error {
		return nil
	})
	if !res.svcSpecific || res.exitCode != 1 {
		t.Fatalf("result = %+v, want svcSpecific=true exitCode=1", res)
	}
}
