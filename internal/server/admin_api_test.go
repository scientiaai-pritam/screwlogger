package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"screwlogger/internal/protocol"
	"screwlogger/internal/server"
)

// adminAPI builds a real AdminAPI handler over a fresh store plus an AdminAuth
// with a known password.
func adminAPI(t *testing.T) (*server.Store, *server.AdminAuth, http.Handler) {
	t.Helper()
	s := openTestStore(t)
	auth, _, err := server.NewAdminAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	return s, auth, server.AdminAPI(s, auth)
}

// adminLogin logs in via the AdminAuth handler and returns the sl_admin cookie.
func adminLogin(t *testing.T, auth *server.AdminAuth, password string) *http.Cookie {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	auth.Login(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "sl_admin" {
			return c
		}
	}
	t.Fatalf("no sl_admin cookie; Set-Cookie=%v", w.Header()["Set-Cookie"])
	return nil
}

// adminReq issues a request against the admin handler, attaching the session
// cookie and (optionally) a JSON body.
func adminReq(h http.Handler, method, path string, cookie *http.Cookie, payload any) *httptest.ResponseRecorder {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, body)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdminAPIRequiresSession(t *testing.T) {
	_, _, h := adminAPI(t)
	routes := []struct{ method, path string }{
		{"POST", "/admin/api/devices"},
		{"GET", "/admin/api/devices"},
		{"POST", "/admin/api/devices/someid/revoke"},
		{"GET", "/admin/api/rules"},
		{"PUT", "/admin/api/rules"},
		{"DELETE", "/admin/api/rules"},
		{"POST", "/admin/api/keys"},
		{"GET", "/admin/api/keys"},
		{"POST", "/admin/api/keys/abcdefgh/revoke"},
		{"GET", "/admin/api/usage"},
		{"GET", "/admin/api/active-ratio"},
		{"GET", "/admin/api/events"},
	}
	for _, rt := range routes {
		w := adminReq(h, rt.method, rt.path, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401 without a session, got %d", rt.method, rt.path, w.Code)
		}
	}
}

func TestAdminEnrollDeviceThenIngest(t *testing.T) {
	s, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")

	w := adminReq(h, "POST", "/admin/api/devices", cookie, map[string]string{"name": "PC-4"})
	if w.Code != http.StatusOK {
		t.Fatalf("enroll: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	decodeJSON(t, w, &resp)
	if resp.DeviceID == "" || !strings.HasPrefix(resp.Token, "sl_") {
		t.Fatalf("enroll response must carry device_id + sl_ token: %+v", resp)
	}

	// The one-time token must work against the ingest handler.
	ingest := server.IngestHandler(s)
	body, _ := json.Marshal(protocol.IngestBatch{
		SchemaVersion: protocol.SchemaVersion,
		DeviceSentAt:  1000,
		Heartbeats:    []protocol.Heartbeat{{ID: "h1", TS: 1000, App: "excel.exe", Category: "Office", Active: true}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	iw := httptest.NewRecorder()
	ingest.ServeHTTP(iw, req)
	if iw.Code != http.StatusOK {
		t.Fatalf("ingest with enrolled token: want 200, got %d (%s)", iw.Code, iw.Body.String())
	}
}

func TestAdminListDevicesNoToken(t *testing.T) {
	_, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")

	w := adminReq(h, "POST", "/admin/api/devices", cookie, map[string]string{"name": "PC-4"})
	var enrolled struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	decodeJSON(t, w, &enrolled)

	lw := adminReq(h, "GET", "/admin/api/devices", cookie, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("list devices: want 200, got %d (%s)", lw.Code, lw.Body.String())
	}
	if strings.Contains(lw.Body.String(), enrolled.Token) {
		t.Fatalf("device token leaked in list: %s", lw.Body.String())
	}
	var devices []apiDevice
	decodeJSON(t, lw, &devices)
	if len(devices) != 1 || devices[0].Name != "PC-4" || devices[0].Status != "offline" {
		t.Fatalf("devices=%+v", devices)
	}
}

func TestAdminRevokeDeviceStopsIngest(t *testing.T) {
	s, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")

	w := adminReq(h, "POST", "/admin/api/devices", cookie, map[string]string{"name": "PC-4"})
	var resp struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	decodeJSON(t, w, &resp)

	rw := adminReq(h, "POST", "/admin/api/devices/"+resp.DeviceID+"/revoke", cookie, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("revoke: want 200, got %d (%s)", rw.Code, rw.Body.String())
	}

	// The revoked token must now be rejected (403) on ingest.
	ingest := server.IngestHandler(s)
	body, _ := json.Marshal(protocol.IngestBatch{
		SchemaVersion: protocol.SchemaVersion,
		Heartbeats:    []protocol.Heartbeat{{ID: "h1", TS: 1000, App: "excel.exe", Category: "Office", Active: true}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	iw := httptest.NewRecorder()
	ingest.ServeHTTP(iw, req)
	if iw.Code != http.StatusForbidden {
		t.Fatalf("revoked token: want 403, got %d (%s)", iw.Code, iw.Body.String())
	}
}

func TestAdminRuleApplyToHistoryRecategorizes(t *testing.T) {
	s, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")

	devID, _, err := s.CreateDevice("PC-1")
	if err != nil {
		t.Fatal(err)
	}
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "MES_LINE3.EXE", "Manufacturing", true),
		hbeat("h2", 1010, "chrome.exe", "Browser", true),
	)

	w := adminReq(h, "PUT", "/admin/api/rules", cookie, map[string]any{
		"pattern": "mes_*.exe", "category": "Production", "apply_to_history": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("put rule: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var rr struct {
		OK            bool  `json:"ok"`
		Recategorized int64 `json:"recategorized"`
	}
	decodeJSON(t, w, &rr)
	if !rr.OK || rr.Recategorized != 1 {
		t.Fatalf("put rule response: %+v", rr)
	}

	// Case-insensitive re-map via ListEvents.
	evs, err := s.ListEvents(devID, 1000, 1100, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.App == "MES_LINE3.EXE" && e.Category != "Production" {
			t.Fatalf("mes category not remapped: %+v", e)
		}
		if e.App == "chrome.exe" && e.Category != "Browser" {
			t.Fatalf("chrome category wrongly changed: %+v", e)
		}
	}

	// And via DwellUsage: Production and Browser each own 10 s of [1000, 1020).
	buckets, err := s.DwellUsage(devID, 1000, 1020, "category")
	if err != nil {
		t.Fatal(err)
	}
	var prod, browser int64
	for _, b := range buckets {
		switch b.Key {
		case "Production":
			prod = b.Seconds
		case "Browser":
			browser = b.Seconds
		}
	}
	if prod != 10 || browser != 10 {
		t.Fatalf("buckets=%+v (prod=%d browser=%d)", buckets, prod, browser)
	}
}

func TestAdminRulesRoundTrip(t *testing.T) {
	_, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")

	type ruleJSON struct {
		Pattern   string `json:"pattern"`
		Category  string `json:"category"`
		UpdatedAt int64  `json:"updated_at"`
	}
	var rules []ruleJSON

	lw := adminReq(h, "GET", "/admin/api/rules", cookie, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("list rules: want 200, got %d", lw.Code)
	}
	decodeJSON(t, lw, &rules)
	if len(rules) != 0 {
		t.Fatalf("rules=%+v", rules)
	}

	pw := adminReq(h, "PUT", "/admin/api/rules", cookie, map[string]string{"pattern": "mes_*.exe", "category": "Production"})
	if pw.Code != http.StatusOK {
		t.Fatalf("put rule: want 200, got %d (%s)", pw.Code, pw.Body.String())
	}

	lw = adminReq(h, "GET", "/admin/api/rules", cookie, nil)
	decodeJSON(t, lw, &rules)
	if len(rules) != 1 || rules[0].Pattern != "mes_*.exe" || rules[0].Category != "Production" {
		t.Fatalf("rules=%+v", rules)
	}

	dw := adminReq(h, "DELETE", "/admin/api/rules", cookie, map[string]string{"pattern": "mes_*.exe"})
	if dw.Code != http.StatusOK {
		t.Fatalf("delete rule: want 200, got %d (%s)", dw.Code, dw.Body.String())
	}
	lw = adminReq(h, "GET", "/admin/api/rules", cookie, nil)
	decodeJSON(t, lw, &rules)
	if len(rules) != 0 {
		t.Fatalf("rules after delete=%+v", rules)
	}
}

func TestAdminKeyLifecycle(t *testing.T) {
	s, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")

	w := adminReq(h, "POST", "/admin/api/keys", cookie, map[string]string{"label": "build-bot"})
	if w.Code != http.StatusOK {
		t.Fatalf("create key: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Key string `json:"key"`
	}
	decodeJSON(t, w, &resp)
	if !strings.HasPrefix(resp.Key, "ak_") {
		t.Fatalf("key must be ak_-prefixed: %q", resp.Key)
	}

	// The key works against QueryAPI.
	qh := server.QueryAPI(s)
	qw := doReq(qh, "GET", "/api/v1/usage", resp.Key)
	if qw.Code != http.StatusOK {
		t.Fatalf("query with key: want 200, got %d (%s)", qw.Code, qw.Body.String())
	}

	// List shows only the 8-char hash prefix, never the plaintext.
	lw := adminReq(h, "GET", "/admin/api/keys", cookie, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("list keys: want 200, got %d", lw.Code)
	}
	var keys []struct {
		Label      string `json:"label"`
		CreatedAt  int64  `json:"created_at"`
		RevokedAt  *int64 `json:"revoked_at"`
		HashPrefix string `json:"hash_prefix"`
	}
	decodeJSON(t, lw, &keys)
	if len(keys) != 1 || len(keys[0].HashPrefix) != 8 {
		t.Fatalf("keys=%+v", keys)
	}
	if strings.Contains(lw.Body.String(), resp.Key) {
		t.Fatalf("plaintext key leaked in list: %s", lw.Body.String())
	}

	// Revoke by the listed hash prefix stops the key.
	rw := adminReq(h, "POST", "/admin/api/keys/"+keys[0].HashPrefix+"/revoke", cookie, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("revoke key: want 200, got %d (%s)", rw.Code, rw.Body.String())
	}
	qw2 := doReq(qh, "GET", "/api/v1/usage", resp.Key)
	if qw2.Code != http.StatusForbidden {
		t.Fatalf("revoked key: want 403, got %d", qw2.Code)
	}
}

func TestAdminKeyRevokeUnknown(t *testing.T) {
	_, auth, h := adminAPI(t)
	cookie := adminLogin(t, auth, "s3cret")
	rw := adminReq(h, "POST", "/admin/api/keys/deadbeef/revoke", cookie, nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("unknown prefix: want 404, got %d (%s)", rw.Code, rw.Body.String())
	}
}
