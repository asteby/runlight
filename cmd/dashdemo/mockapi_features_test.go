package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, mux *http.ServeMux, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

var okHdr = map[string]string{"Content-Type": "application/json", "X-Runlight": "1"}

func TestFeatureMocksSettingsValidation(t *testing.T) {
	mux := http.NewServeMux()
	registerFeatureMocks(mux)

	for name, tc := range map[string]struct {
		body string
		key  string
	}{
		"out of range": {`{"changes":{"scaling.maxRunners":99}}`, "scaling.maxRunners"},
		"not integer":  {`{"changes":{"scaling.maxRunners":2.5}}`, "scaling.maxRunners"},
		"unknown":      {`{"changes":{"nope":1}}`, "nope"},
		"wrong type":   {`{"changes":{"alert.disk_low.enabled":"yes"}}`, "alert.disk_low.enabled"},
		"min over max": {`{"changes":{"scaling.minRunners":5}}`, "scaling.minRunners"},
	} {
		w := do(t, mux, "PUT", "/api/settings", tc.body, okHdr)
		var got struct {
			Fields map[string]string `json:"fields"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != 400 || got.Fields[tc.key] == "" {
			t.Errorf("%s: code=%d body=%s", name, w.Code, w.Body)
		}
	}

	// Atomic: one bad field means nothing is applied.
	do(t, mux, "PUT", "/api/settings", `{"changes":{"scaling.maxRunners":4,"scaling.idleTtlSec":1}}`, okHdr)
	var s struct {
		Values map[string]float64 `json:"values"`
	}
	_ = json.Unmarshal(do(t, mux, "GET", "/api/settings", "", nil).Body.Bytes(), &s)
	if s.Values["scaling.maxRunners"] != 3 {
		t.Errorf("partial apply: %v", s.Values["scaling.maxRunners"])
	}

	if w := do(t, mux, "PUT", "/api/settings", `{"changes":{"scaling.maxRunners":4}}`, okHdr); w.Code != 200 {
		t.Errorf("valid change: %d %s", w.Code, w.Body)
	}
}

func TestFeatureMocksGuards(t *testing.T) {
	mux := http.NewServeMux()
	registerFeatureMocks(mux)
	body := `{"changes":{}}`
	if w := do(t, mux, "PUT", "/api/settings", body, map[string]string{"Content-Type": "application/json"}); w.Code != 403 {
		t.Errorf("no X-Runlight: %d", w.Code)
	}
	if w := do(t, mux, "PUT", "/api/settings", body, map[string]string{"X-Runlight": "1", "Content-Type": "text/plain"}); w.Code != 415 {
		t.Errorf("bad content type: %d", w.Code)
	}
	if w := do(t, mux, "PUT", "/api/settings", strings.Repeat("x", 20000), okHdr); w.Code != 413 {
		t.Errorf("big body: %d", w.Code)
	}
}

func TestFeatureMocksPushAndAlerts(t *testing.T) {
	mux := http.NewServeMux()
	registerFeatureMocks(mux)
	sub := `{"subscription":{"endpoint":"https://fcm.googleapis.com/x/1","keys":{"p256dh":"a","auth":"b"}},"label":"Test"}`
	var d struct{ ID string }
	w := do(t, mux, "POST", "/api/push/subscribe", sub, okHdr)
	if w.Code != 200 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if strings.Contains(w.Body.String(), "fcm.googleapis.com") {
		t.Error("endpoint leaked")
	}
	if w := do(t, mux, "POST", "/api/push/unsubscribe", `{"id":"`+d.ID+`"}`, okHdr); w.Code != 204 {
		t.Errorf("unsubscribe: %d", w.Code)
	}
	if w := do(t, mux, "POST", "/api/push/subscribe", `{"subscription":{"endpoint":"http://x","keys":{}}}`, okHdr); w.Code != 400 {
		t.Errorf("bad subscription: %d", w.Code)
	}

	// Turning a rule off in settings must clear its active alert.
	do(t, mux, "PUT", "/api/settings", `{"changes":{"alert.disk_low.enabled":false}}`, okHdr)
	if w := do(t, mux, "GET", "/api/alerts", "", nil); strings.Contains(w.Body.String(), `"id":"disk_low","rule":"disk_low","severity":"critical","title":"Disco casi lleno","body":"Quedan`) {
		t.Errorf("disk_low still active: %s", w.Body)
	}
}
