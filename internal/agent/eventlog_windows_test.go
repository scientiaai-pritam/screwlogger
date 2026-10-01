//go:build windows

package agent

import (
	"errors"
	"testing"
)

// fakeReporter records what the sink routed.
type fakeReporter struct {
	warnings []string
}

func (f *fakeReporter) Warning(msg string) error {
	f.warnings = append(f.warnings, msg)
	return nil
}

// failingReporter fails on every report, like an unregistered event source.
type failingReporter struct {
	calls int
}

func (f *failingReporter) Warning(string) error { f.calls++; return errors.New("no source") }

// Stdlib log lines carry no level, so everything the sink sees is reported as
// one Warning event; the line text is the diagnosis, not the severity icon.
func TestEventLogSinkReportsEveryLine(t *testing.T) {
	r := &fakeReporter{}
	s := newEventLogSinkWith(r)

	first := "2026/10/01 10:00:00 spawner: launch watcher: access is denied"
	if n, err := s.Write([]byte(first + "\n")); err != nil || n != len(first)+1 {
		t.Fatalf("write first line: n=%d err=%v", n, err)
	}
	second := "2026/10/01 10:00:01 service running"
	if n, err := s.Write([]byte(second + "\n")); err != nil || n != len(second)+1 {
		t.Fatalf("write second line: n=%d err=%v", n, err)
	}

	if len(r.warnings) != 2 || r.warnings[0] != first || r.warnings[1] != second {
		t.Fatalf("warnings = %q, want [%q %q]", r.warnings, first, second)
	}
}

func TestEventLogSinkSplitsBatchedLines(t *testing.T) {
	r := &fakeReporter{}
	s := newEventLogSinkWith(r)

	batch := "line one\nline two\n"
	if n, err := s.Write([]byte(batch)); err != nil || n != len(batch) {
		t.Fatalf("write batch: n=%d err=%v", n, err)
	}
	if len(r.warnings) != 2 || r.warnings[0] != "line one" || r.warnings[1] != "line two" {
		t.Fatalf("warnings = %q, want two lines", r.warnings)
	}
}

func TestEventLogSinkKeepsPartialLine(t *testing.T) {
	r := &fakeReporter{}
	s := newEventLogSinkWith(r)

	s.Write([]byte("2026/10/01 partial"))
	s.Write([]byte(" message\n"))

	if len(r.warnings) != 1 || r.warnings[0] != "2026/10/01 partial message" {
		t.Fatalf("warnings = %q, want one joined line", r.warnings)
	}
}

// A broken reporter (unregistered source, full event log) must latch the sink
// off: log output must never make the caller's writes fail, and retrying a
// dead sink on every log line would burn CPU for nothing.
func TestEventLogSinkSurvivesBrokenReporter(t *testing.T) {
	r := &failingReporter{}
	s := newEventLogSinkWith(r)

	for i := 0; i < 3; i++ {
		if n, err := s.Write([]byte("x\n")); err != nil || n != 2 {
			t.Fatalf("write %d: n=%d err=%v, want absorbed", i, n, err)
		}
	}
	if r.calls != 1 {
		t.Fatalf("reporter calls = %d, want 1 (latched off after first failure)", r.calls)
	}
}
