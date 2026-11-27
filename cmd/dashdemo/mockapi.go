package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

// withMocks puts the in-memory feature endpoints (alerts, settings, push
// devices, actions: see mockapi_features.go) in front of the real dashboard
// handler. state, history and procs stay real: they come from the synthetic
// telemetry hub driven through the actual governor.
//
// The mock routes are protected the same way as the real ones: when the
// dashboard has a login, a request without a valid session gets the inner
// handler's own 401, so the UI is exercised against the real auth flow.
func withMocks(inner http.Handler) http.Handler {
	mux := http.NewServeMux()
	registerFeatureMocks(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/state" && r.Method == http.MethodGet {
			serveStateWithMemory(inner, w, r)
			return
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			inner.ServeHTTP(w, r)
			return
		}
		if !hasSession(inner, r) {
			inner.ServeHTTP(w, r) // lets the real handler answer 401 / redirect
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// hasSession asks the real handler whether this request is allowed to reach
// /api/me (which needs a session whenever auth is on).
func hasSession(inner http.Handler, r *http.Request) bool {
	probe := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	if c := r.Header.Get("Cookie"); c != "" {
		probe.Header.Set("Cookie", c)
	}
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, probe)
	return rec.Code == http.StatusOK
}

// demoMemory is the real VPS case from the swap investigation: swap 6/6 GB full
// with zero swap activity (idle), /tmp as a 12 GB tmpfs holding 6.2 GB of CI junk.
var demoMemory = map[string]any{
	"swapUsedMb": 6144, "swapTotalMb": 6144, "swapInKbs": 0, "swapOutKbs": 0,
	"tmpfs": []map[string]any{
		{"path": "/tmp", "usedMb": 6350, "totalMb": 12288},
		{"path": "/dev/shm", "usedMb": 12, "totalMb": 11731},
	},
	"topSwap": []map[string]any{
		{"pid": 1423, "name": "mariadbd", "mb": 2310, "unit": "mariadb.service"},
		{"pid": 2210, "name": "tienda-main", "mb": 1480, "unit": "tienda-main.service"},
		{"pid": 3871, "name": "node", "mb": 940, "unit": "pm2-ubuntu.service"},
		{"pid": 912, "name": "snapd", "mb": 210, "unit": "snapd.service"},
		{"pid": 1, "name": "systemd", "mb": 64, "unit": "init.scope"},
	},
	"verdict": "idle",
	"note":    "Swap ocupado pero inactivo: son páginas viejas que nadie usa; no hay impacto. /tmp (en RAM) guarda 6.2 GB de archivos temporales.",
}

// serveStateWithMemory serves the real /api/state and, when the telemetry hub
// does not provide "memory" itself, adds the demo block so the UI card has data.
func serveStateWithMemory(inner http.Handler, w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	var st map[string]json.RawMessage
	if rec.Code == http.StatusOK && json.Unmarshal(body, &st) == nil {
		if _, ok := st["memory"]; !ok {
			if mem, err := json.Marshal(demoMemory); err == nil {
				st["memory"] = mem
				if out, err := json.Marshal(st); err == nil {
					body = out
				}
			}
		}
	}
	for k, v := range rec.Header() {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}
