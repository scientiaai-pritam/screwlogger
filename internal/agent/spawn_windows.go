//go:build windows

package agent

import (
	"context"
	"fmt"
	"log"
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
	if s.Pipe.Connected() {
		s.ladderIdx = 0 // a watcher is attached; ladder resets
		s.nextAttempt = time.Time{}
		return
	}
	sid := s.ConsoleSession()
	if sid == noConsoleSession || sid == 0 {
		return // nobody on the console: ship nothing (spec §5)
	}
	now := s.Now()
	if now.Before(s.nextAttempt) {
		return
	}
	tok, err := s.QueryToken(sid)
	if err != nil {
		log.Printf("spawner: query token for session %d: %v", sid, err)
		s.consumeLadder(now)
		return
	}
	defer tok.Close()
	sidStr, err := s.TokenUser(tok)
	if err != nil {
		log.Printf("spawner: token user: %v", err)
		s.consumeLadder(now)
		return
	}
	// Grant the session user pipe-write access before the watcher exists.
	s.Pipe.EnsureSDDL(sidStr)
	if err := s.Spawn(tok, s.ExePath, []string{"-userwatch", "-idle", fmt.Sprintf("%d", s.IdleSeconds)}); err != nil {
		log.Printf("spawner: launch watcher: %v", err)
	}
	s.consumeLadder(now)
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
// no window (spec §2, §4.2). The child is not tracked: pipe-connection state
// drives reconciliation, so its handles are closed immediately.
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
	cl, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return err
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(tok, exePtr, cl, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		env, nil, si, &pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Process)
	windows.CloseHandle(pi.Thread)
	return nil
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
