//go:build windows

package agent

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestSpawnIntoJobChildDiesWhenJobCloses pins the watcher lifecycle contract:
// a child created by spawnIntoJob belongs to the job, so closing the job
// handle kills it. This is what keeps a service restart or -upgrade from
// leaking the previous generation's watcher into the user's session.
func TestSpawnIntoJobChildDiesWhenJobCloses(t *testing.T) {
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	defer windows.CloseHandle(job)

	// Our own primary token exempts the test from SE_ASSIGNPRIMARYTOKEN_NAME;
	// the child lands in this process's session, like a real watcher would.
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY, &tok); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer tok.Close()

	exe := systemCmdExe(t)
	pi, err := spawnIntoJob(tok, exe, `"`+exe+`" /c ping -n 30 127.0.0.1 > nul`, nil, job)
	if err != nil {
		t.Fatalf("spawnIntoJob: %v", err)
	}
	defer func() {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(pi.Thread)
	}()

	// Running, not parked suspended: the child was resumed after assignment.
	if s, err := waitState(pi.Process, 0); err != nil || s != waitTimedOut {
		t.Fatalf("child state right after spawn: state=%d err=%v, want alive (WAIT_TIMEOUT)", s, err)
	}

	// Closing the job kills the child — the service-exit path.
	if err := windows.CloseHandle(job); err != nil {
		t.Fatalf("CloseHandle(job): %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := waitState(pi.Process, 500)
		if err == nil && s == waitSignaled {
			break // child exited when the job handle closed
		}
		if time.Now().After(deadline) {
			t.Fatalf("child still running 10s after job close (state=%d err=%v)", s, err)
		}
	}
	code, err := exitCode(pi.Process)
	if err != nil {
		t.Fatalf("exitCode: %v", err)
	}
	if code == stillActive {
		t.Fatalf("child exit code = STILL_ACTIVE after signaling")
	}
}

// systemCmdExe returns cmd.exe's full path.
func systemCmdExe(t *testing.T) string {
	t.Helper()
	dir, err := windows.GetSystemDirectory()
	if err != nil || dir == "" {
		t.Fatalf("GetSystemDirectory: %v", err)
	}
	return dir + `\cmd.exe`
}

// stillActive is GetExitCodeProcess's STILL_ACTIVE (259); x/sys does not
// export the constant.
const stillActive = 259

const (
	waitSignaled = uint32(windows.WAIT_OBJECT_0)
	waitTimedOut = uint32(windows.WAIT_TIMEOUT)
)

func waitState(h windows.Handle, timeoutMs uint32) (uint32, error) {
	s, err := windows.WaitForSingleObject(h, timeoutMs)
	return uint32(s), err
}

func exitCode(h windows.Handle) (uint32, error) {
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, err
	}
	return code, nil
}
