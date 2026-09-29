//go:build windows

package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func testPipeName() string {
	return fmt.Sprintf(`\\.\pipe\monsvc-test-%d-%d`, os.Getpid(), time.Now().UnixNano()%1_000_000)
}

// currentUserSID returns the test process token's user SID.
func currentUserSID(t *testing.T) string {
	t.Helper()
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// dialPipe opens a write handle to a pipe server, retrying while the server
// brings its pending instance up.
func dialPipe(t *testing.T, name string) io.WriteCloser {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(name),
			windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			return os.NewFile(uintptr(h), name)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipe %s never became available: %v", name, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readSample(t *testing.T, out <-chan Sample) Sample {
	t.Helper()
	select {
	case s := <-out:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no sample within 5s")
		return Sample{}
	}
}

func waitConnected(t *testing.T, p *PipeServer, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Connected() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Connected() still %v, want %v", p.Connected(), want)
}

func startTestPipe(t *testing.T) (*PipeServer, chan Sample, string) {
	t.Helper()
	name := testPipeName()
	p := NewPipeServer(name)
	// Non-elevated dev shells hold Administrators deny-only, so the BA ACE
	// never admits this process; grant the current user read+write the way
	// EnsureSDDL would for a session user (FRFW — a GW-only ACE fails the
	// NPFS access check even for write-direction opens). DACL enforcement
	// itself is a Task 7 live check (spec §7). Set before Serve starts, so
	// there is no race.
	p.sddl = baseSD + fmt.Sprintf("(A;;FRFW;;;%s)", currentUserSID(t))
	out := make(chan Sample, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Serve(ctx, out)
	return p, out, name
}

func TestPipeRoundtrip(t *testing.T) {
	p, out, name := startTestPipe(t)
	c := dialPipe(t, name)
	waitConnected(t, p, true)
	if _, err := c.Write(EncodeSample(Sample{App: "chrome.exe", Active: true})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "chrome.exe" || !s.Active {
		t.Fatalf("sample = %+v", s)
	}
	if _, err := c.Write(EncodeSample(Sample{App: "excel.exe", Active: false})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "excel.exe" || s.Active {
		t.Fatalf("sample = %+v", s)
	}
}

func TestPipeMalformedLinesSkipped(t *testing.T) {
	_, out, name := startTestPipe(t)
	c := dialPipe(t, name)
	bad := []byte("garbage\n" + "\n" + `{"nope":1}` + "\n")
	if _, err := c.Write(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(EncodeSample(Sample{App: "good.exe", Active: true})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "good.exe" {
		t.Fatalf("expected good.exe to survive garbage, got %+v", s)
	}
}

func TestPipeNewestWins(t *testing.T) {
	p, out, name := startTestPipe(t)
	c1 := dialPipe(t, name)
	waitConnected(t, p, true)

	c2 := dialPipe(t, name) // second client: server must drop c1
	if _, err := c2.Write(EncodeSample(Sample{App: "new.exe", Active: true})); err != nil {
		t.Fatal(err)
	}
	if s := readSample(t, out); s.App != "new.exe" {
		t.Fatalf("sample = %+v", s)
	}

	// c1 must become unwritable once the server dropped it (poll: the write
	// may land in an OS buffer once before the connection breaks).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := c1.Write(EncodeSample(Sample{App: "old.exe"})); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old client still writable after newest-wins drop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPipeEnsureSDDLDropsClient(t *testing.T) {
	p, _, name := startTestPipe(t)
	_ = dialPipe(t, name)
	waitConnected(t, p, true)

	// A valid SID that is not ours: the DACL is replaced (not merged) and the
	// current client is dropped. (A non-elevated process cannot dial under
	// the foreign DACL to observe the denial directly; Task 7 checks
	// enforcement live.)
	p.EnsureSDDL("S-1-5-99-12345")
	waitConnected(t, p, false)

	// Granting our own SID again is a DACL change: the server recovers and
	// accepts a new client under the new DACL.
	p.EnsureSDDL(currentUserSID(t))
	c := dialPipe(t, name)
	waitConnected(t, p, true)
	_ = c
}
