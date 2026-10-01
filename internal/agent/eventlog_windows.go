//go:build windows

package agent

import (
	"bytes"
	"strings"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

// The service reports its diagnostics (spawn failures, flush errors,
// lifecycle) through the standard library log. In service mode stderr goes
// nowhere, so the log output is routed into the Application event log under
// the service's source instead — the one place a silent PC can still say
// what it did (Event Viewer → Windows Logs → Application, or remotely via
// Get-WinEvent -ComputerName).

// eventReporter is the seam that keeps the sink testable without a real
// event source.
type eventReporter interface {
	Warning(msg string) error
}

const eventIDWarning = 1

type eventLogReporter struct{ el *eventlog.Log }

func (r eventLogReporter) Warning(msg string) error { return r.el.Warning(eventIDWarning, msg) }

// eventLogSink is a log output that reports each complete line as one event.
type eventLogSink struct {
	report  eventReporter
	closer  func() error
	pending []byte
	broken  bool
}

// NewEventLogSink registers (best-effort) and opens the named event-log
// source, returning a log output for it. On error the caller should proceed
// without the sink — diagnostics are best-effort and must never keep the
// service from running.
func NewEventLogSink(source string) (*eventLogSink, error) {
	ensureEventLogSource(source)
	el, err := eventlog.Open(source)
	if err != nil {
		return nil, err
	}
	return &eventLogSink{
		report: eventLogReporter{el},
		closer: el.Close,
	}, nil
}

// newEventLogSinkWith builds a sink around the given reporter (test seam).
func newEventLogSinkWith(r eventReporter) *eventLogSink {
	return &eventLogSink{report: r}
}

// Close releases the event source handle.
func (s *eventLogSink) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer()
}

// Write splits p into lines and reports each as one Warning event — stdlib
// log lines carry no level, so the line text is the diagnosis. A failing
// reporter latches the sink off: log output must never make the caller's
// write fail, and retrying a dead sink on every line would burn CPU.
func (s *eventLogSink) Write(p []byte) (int, error) {
	if s.broken {
		return len(p), nil
	}
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(s.pending[:i]), "\r")
		s.pending = s.pending[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := s.report.Warning(line); err != nil {
			s.broken = true
			s.pending = s.pending[:0]
			break
		}
	}
	if len(s.pending) > 64<<10 {
		s.pending = s.pending[:0] // drop a runaway partial line
	}
	return len(p), nil
}

// ensureEventLogSource registers the source and points EventMessageFile at
// the system's generic "%1" message table so lines render as written in
// Event Viewer. Best-effort: without admin rights the registry write fails
// and the Open below decides whether a sink is possible at all.
func ensureEventLogSource(source string) {
	err := eventlog.InstallAsEventCreate(source, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		// fall through: Open reports a usable error if this mattered
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services\EventLog\Application\`+source, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue("EventMessageFile",
		`%SystemRoot%\Microsoft.NET\Framework64\v4.0.30319\EventLogMessages.dll`)
}
