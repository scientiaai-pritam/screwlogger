//go:build windows

package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// samplePipeName is the local pipe in-session watchers write samples to.
const samplePipeName = `\\.\pipe\monsvc-samples`

// baseSD denies everyone by default and grants full access to SYSTEM and
// Administrators; EnsureSDDL appends read+write ACEs for the console session
// user's SID (spec §4.3).
const baseSD = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"

// PipeServer accepts watcher connections (newest wins) and publishes decoded
// samples to a channel. All state is guarded by mu; Serve owns the accept
// loop, EnsureSDDL may run on the spawner's goroutine.
type PipeServer struct {
	name string

	mu        sync.Mutex
	sddl      string
	pending   windows.Handle // instance waiting in ConnectNamedPipe, 0 when none
	client    io.ReadCloser
	connected bool
	dirty     bool // DACL changed; Serve must drop the pending instance
}

// NewPipeServer creates the server; the DACL starts as baseSD (no user ACE)
// until EnsureSDDL names the session user.
func NewPipeServer(name string) *PipeServer {
	return &PipeServer{name: name, sddl: baseSD}
}

// Connected reports whether a watcher is currently attached.
func (p *PipeServer) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connected
}

// EnsureSDDL grants the session user access to the pipe. The ACE is FRFW
// (FILE_GENERIC_READ|FILE_GENERIC_WRITE): NPFS rejects client opens that lack
// the read bits even on a write-direction open, and a plain GW ACE fails the
// check (probed empirically — spec §4.3). A DACL change drops the current
// client and the pending instance so the watcher for the new session can
// attach (a stale watcher is denied on reconnect and exits; the spawner then
// launches the new session's watcher — spec §4.3, Review Focus #2).
func (p *PipeServer) EnsureSDDL(userSID string) {
	if userSID == "" {
		return
	}
	sddl := baseSD + fmt.Sprintf("(A;;FRFW;;;%s)", userSID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if sddl == p.sddl {
		return
	}
	p.sddl = sddl
	p.dirty = true
	p.connected = false
	if p.client != nil {
		p.client.Close()
		p.client = nil
	}
	if p.pending != 0 {
		// Unblocks ConnectNamedPipe; Serve closes the handle and recreates.
		windows.CancelIoEx(p.pending, nil)
	}
}

// Serve accepts clients until ctx is cancelled, publishing decoded samples
// to out. Exactly one pending pipe instance exists at a time; on accept, any
// previous client is closed (newest wins).
func (p *PipeServer) Serve(ctx context.Context, out chan<- Sample) {
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.connected = false
		if p.client != nil {
			p.client.Close()
			p.client = nil
		}
		if p.pending != 0 {
			windows.CancelIoEx(p.pending, nil)
			windows.CloseHandle(p.pending)
			p.pending = 0
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		sddl, dirty := p.sddl, p.dirty
		p.dirty = false
		p.mu.Unlock()

		if dirty && p.pendingHandle() != 0 {
			// Drop the stale-DACL instance; recreate below.
			p.mu.Lock()
			h := p.pending
			p.pending = 0
			p.mu.Unlock()
			windows.CloseHandle(h)
		}
		pending := p.pendingHandle()
		if pending == 0 {
			h, err := createPipeInstance(p.name, sddl)
			if err != nil {
				log.Printf("pipe create: %v (retry in 1s)", err)
				if !sleepCtx(ctx, time.Second) {
					return
				}
				continue
			}
			// Store the pending handle and re-check dirty atomically: an
			// EnsureSDDL between the sddl read and the create must not leave
			// a stale-DACL instance waiting forever.
			p.mu.Lock()
			if p.dirty {
				p.mu.Unlock()
				windows.CloseHandle(h)
				continue
			}
			p.pending = h
			p.mu.Unlock()
			pending = h
		}

		connCh := make(chan error, 1)
		go func() { connCh <- windows.ConnectNamedPipe(pending, nil) }()
		var connErr error
		select {
		case <-ctx.Done():
			windows.CancelIoEx(pending, nil)
			p.mu.Lock()
			p.pending = 0
			p.mu.Unlock()
			windows.CloseHandle(pending)
			return
		case connErr = <-connCh:
		}
		p.mu.Lock()
		p.pending = 0
		p.mu.Unlock()

		if connErr != nil {
			windows.CloseHandle(pending) // no connection; discard the instance
			if ctx.Err() != nil {
				return
			}
			continue // canceled by EnsureSDDL or spurious; recreate
		}
		// On success pending is the server's read end of the connection: it
		// stays open and is adopted (and later closed) by readClient via the
		// os.File below — closing it here would break every client.

		p.mu.Lock()
		if p.dirty {
			// The DACL changed while this instance waited: this client (or
			// connect) belongs to the old session — drop and recreate.
			c := os.NewFile(uintptr(pending), p.name)
			c.Close()
			p.mu.Unlock()
			continue
		}
		if p.client != nil {
			p.client.Close() // newest wins
		}
		c := os.NewFile(uintptr(pending), p.name)
		p.client = c
		p.connected = true
		p.mu.Unlock()
		go p.readClient(c, out)
	}
}

func (p *PipeServer) pendingHandle() windows.Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending
}

// readClient decodes lines until the client goes away; malformed lines are
// logged and skipped (Review Focus #3). On disconnect it clears connected
// state unless a newer client already replaced it.
func (p *PipeServer) readClient(c io.ReadCloser, out chan<- Sample) {
	defer func() {
		p.mu.Lock()
		if p.client == c {
			p.client = nil
			p.connected = false
		}
		p.mu.Unlock()
		c.Close()
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), 4096)
	for sc.Scan() {
		s, err := DecodeSample(sc.Bytes())
		if err != nil {
			log.Printf("pipe: %v", err)
			continue
		}
		select {
		case out <- s:
		default:
			log.Printf("pipe: sample dropped (consumer slow)")
		}
	}
}

// createPipeInstance opens one server-side pipe instance with the given DACL.
func createPipeInstance(name, sddl string) (windows.Handle, error) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, fmt.Errorf("sddl: %w", err)
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateNamedPipe(windows.StringToUTF16Ptr(name),
		windows.PIPE_ACCESS_INBOUND,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		windows.PIPE_UNLIMITED_INSTANCES, 512, 512, 0, sa)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// sleepCtx waits for d or ctx cancellation; reports whether d elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
