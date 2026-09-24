package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"

	"screwlogger/internal/protocol"
)

func TestHeartbeatJSONRoundTrip(t *testing.T) {
	hb := protocol.Heartbeat{ID: "abc", TS: 1729800000, App: "excel.exe", Category: "Office", Active: true}
	b, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":"abc","ts":1729800000,"app":"excel.exe","category":"Office","active":true}` {
		t.Fatalf("unexpected wire format: %s", b)
	}
	var back protocol.Heartbeat
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != hb {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestIngestBatchWireFormat(t *testing.T) {
	batch := protocol.IngestBatch{SchemaVersion: protocol.SchemaVersion, DeviceSentAt: 1729800001,
		Heartbeats: []protocol.Heartbeat{{ID: "x", TS: 1, App: "a", Category: "c", Active: false}}}
	b, _ := json.Marshal(batch)
	for _, want := range []string{`"schema_version":1`, `"device_sent_at"`, `"heartbeats"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
}

func TestIngestResponseWireFormat(t *testing.T) {
	resp := protocol.IngestResponse{Accepted: 3}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"accepted"`) {
		t.Fatalf("missing %s in %s", `"accepted"`, b)
	}
}
