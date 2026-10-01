//go:build windows

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// TestExecuteLogsLifecycle pins the service's own trail: the event log must
// show that the service came up and that a stop was requested, or a silent PC
// cannot be told apart from a never-started one.
func TestExecuteLogsLifecycle(t *testing.T) {
	buf := captureLogs(t)
	h := &serviceHandler{run: func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8)
	done := make(chan struct{})
	go func() { h.Execute(nil, reqCh, statusCh); close(done) }()
	<-statusCh // StartPending
	<-statusCh // Running

	reqCh <- svc.ChangeRequest{Cmd: svc.Stop}
	<-done

	out := buf.String()
	if !strings.Contains(out, "service running") {
		t.Fatalf("log missing \"service running\":\n%s", out)
	}
	if !strings.Contains(out, "stop requested") {
		t.Fatalf("log missing \"stop requested\":\n%s", out)
	}
}

// RunService narrates its startup before handing control to the SCM — on a
// normal box svc.Run fails fast with the service-controller-connect error,
// which also makes this testable without a real service.
func TestRunServiceLogsStart(t *testing.T) {
	buf := captureLogs(t)

	cfg := Config{ServerURL: "http://127.0.0.1:8998", IdleThresholdSeconds: 180}
	if err := RunService("monsvc-selftest-not-a-service", cfg, time.Now); err == nil {
		t.Fatal("RunService on a non-service process should fail")
	}

	if !strings.Contains(buf.String(), "service starting") {
		t.Fatalf("log missing \"service starting\":\n%s", buf.String())
	}
}

// execResult wraps the Execute return tuple so it can be read from a channel
// without a goroutine writing two values into the same select.
type execResult struct {
	svcSpecific bool
	exitCode    uint32
}

func TestExecuteGracefulStop(t *testing.T) {
	captureLogs(t)
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
	if s := <-statusCh; s.State != svc.Running || s.Accepts&(svc.AcceptStop|svc.AcceptShutdown|svc.AcceptSessionChange) != svc.AcceptStop|svc.AcceptShutdown|svc.AcceptSessionChange {
		t.Fatalf("second status = %+v, want Running accepting Stop|Shutdown|SessionChange", s)
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
	captureLogs(t) // Execute narrates its lifecycle; keep the suite output clean
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

func TestExecuteSessionChangePokes(t *testing.T) {
	captureLogs(t)
	pokes := 0
	h := &serviceHandler{
		run:           func(ctx context.Context) error { <-ctx.Done(); return nil },
		sessionChange: func() { pokes++ },
	}
	reqCh := make(chan svc.ChangeRequest)
	statusCh := make(chan svc.Status, 8)
	done := make(chan struct{})
	go func() { h.Execute(nil, reqCh, statusCh); close(done) }()
	<-statusCh // StartPending
	<-statusCh // Running

	reqCh <- svc.ChangeRequest{Cmd: svc.SessionChange, EventType: windows.WTS_REMOTE_CONNECT}
	// Interrogate is received after the SessionChange (FIFO channel), so its
	// echo proves the poke already ran — the send alone only synchronizes
	// with the receive.
	reqCh <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	if s := <-statusCh; s.State != svc.Running {
		t.Fatalf("interrogate echo = %v, want Running", s.State)
	}
	if pokes != 1 {
		t.Fatalf("pokes = %d, want 1", pokes)
	}
	reqCh <- svc.ChangeRequest{Cmd: svc.Stop}
	<-done
}
