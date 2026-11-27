package main

import (
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
