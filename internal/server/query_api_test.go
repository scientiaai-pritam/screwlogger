package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"screwlogger/internal/server"
)

func doReq(h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
}

type apiDevice struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	EnrolledAt int64  `json:"enrolled_at"`
	LastSeen   *int64 `json:"last_seen"`
	RevokedAt  *int64 `json:"revoked_at"`
	Status     string `json:"status"`
}

type apiUsage struct {
	GroupBy string `json:"group_by"`
	Buckets []struct {
		Key     string `json:"key"`
		Seconds int64  `json:"seconds"`
	} `json:"buckets"`
}

type apiRatio struct {
	ActiveSeconds int64   `json:"active_seconds"`
	IdleSeconds   int64   `json:"idle_seconds"`
	Ratio         float64 `json:"ratio"`
	TotalSeconds  int64   `json:"total_seconds"`
}

type apiEvents struct {
	Events []struct {
		ID       string `json:"id"`
		TS       int64  `json:"ts"`
		App      string `json:"app"`
		Category string `json:"category"`
		Active   bool   `json:"active"`
	} `json:"events"`
}

func TestQueryAPIRequiresKey(t *testing.T) {
	s, h := queryAPIHandler(t)
	if _, err := s.CreateAPIKey("unused"); err != nil {
		t.Fatal(err)
	}
	w := doReq(h, "GET", "/api/v1/usage", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", w.Code)
	}
}

func TestQueryAPIUnknownKey(t *testing.T) {
	_, h := queryAPIHandler(t)
	w := doReq(h, "GET", "/api/v1/usage", "ak_bogus")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: want 401, got %d", w.Code)
	}
}

func TestQueryAPIRevokedKey(t *testing.T) {
	s, h := queryAPIHandler(t)
	key, err := s.CreateAPIKey("build-bot")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(key); err != nil {
		t.Fatal(err)
	}
	w := doReq(h, "GET", "/api/v1/usage", key)
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked key: want 403, got %d", w.Code)
	}
}

func queryAPIHandler(t *testing.T) (*server.Store, http.Handler) {
	t.Helper()
	s := openTestStore(t)
	return s, server.QueryAPI(s)
}

func TestQueryAPIDevices(t *testing.T) {
	s, h := queryAPIHandler(t)
	devID, _, err := s.CreateDevice("PC-1")
	if err != nil {
		t.Fatal(err)
	}
	insertHBs(t, s, devID, hbeat("h1", 1000, "excel.exe", "Office", true))
	key, err := s.CreateAPIKey("t")
	if err != nil {
		t.Fatal(err)
	}
	w := doReq(h, "GET", "/api/v1/devices", key)
	if w.Code != http.StatusOK {
		t.Fatalf("devices: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var out []apiDevice
	decodeJSON(t, w, &out)
	if len(out) != 1 {
		t.Fatalf("want 1 device, got %d", len(out))
	}
	d := out[0]
	if d.ID != devID || d.Name != "PC-1" || d.Status != "active" {
		t.Fatalf("device=%+v", d)
	}
	if d.LastSeen == nil {
		t.Fatalf("fresh device must have last_seen set: %+v", d)
	}
	if d.RevokedAt != nil {
		t.Fatalf("unrevoked device must have null revoked_at: %+v", d)
	}
}

func TestQueryAPIDevicesOffline(t *testing.T) {
	s, h := queryAPIHandler(t)
	// A device with no heartbeats has last_seen = NULL -> offline.
	if _, _, err := s.CreateDevice("PC-off"); err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateAPIKey("t")
	if err != nil {
		t.Fatal(err)
	}
	w := doReq(h, "GET", "/api/v1/devices", key)
	if w.Code != http.StatusOK {
		t.Fatalf("devices: want 200, got %d", w.Code)
	}
	var out []apiDevice
	decodeJSON(t, w, &out)
	if len(out) != 1 {
		t.Fatalf("want 1 device, got %d", len(out))
	}
	if out[0].Status != "offline" {
		t.Fatalf("null last_seen must be offline, got %q", out[0].Status)
	}
	if out[0].LastSeen != nil {
		t.Fatalf("null last_seen must be emitted as null, got %v", *out[0].LastSeen)
	}
}

func TestQueryAPIUsage(t *testing.T) {
	s, h := queryAPIHandler(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1010, "word.exe", "Office", true),
	)
	key, _ := s.CreateAPIKey("t")

	// explicit group_by=app
	w := doReq(h, "GET", "/api/v1/usage?device="+devID+"&from=1000&to=1020&group_by=app", key)
	if w.Code != http.StatusOK {
		t.Fatalf("usage: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var byApp apiUsage
	decodeJSON(t, w, &byApp)
	if byApp.GroupBy != "app" {
		t.Fatalf("group_by want app, got %q", byApp.GroupBy)
	}
	if len(byApp.Buckets) != 2 {
		t.Fatalf("want 2 app buckets, got %+v", byApp.Buckets)
	}

	// group_by defaults to category
	w = doReq(h, "GET", "/api/v1/usage?device="+devID+"&from=1000&to=1020", key)
	if w.Code != http.StatusOK {
		t.Fatalf("usage default group_by: want 200, got %d", w.Code)
	}
	var byCat apiUsage
	decodeJSON(t, w, &byCat)
	if byCat.GroupBy != "category" {
		t.Fatalf("default group_by want category, got %q", byCat.GroupBy)
	}
	if len(byCat.Buckets) != 1 || byCat.Buckets[0].Key != "Office" || byCat.Buckets[0].Seconds != 20 {
		t.Fatalf("category buckets=%+v", byCat.Buckets)
	}
}

func TestQueryAPIActiveRatio(t *testing.T) {
	s, h := queryAPIHandler(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID,
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1010, "excel.exe", "Office", false),
	)
	key, _ := s.CreateAPIKey("t")
	w := doReq(h, "GET", "/api/v1/active-ratio?device="+devID+"&from=1000&to=1020", key)
	if w.Code != http.StatusOK {
		t.Fatalf("active-ratio: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var ar apiRatio
	decodeJSON(t, w, &ar)
	if ar.ActiveSeconds != 10 || ar.IdleSeconds != 10 || ar.TotalSeconds != 20 || ar.Ratio != 0.5 {
		t.Fatalf("ratio=%+v", ar)
	}
}

func TestQueryAPIEvents(t *testing.T) {
	s, h := queryAPIHandler(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID,
		hbeat("h3", 1030, "excel.exe", "Office", false),
		hbeat("h1", 1000, "excel.exe", "Office", true),
		hbeat("h2", 1010, "excel.exe", "Office", true),
	)
	key, _ := s.CreateAPIKey("t")
	w := doReq(h, "GET", "/api/v1/events?device="+devID+"&from=1000&to=1100&limit=2", key)
	if w.Code != http.StatusOK {
		t.Fatalf("events: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var ev apiEvents
	decodeJSON(t, w, &ev)
	if len(ev.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(ev.Events))
	}
	if ev.Events[0].TS != 1000 || ev.Events[1].TS != 1010 {
		t.Fatalf("want ASC order, got %+v", ev.Events)
	}
	if !ev.Events[0].Active {
		t.Fatalf("h1 must be active: %+v", ev.Events[0])
	}
}

func TestQueryAPIBadParams(t *testing.T) {
	s, h := queryAPIHandler(t)
	devID, _, _ := s.CreateDevice("PC-1")
	insertHBs(t, s, devID, hbeat("h1", 1000, "excel.exe", "Office", true))
	key, _ := s.CreateAPIKey("t")

	cases := []string{
		"/api/v1/usage?device=" + devID + "&group_by=weird",
		"/api/v1/usage?device=" + devID + "&from=abc",
		"/api/v1/events?device=" + devID + "&limit=0",
		"/api/v1/events?device=" + devID + "&limit=99999",
	}
	for _, path := range cases {
		w := doReq(h, "GET", path, key)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", path, w.Code)
		}
	}
}

func TestQueryAPIOptions(t *testing.T) {
	_, h := queryAPIHandler(t)
	w := doReq(h, "OPTIONS", "/api/v1/usage", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS: want 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin want *, got %q", got)
	}
}

func TestQueryAPICORSOnErrors(t *testing.T) {
	_, h := queryAPIHandler(t)
	w := doReq(h, "GET", "/api/v1/usage", "")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("error response must carry CORS header, got %q", got)
	}
}
