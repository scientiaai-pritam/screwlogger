//go:build windows

package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// noConsoleSession is WTSGetActiveConsoleSessionId's "no session" value.
const noConsoleSession = 0xFFFFFFFF

// spawnLadder is the watcher respawn backoff (spec §4.2): 1s, 5s, 30s cap.
// Review Focus #4: the cap keeps a user repeatedly killing the watcher from
// thrashing the CPU.
var spawnLadder = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// samplePipe is what the spawner needs from the pipe server.
type samplePipe interface {
	Connected() bool
	EnsureSDDL(userSID string)
}

// Spawner keeps a watcher alive inside the active console session. On a 30s
// tick — immediately when Poked (session-change events) — it checks whether a
// watcher is attached and, backoff permitting, launches one as the session
// user. Reconciliation is driven by pipe-connection state, not PIDs (spec §4.2).
type Spawner struct {
	ExePath     string
	IdleSeconds int
	Pipe        samplePipe

	ConsoleSession func() uint32
	QueryToken     func(uint32) (windows.Token, error)
	TokenUser      func(windows.Token) (string, error)
	Spawn          func(windows.Token, string, []string) error
	Now            func() time.Time

	nextAttempt time.Time
	ladderIdx   int
	poke        chan struct{}

	// Diagnostic state: transitions are logged once each, and a repeating
	// error (a service parked at the login screen) is logged once per
	// distinct message. The event log is the only record a service leaves,
	// so reconcile narrates what it saw.
	hadSession bool
	hadWatcher bool
	lastErrLog string
}

// NewSpawner fills production collaborators; tests override fields.
func NewSpawner(exePath string, idleSeconds int, pipe *PipeServer) *Spawner {
	return &Spawner{
		ExePath:        exePath,
		IdleSeconds:    idleSeconds,
		Pipe:           pipe,
		ConsoleSession: windows.WTSGetActiveConsoleSessionId,
		QueryToken: func(sid uint32) (windows.Token, error) {
			var tok windows.Token
			if err := windows.WTSQueryUserToken(sid, &tok); err != nil {
				return 0, err
			}
			return tok, nil
		},
		TokenUser:      tokenUserSID,
		Spawn:          spawnWatcherProcess,
		Now:            time.Now,
		poke:           make(chan struct{}, 1),
	}
}

// Poke requests an immediate reconcile (WTS session change). Non-blocking.
func (s *Spawner) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is cancelled. The first reconcile happens
// immediately: on service start (e.g. after -upgrade or reboot-with-autologon)
// a watcher must come up without waiting out the first 30s tick (spec §4.2).
func (s *Spawner) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	s.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.poke:
		case <-tick.C:
		}
		s.reconcile()
	}
}

func (s *Spawner) reconcile() {
	sid := s.ConsoleSession()
	if sid == noConsoleSession || sid == 0 {
		if s.hadSession {
			s.hadSession = false
			log.Printf("spawner: console session gone")
		} else {
			s.logChanged("spawner: no console session (waiting for logon)")
		}
		return // nobody on the console: ship nothing (spec §5)
	}
	if !s.hadSession {
		s.hadSession = true
		log.Printf("spawner: console session appeared (sid=%d)", sid)
	}
	tok, err := s.QueryToken(sid)
	if err != nil {
		s.logChanged(fmt.Sprintf("spawner: query token for session %d: %v", sid, err))
		s.consumeLadder(s.Now())
		return
	}
	defer tok.Close()
	sidStr, err := s.TokenUser(tok)
	if err != nil {
		s.logChanged(fmt.Sprintf("spawner: token user: %v", err))
		s.consumeLadder(s.Now())
		return
	}
	// Grant the session user pipe-write access on every reconcile, not only
	// on the spawn path: while a watcher is attached it is a no-op, but on a
	// fast user switch it drops the previous user's still-attached watcher
	// (self-paused before its write, so the pipe never reports a failure),
	// which is what unblocks the spawn below for the new console user.
	s.Pipe.EnsureSDDL(sidStr)
	now := s.Now()
	connected := s.Pipe.Connected()
	if connected != s.hadWatcher {
		s.hadWatcher = connected
		if connected {
			log.Printf("spawner: watcher attached (user %s)", sidStr)
		} else {
			log.Printf("spawner: watcher gone (respawning)")
		}
	}
	if connected {
		s.ladderIdx = 0 // a watcher is attached; ladder resets
		s.nextAttempt = time.Time{}
		return
	}
	if now.Before(s.nextAttempt) {
		return
	}
	if err := s.Spawn(tok, s.ExePath, []string{"-userwatch", "-idle", fmt.Sprintf("%d", s.IdleSeconds)}); err != nil {
		s.logChanged(fmt.Sprintf("spawner: launch watcher: %v", err))
	} else {
		log.Printf("spawner: launched watcher for session %d (user %s)", sid, sidStr)
	}
	s.consumeLadder(now)
}

// logChanged logs msg unless it is identical to the last logged line: a
// service waiting out the respawn ladder at the login screen would otherwise
// repeat the same failure every 30s for as long as nobody logs on.
func (s *Spawner) logChanged(msg string) {
	if msg == s.lastErrLog {
		return
	}
	s.lastErrLog = msg
	log.Print(msg)
}

// consumeLadder schedules the next attempt per the backoff ladder.
func (s *Spawner) consumeLadder(now time.Time) {
	s.nextAttempt = now.Add(spawnLadder[s.ladderIdx])
	if s.ladderIdx < len(spawnLadder)-1 {
		s.ladderIdx++
	}
}

// tokenUserSID resolves the account SID string of a user token.
func tokenUserSID(tok windows.Token) (string, error) {
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

var (
	userenv                     = windows.NewLazySystemDLL("userenv.dll")
	procCreateEnvironmentBlock  = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock = userenv.NewProc("DestroyEnvironmentBlock")
)

// spawnWatcherProcess launches exe with args in the token's session and with
// no window (spec §2, §4.2). The child is not tracked by PID — pipe-connection
// state drives reconciliation — but it IS placed in the service's kill-on-close
// job object: when the service process exits (stop, crash, -upgrade), the job
// handle closes and the watcher dies with it. Without that, the previous
// generation's watcher survives a restart, redials the new pipe, and fights
// the new watcher (newest-wins flapping) — observed live during Task 7.
func spawnWatcherProcess(tok windows.Token, exe string, args []string) error {
	env, err := createEnvironmentBlock(tok)
	if err != nil {
		return fmt.Errorf("environment block: %w", err)
	}
	defer destroyEnvironmentBlock(env)

	line := `"` + exe + `"`
	for _, a := range args {
		line += " " + syscall.EscapeArg(a)
	}
	job, err := getWatcherJob()
	if err != nil {
		return fmt.Errorf("watcher job object: %w", err)
	}
	pi, err := spawnIntoJob(tok, exe, line, env, job)
	if err != nil {
		return err
	}
	windows.CloseHandle(pi.Process)
	windows.CloseHandle(pi.Thread)
	return nil
}

// createWatcherJob is the job-object factory; a var so tests can inject a
// transient failure. A failure here must NOT latch: getWatcherJob retries on
// the next spawn.
var createWatcherJob = createKillOnCloseJob

var (
	watcherJobMu sync.Mutex
	watcherJob   windows.Handle
)

// getWatcherJob lazily creates the per-service-process job object. The handle
// lives for the process's lifetime; the OS closes it on any exit path, which
// is what kills the watchers. A creation failure (seen in the wild as a
// boot-time service start) is returned to the caller and retried on the next
// spawn — one bad moment at boot must not silence the service for its whole
// uptime.
func getWatcherJob() (windows.Handle, error) {
	watcherJobMu.Lock()
	defer watcherJobMu.Unlock()
	if watcherJob != 0 {
		return watcherJob, nil
	}
	h, err := createWatcherJob()
	if err != nil {
		log.Printf("spawner: watcher job create failed (retrying next spawn): %v", err)
		return 0, err
	}
	watcherJob = h
	return h, nil
}

// createKillOnCloseJob returns a job object handle that terminates every
// process in it when the handle is closed.
func createKillOnCloseJob() (windows.Handle, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	li := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	li.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&li)), uint32(unsafe.Sizeof(li))); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

// spawnIntoJob creates exe under tok as a suspended child, assigns it to job,
// and resumes it. The suspend-assign-resume order closes the race where a
// just-created child could outlive a service that dies mid-spawn: an
// unassigned child is still suspended, and a failure after creation
// terminates it rather than leaking it.
func spawnIntoJob(tok windows.Token, exe, cmdLine string, env *uint16, job windows.Handle) (windows.ProcessInformation, error) {
	cl, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(tok, exePtr, cl, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW|windows.CREATE_SUSPENDED,
		env, nil, si, &pi); err != nil {
		return windows.ProcessInformation{}, err
	}
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return windows.ProcessInformation{}, fmt.Errorf("assign to job: %w", err)
	}
	windows.ResumeThread(pi.Thread)
	return pi, nil
}

// createEnvironmentBlock builds the user's environment (TEMP, APPDATA, …).
func createEnvironmentBlock(tok windows.Token) (*uint16, error) {
	var env *uint16
	r, _, callErr := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&env)), uintptr(tok), 1)
	if r == 0 {
		return nil, callErr
	}
	return env, nil
}

func destroyEnvironmentBlock(env *uint16) {
	procDestroyEnvironmentBlock.Call(uintptr(unsafe.Pointer(env)))
}
