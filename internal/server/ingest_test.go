package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

func ingestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := server.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, token, err := s.CreateDevice("PC-TEST")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.IngestHandler(s))
	t.Cleanup(srv.Close)
	return srv, token
}

func postBatch(t *testing.T, srv *httptest.Server, token string, batch protocol.IngestBatch) *http.Response {
	t.Helper()
	body, _ := json.Marshal(batch)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func validBatch() protocol.IngestBatch {
	return protocol.IngestBatch{SchemaVersion: protocol.SchemaVersion, DeviceSentAt: 1729800001,
		Heartbeats: []protocol.Heartbeat{{ID: "h1", TS: 1729800000, App: "excel.exe", Category: "Office", Active: true}}}
}

func TestIngestSuccess(t *testing.T) {
	srv, token := ingestServer(t)
	resp := postBatch(t, srv, token, validBatch())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var ir protocol.IngestResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	if ir.Accepted != 1 {
		t.Fatalf("accepted=%d", ir.Accepted)
	}
}

func TestIngestDuplicateBatchAcceptedZero(t *testing.T) {
	srv, token := ingestServer(t)
	postBatch(t, srv, token, validBatch())
	resp := postBatch(t, srv, token, validBatch())
	var ir protocol.IngestResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	if resp.StatusCode != http.StatusOK || ir.Accepted != 0 {
		t.Fatalf("replay: status=%d accepted=%d", resp.StatusCode, ir.Accepted)
	}
}

func TestIngestAuthFailures(t *testing.T) {
	srv, _ := ingestServer(t)
	if resp := postBatch(t, srv, "sl_bogus", validBatch()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", resp.StatusCode)
	}
}

func TestIngestBadSchemaVersionRejected(t *testing.T) {
	srv, token := ingestServer(t)
	batch := validBatch()
	batch.SchemaVersion = 99
	if resp := postBatch(t, srv, token, batch); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad schema_version: %d", resp.StatusCode)
	}
}

func TestIngestMalformedJSONRejected(t *testing.T) {
	srv, token := ingestServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ingest", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json: %d", resp.StatusCode)
	}
}

func TestIngestMissingBearer(t *testing.T) {
	srv, _ := ingestServer(t)
	body, _ := json.Marshal(validBatch())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/ingest", bytes.NewReader(body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer: %d", resp.StatusCode)
	}
}

func TestIngestMethodNotAllowed(t *testing.T) {
	srv, _ := ingestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/ingest", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestIngestRevokedToken403(t *testing.T) {
	s, err := server.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	devID, token, err := s.CreateDevice("PC-R")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(devID); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.IngestHandler(s))
	defer srv.Close()
	if resp := postBatch(t, srv, token, validBatch()); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked token: %d", resp.StatusCode)
	}
}
