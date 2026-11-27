// Package dashboard serves Runlight's web UI and JSON API. It holds
// no state of its own: everything comes from the telemetry hub.
package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

//go:embed web
var webFS embed.FS

// Option customises Handler.
type Option func(*options)

type options struct {
	auth     *Auth
	push     PushService
	alerts   AlertsService
	settings SettingsService
	actions  Actions
}

// WithAuth puts the whole dashboard behind a login page.
func WithAuth(a *Auth) Option { return func(o *options) { o.auth = a } }

// Handler returns the dashboard. The state-changing endpoints (settings, push,
// actions) answer 503 until enabled with WithPush/WithSettings/WithActions and
// always sit behind the CSRF guard. With WithAuth, every page and API call
// except the login screen and the PWA shell needs a session.
func Handler(h *telemetry.Hub, opts ...Option) http.Handler {
	var cfg options
	for _, o := range opts {
		o(&cfg)
	}
	static, _ := fs.Sub(webFS, "web")
	procs := &procSampler{ttl: 4 * time.Second}
	mux := http.NewServeMux()

	mux.Handle("GET /", staticHeaders(http.FileServerFS(static)))
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if cfg.auth == nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if _, ok := cfg.auth.session(r); ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		page, err := fs.ReadFile(static, "login.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	})
	if cfg.auth != nil {
		mux.HandleFunc("POST /api/login", cfg.auth.login)
		mux.HandleFunc("POST /api/logout", cfg.auth.logout)
	}
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		if cfg.auth == nil {
			writeJSON(w, map[string]any{"auth": false})
			return
		}
		user, _ := cfg.auth.session(r)
		writeJSON(w, map[string]any{"auth": true, "user": user})
	})
	cfg.registerAPI(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, stateOut{State: h.Snapshot(), Alerts: cfg.countAlerts()})
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
		if minutes <= 0 || minutes > 60 {
			minutes = 20
		}
		since := time.Now().Add(-time.Duration(minutes) * time.Minute).UnixMilli()
		writeJSON(w, map[string]any{"points": h.History(since)})
	})
	mux.HandleFunc("GET /api/procs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, procs.get(r.Context()))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		st := h.Snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		gauge := func(name, help string, v float64) {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
		}
		if p := st.Point; p != nil {
			gauge("runlight_limit", "Concurrency ceiling chosen by the governor.", float64(p.Limit))
			gauge("runlight_runners", "Live runners.", float64(p.Runners))
			gauge("runlight_runners_busy", "Runners executing a job.", float64(p.Busy))
			gauge("runlight_jobs_queued", "Jobs waiting for a runner.", float64(p.Queued))
			gauge("runlight_prod_cpu_pressure_percent", "Production CPU stall (PSI avg10).", p.ProdCPU)
			gauge("runlight_prod_memory_pressure_percent", "Production memory stall (PSI avg10).", p.ProdMem)
			gauge("runlight_memory_available_mb", "Memory available to everyone.", float64(p.MemAvailMB))
		}
		gauge("runlight_jobs_succeeded_total", "Jobs succeeded since start.", float64(st.Totals.Succeeded))
		gauge("runlight_jobs_failed_total", "Jobs failed since start.", float64(st.Totals.Failed))
	})
	if cfg.auth != nil {
		return secure(cfg.auth.middleware(mux))
	}
	return secure(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// secure adds strict headers. The UI loads only its own files, and renders
// GitHub-supplied text (repo and job names) with textContent, never as HTML.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'; worker-src 'self'; manifest-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
