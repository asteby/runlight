package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/dannyjp49/runlight/internal/alerts"
	"github.com/dannyjp49/runlight/internal/push"
	"github.com/dannyjp49/runlight/internal/settings"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

// maxBody is the largest request body a mutating endpoint accepts.
const maxBody = 16 << 10

// PushService is what the dashboard needs from the push store.
type PushService interface {
	PublicKey() string
	Subscribe(sub push.Subscription, label, userAgent string) (push.Device, error)
	Unsubscribe(id string) error
	Devices() []push.Device
	Send(ctx context.Context, n push.Notification) push.Report
	SendTo(ctx context.Context, id string, n push.Notification) (push.Report, error)
}

// AlertsService exposes the alert engine's state.
type AlertsService interface {
	Snapshot() alerts.Snapshot
}

// SettingsService is the editable, persisted configuration.
type SettingsService interface {
	Schema() []settings.Field
	Values() map[string]any
	Defaults() map[string]any
	Audit() []settings.AuditEntry
	Update(actor string, changes map[string]any) (settings.Result, error)
}

// Actions are the operator buttons. Each returns the message shown to the user.
type Actions interface {
	Pause() (string, error)
	Resume() (string, error)
	JanitorNow() (string, error)
}

// WithPush enables the /api/push/* endpoints.
func WithPush(p PushService) Option { return func(o *options) { o.push = p } }

// WithAlerts enables /api/alerts and the alert counters of /api/state.
func WithAlerts(a AlertsService) Option { return func(o *options) { o.alerts = a } }

// WithSettings enables /api/settings.
func WithSettings(s SettingsService) Option { return func(o *options) { o.settings = s } }

// WithActions enables /api/actions/*.
func WithActions(a Actions) Option { return func(o *options) { o.actions = a } }

type stateOut struct {
	telemetry.State
	Alerts alertCount `json:"alerts"`
}

type alertCount struct {
	Active   int `json:"active"`
	Critical int `json:"critical"`
}

// decodeBody parses the (already size-limited) JSON body into v. An empty
// body is accepted as {}.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			jsonErr(w, http.StatusRequestEntityTooLarge, "La solicitud es demasiado grande")
		} else {
			jsonErr(w, http.StatusBadRequest, "Solicitud no válida")
		}
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	if err := json.Unmarshal(data, v); err != nil {
		jsonErr(w, http.StatusBadRequest, "Solicitud no válida")
		return false
	}
	return true
}

// guard protects state-changing endpoints: same-origin, an explicit
// X-Runlight header (a cross-site form cannot set it), a JSON content type
// and a small body.
func guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			jsonErr(w, http.StatusForbidden, "Origen no permitido")
			return
		}
		if r.Header.Get("X-Runlight") != "1" {
			jsonErr(w, http.StatusForbidden, "Falta la cabecera X-Runlight")
			return
		}
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			jsonErr(w, http.StatusUnsupportedMediaType, "El tipo de contenido debe ser application/json")
			return
		}
		if r.ContentLength > maxBody {
			jsonErr(w, http.StatusRequestEntityTooLarge, "La solicitud es demasiado grande")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	}
}

func unavailable(w http.ResponseWriter, what string) {
	jsonErr(w, http.StatusServiceUnavailable, what+" no está disponible en este servidor")
}

func (cfg *options) actor(r *http.Request) string {
	if cfg.auth != nil {
		if u, ok := cfg.auth.session(r); ok {
			return u
		}
	}
	return "local"
}

func (cfg *options) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/push/key", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.push == nil {
			unavailable(w, "Las notificaciones")
			return
		}
		writeJSON(w, map[string]string{"publicKey": cfg.push.PublicKey()})
	})
	mux.HandleFunc("GET /api/push/devices", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.push == nil {
			unavailable(w, "Las notificaciones")
			return
		}
		devs := cfg.push.Devices()
		if devs == nil {
			devs = []push.Device{}
		}
		writeJSON(w, map[string]any{"devices": devs})
	})
	mux.HandleFunc("POST /api/push/subscribe", guard(func(w http.ResponseWriter, r *http.Request) {
		if cfg.push == nil {
			unavailable(w, "Las notificaciones")
			return
		}
		var body struct {
			Subscription struct {
				Endpoint string `json:"endpoint"`
				Keys     struct {
					P256dh string `json:"p256dh"`
					Auth   string `json:"auth"`
				} `json:"keys"`
			} `json:"subscription"`
			Label string `json:"label"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		dev, err := cfg.push.Subscribe(push.Subscription{
			Endpoint: body.Subscription.Endpoint, P256dh: body.Subscription.Keys.P256dh, Auth: body.Subscription.Keys.Auth,
		}, body.Label, r.UserAgent())
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, dev)
	}))
	mux.HandleFunc("POST /api/push/unsubscribe", guard(func(w http.ResponseWriter, r *http.Request) {
		if cfg.push == nil {
			unavailable(w, "Las notificaciones")
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if body.ID == "" {
			jsonErr(w, http.StatusBadRequest, "Falta el id del dispositivo")
			return
		}
		if err := cfg.push.Unsubscribe(body.ID); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/push/test", guard(func(w http.ResponseWriter, r *http.Request) {
		if cfg.push == nil {
			unavailable(w, "Las notificaciones")
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		n := push.Notification{
			Title: "runlight", Body: "Notificación de prueba: si la ves, los avisos funcionan en este dispositivo.",
			Tag: "runlight-test", URL: "/#/alertas", Level: "info",
		}
		var rep push.Report
		if body.ID != "" {
			var err error
			if rep, err = cfg.push.SendTo(r.Context(), body.ID, n); err != nil {
				jsonErr(w, http.StatusNotFound, err.Error())
				return
			}
		} else {
			rep = cfg.push.Send(r.Context(), n)
		}
		writeJSON(w, rep)
	}))

	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.alerts == nil {
			unavailable(w, "Los avisos")
			return
		}
		writeJSON(w, normalizeSnapshot(cfg.alerts.Snapshot()))
	})

	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.settings == nil {
			unavailable(w, "La configuración")
			return
		}
		audit := cfg.settings.Audit()
		if audit == nil {
			audit = []settings.AuditEntry{}
		}
		writeJSON(w, map[string]any{
			"values": cfg.settings.Values(), "schema": cfg.settings.Schema(),
			"audit": audit, "defaults": cfg.settings.Defaults(),
		})
	})
	mux.HandleFunc("PUT /api/settings", guard(func(w http.ResponseWriter, r *http.Request) {
		if cfg.settings == nil {
			unavailable(w, "La configuración")
			return
		}
		var body struct {
			Changes map[string]any `json:"changes"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if len(body.Changes) == 0 {
			jsonErr(w, http.StatusBadRequest, "No hay cambios que aplicar")
			return
		}
		res, err := cfg.settings.Update(cfg.actor(r), body.Changes)
		if err != nil {
			var fe settings.FieldErrors
			if errors.As(err, &fe) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "Hay valores no válidos", "fields": map[string]string(fe)})
				return
			}
			jsonErr(w, http.StatusInternalServerError, "No se pudo guardar la configuración")
			return
		}
		if res.Applied == nil {
			res.Applied = []string{}
		}
		if res.Unchanged == nil {
			res.Unchanged = []string{}
		}
		writeJSON(w, map[string]any{"values": res.Values, "applied": res.Applied, "unchanged": res.Unchanged})
	}))

	mux.HandleFunc("POST /api/actions/{name}", guard(func(w http.ResponseWriter, r *http.Request) {
		if cfg.actions == nil {
			unavailable(w, "Las acciones")
			return
		}
		var body map[string]any
		if !decodeBody(w, r, &body) {
			return
		}
		var (
			msg string
			err error
		)
		switch r.PathValue("name") {
		case "pause":
			msg, err = cfg.actions.Pause()
		case "resume":
			msg, err = cfg.actions.Resume()
		case "janitor-now":
			msg, err = cfg.actions.JanitorNow()
		default:
			jsonErr(w, http.StatusNotFound, "Acción desconocida")
			return
		}
		if err != nil {
			jsonErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": msg})
	}))
}

func normalizeSnapshot(s alerts.Snapshot) alerts.Snapshot {
	if s.Active == nil {
		s.Active = []alerts.Alert{}
	}
	if s.History == nil {
		s.History = []alerts.Alert{}
	}
	if s.Rules == nil {
		s.Rules = []alerts.RuleInfo{}
	}
	return s
}

func (cfg *options) countAlerts() alertCount {
	var c alertCount
	if cfg.alerts == nil {
		return c
	}
	for _, a := range cfg.alerts.Snapshot().Active {
		c.Active++
		if a.Severity == alerts.Critical {
			c.Critical++
		}
	}
	return c
}

// staticHeaders sets the right MIME type and caching for the embedded files.
func staticHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/" && strings.HasSuffix(p, "/") { // no directory listings
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		switch {
		case strings.HasSuffix(p, ".webmanifest"):
			h.Set("Content-Type", "application/manifest+json")
		case strings.HasSuffix(p, ".js"):
			h.Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(p, ".css"):
			h.Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(p, ".svg"):
			h.Set("Content-Type", "image/svg+xml")
		}
		switch {
		case p == "/sw.js":
			h.Set("Cache-Control", "no-cache")
			h.Set("Service-Worker-Allowed", "/")
		case strings.HasPrefix(p, "/icons/") || strings.HasPrefix(p, "/fonts/"):
			h.Set("Cache-Control", "public, max-age=86400")
		default:
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}
