package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

// TestM2EndToEnd drives the full M2 journey through the same route assembly as
// main (server.NewMux): enroll → ingest → query API → admin login → admin enroll
// → ingest with the fresh token. It is the M2 gate test (spec §8).
func TestM2EndToEnd(t *testing.T) {
	store := openTestStore(t)
	auth, _, err := server.NewAdminAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	mux := server.NewMux(store, auth)

	// 1. Enroll a device directly on the store (as -enroll does).
	devID, devToken, err := store.CreateDevice("PC-1")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Ingest a two-heartbeat batch with the device token.
	w := ingestReq(t, mux, devToken, 1000,
		hbeat("e1", 1000, "excel.exe", "Office", true),
		hbeat("e2", 1010, "excel.exe", "Office", false),
	)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// 3. Create a read-only API key (as -apikey / the admin UI does).
	apiKey, err := store.CreateAPIKey("e2e")
	if err != nil {
		t.Fatal(err)
	}

	// 4. Query the read-only API with the API key.
	usage := doReq(mux, "GET", "/api/v1/usage?device="+devID+"&from=1000&to=1020&group_by=app", apiKey)
	if usage.Code != http.StatusOK {
		t.Fatalf("usage: want 200, got %d (%s)", usage.Code, usage.Body.String())
	}
	var u apiUsage
	decodeJSON(t, usage, &u)
	if len(u.Buckets) != 1 || u.Buckets[0].Key != "excel.exe" || u.Buckets[0].Seconds != 20 {
		t.Fatalf("usage buckets=%+v", u.Buckets)
	}

	ratio := doReq(mux, "GET", "/api/v1/active-ratio?device="+devID+"&from=1000&to=1020", apiKey)
	if ratio.Code != http.StatusOK {
		t.Fatalf("active-ratio: want 200, got %d (%s)", ratio.Code, ratio.Body.String())
	}
	var ar apiRatio
	decodeJSON(t, ratio, &ar)
	if ar.ActiveSeconds != 10 || ar.IdleSeconds != 10 || ar.TotalSeconds != 20 || ar.Ratio != 0.5 {
		t.Fatalf("ratio=%+v", ar)
	}

	// No plaintext token or key may leak through any /api/v1/* response body.
	for _, path := range []string{
		"/api/v1/devices",
		"/api/v1/usage?device=" + devID + "&from=1000&to=1020",
		"/api/v1/active-ratio?device=" + devID + "&from=1000&to=1020",
		"/api/v1/events?device=" + devID + "&from=1000&to=1020",
	} {
		rw := doReq(mux, "GET", path, apiKey)
		if rw.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d (%s)", path, rw.Code, rw.Body.String())
		}
		body := rw.Body.String()
		for _, leak := range []string{devToken, apiKey, "sl_", "ak_"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s leaked %q: %s", path, leak, body)
			}
		}
	}

	// 5. Admin login (POST /admin/login) and grab the sl_admin session cookie.
	cookie := adminLogin(t, auth, "s3cret")

	// 6. Enroll PC-2 via the admin API (session auth).
	enroll := adminReq(mux, "POST", "/admin/api/devices", cookie, map[string]string{"name": "PC-2"})
	if enroll.Code != http.StatusOK {
		t.Fatalf("admin enroll: want 200, got %d (%s)", enroll.Code, enroll.Body.String())
	}
	var er struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	decodeJSON(t, enroll, &er)
	if er.DeviceID == "" || !strings.HasPrefix(er.Token, "sl_") {
		t.Fatalf("admin enroll must return device_id + sl_ token: %+v", er)
	}

	// 7. The admin-enrolled device is live end-to-end: its fresh token ingests.
	iw := ingestReq(t, mux, er.Token, 5000,
		hbeat("e3", 5000, "chrome.exe", "Browser", true),
	)
	if iw.Code != http.StatusOK {
		t.Fatalf("ingest fresh token: want 200, got %d (%s)", iw.Code, iw.Body.String())
	}

	// 8. The admin device list contains both enrolled PCs.
	list := adminReq(mux, "GET", "/admin/api/devices", cookie, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("admin list: want 200, got %d (%s)", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "PC-1") || !strings.Contains(list.Body.String(), "PC-2") {
		t.Fatalf("admin list missing enrolled devices: %s", list.Body.String())
	}
}

// ingestReq POSTs an ingest batch (Bearer device-token auth) against the given
// handler and returns the recorder.
func ingestReq(t *testing.T, h http.Handler, token string, sentAt int64, hbs ...protocol.Heartbeat) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(protocol.IngestBatch{
		SchemaVersion: protocol.SchemaVersion,
		DeviceSentAt:  sentAt,
		Heartbeats:    hbs,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
