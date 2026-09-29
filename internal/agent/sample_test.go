package agent_test

import (
	"strings"
	"testing"

	"screwlogger/internal/agent"
)

func TestEncodeSampleIsJSONLine(t *testing.T) {
	b := agent.EncodeSample(agent.Sample{App: "chrome.exe", Active: true})
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("encoded sample lacks trailing newline: %q", b)
	}
	if string(b) != `{"app":"chrome.exe","active":true}`+"\n" {
		t.Fatalf("unexpected encoding: %q", b)
	}
}

func TestEncodeSampleEscapesApp(t *testing.T) {
	b := agent.EncodeSample(agent.Sample{App: `we"irdé.exe`, Active: false})
	s, err := agent.DecodeSample(b)
	if err != nil {
		t.Fatalf("decode own encoding: %v", err)
	}
	if s.App != `we"irdé.exe` || s.Active {
		t.Fatalf("roundtrip mismatch: %+v", s)
	}
}

func TestDecodeSampleRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":       nil,
		"blank":       []byte(""),
		"garbage":     []byte("hello\n"),
		"missing app": []byte(`{"active":true}`),
		"empty app":   []byte(`{"app":"","active":true}`),
		"wrong type":  []byte(`{"app":5}`),
	}
	for name, in := range cases {
		if s, err := agent.DecodeSample(in); err == nil {
			t.Fatalf("%s: expected error, got %+v", name, s)
		}
	}
}
