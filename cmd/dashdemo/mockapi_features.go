package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file mocks the feature endpoints of the HTTP contract (docs/CONTRACT.md
// section 3) in memory: alerts, settings with real schema validation, simulated
// push devices and actions. It enforces the same guard rails as the real server
// on mutations (Origin, Content-Type, X-Runlight, body size) so the UI is
// exercised against them.

type mockField struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Help    string  `json:"help"`
	Group   string  `json:"group"`
	Unit    string  `json:"unit"`
	Kind    string  `json:"kind"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step"`
	Default any     `json:"default"`
}

type mockChange struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

type mockAudit struct {
	At      time.Time    `json:"at"`
	Actor   string       `json:"actor"`
	Changes []mockChange `json:"changes"`
}

type mockRule struct {
	ID, Title, Description, Severity, Unit, ThresholdLabel string
	Min, Max, Step, Default                                float64
}

var mockRules = []mockRule{
	{"disk_low", "Disco casi lleno", "Avisa cuando el espacio libre baja del umbral y se mantiene un minuto.", "critical", "%", "Disco libre mínimo", 3, 50, 1, 12},
	{"swap_full", "Swap llena", "El swap lleva diez minutos casi agotado: el servidor está paginando en exceso.", "warning", "%", "Swap usado desde", 50, 100, 1, 95},
	{"mem_low", "Memoria baja", "La memoria disponible cayó por debajo de la reserva multiplicada por el factor.", "warning", "× reserva", "Factor sobre la reserva", 1, 4, 0.1, 1.5},
	{"queue_stuck", "Cola atascada", "Hay jobs esperando runner durante demasiado tiempo.", "warning", "min", "Espera máxima", 2, 240, 1, 15},
	{"job_failures", "Fallos repetidos", "Varios jobs fallan seguidos en el mismo repositorio (30 min).", "warning", "fallos", "Fallos para avisar", 2, 20, 1, 3},
	{"prod_pressure", "Producción bajo presión", "La CPU de producción supera su límite de forma sostenida.", "warning", "min", "Minutos sostenidos", 1, 60, 1, 3},
	{"daemon_restart", "Reinicio inesperado", "runlight se reinició sin apagarse de forma limpia. Avisa una sola vez.", "critical", "", "", 0, 0, 0, 0},
	{"janitor_stuck", "Limpieza sin efecto", "Hay poco disco libre y el limpiador ya no libera nada.", "warning", "%", "Disco libre mínimo", 3, 50, 1, 12},
}

type mockDevice struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	UserAgent string     `json:"userAgent"`
	CreatedAt time.Time  `json:"createdAt"`
	LastOK    *time.Time `json:"lastOk,omitempty"`
	LastError string     `json:"lastError,omitempty"`
}

type featureMock struct {
	mu       sync.Mutex
	schema   []mockField
	byKey    map[string]mockField
	values   map[string]any
	audit    []mockAudit
	devices  []mockDevice
	paused   bool
	vapid    string
	started  time.Time
	cacheCap map[string]float64
}

func newFeatureMock() *featureMock {
	m := &featureMock{values: map[string]any{}, byKey: map[string]mockField{}, started: time.Now()}
	f := func(key, label, help, group, unit, kind string, min, max, step float64, def any) {
		fd := mockField{key, label, help, group, unit, kind, min, max, step, def}
		m.schema = append(m.schema, fd)
		m.byKey[key] = fd
		m.values[key] = def
	}
	const g1, g2, g3, g4 = "Escalado", "Presión", "Cachés", "Alertas"
	f("scaling.maxRunners", "Máximo de runners", "Tope de runners simultáneos aunque haya cola.", g1, "", "int", 1, 16, 1, 3)
	f("scaling.minRunners", "Runners en caliente", "Runners que se mantienen vivos aunque no haya trabajo.", g1, "", "int", 0, 8, 1, 0)
	f("scaling.memPerRunnerMb", "Memoria estimada por runner", "Lo que se presupone que consume cada runner al decidir si cabe otro.", g1, "MB", "int", 256, 16384, 128, 3072)
	f("scaling.memReserveMb", "Reserva de memoria intocable", "Memoria que runlight nunca toca para dejarla a producción.", g1, "MB", "int", 256, 16384, 128, 4096)
	f("scaling.diskMinFreeMb", "Disco libre mínimo", "Por debajo de esto no se arrancan runners nuevos.", g1, "MB", "int", 1024, 102400, 512, 8192)
	f("scaling.idleTtlSec", "Segundos antes de retirar un runner libre", "Un runner sin trabajo se apaga pasado este tiempo.", g1, "s", "int", 10, 3600, 1, 30)
	f("pressure.cpuHigh", "CPU de producción desde la que frena", "Si la CPU de producción supera este porcentaje, runlight reduce runners.", g2, "%", "float", 5, 95, 1, 25)
	f("pressure.memHigh", "Presión de memoria desde la que frena", "Porcentaje de tiempo con procesos esperando memoria.", g2, "%", "float", 1, 50, 1, 5)
	f("pressure.ioHigh", "Disco E/S desde el que frena", "Porcentaje de tiempo con procesos esperando disco.", g2, "%", "float", 5, 95, 1, 30)
	f("janitor.intervalMin", "Cada cuántos minutos revisa", "Frecuencia de la limpieza de cachés y espacio.", g3, "min", "int", 1, 240, 1, 15)
	m.cacheCap = map[string]float64{"docker-buildx": 40, "go-build": 8, "pnpm": 12}
	names := make([]string, 0, len(m.cacheCap))
	for n := range m.cacheCap {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f("cache."+n+".capGb", "Tope de "+n, "Cuando la caché supera este tamaño se recorta lo más antiguo.", g3, "GB", "float", 0.5, 200, 0.5, m.cacheCap[n])
	}
	for _, r := range mockRules {
		f("alert."+r.ID+".enabled", r.Title+": activa", r.Description, g4, "", "bool", 0, 0, 0, true)
		if r.Max > 0 {
			f("alert."+r.ID+".threshold", r.Title+": "+strings.ToLower(r.ThresholdLabel), r.Description, g4, r.Unit, "float", r.Min, r.Max, r.Step, r.Default)
		}
	}

	now := time.Now()
	ok := now.Add(-40 * time.Minute)
	ok2 := now.Add(-3 * time.Hour)
	m.devices = []mockDevice{
		{ID: "a1b2c3d4e5f6", Label: "Móvil de Danny", UserAgent: "Mozilla/5.0 (Linux; Android 15) Chrome/131 Mobile", CreatedAt: now.AddDate(0, 0, -12), LastOK: &ok},
		{ID: "0f9e8d7c6b5a", Label: "Portátil", UserAgent: "Mozilla/5.0 (Windows NT 10.0) Edg/131", CreatedAt: now.AddDate(0, 0, -30), LastOK: &ok2, LastError: "El servicio de push devolvió 500"},
	}
	m.audit = []mockAudit{{At: now.Add(-26 * time.Hour), Actor: "admin", Changes: []mockChange{{"scaling.maxRunners", float64(2), float64(3)}}}}

	if k, err := ecdh.P256().GenerateKey(rand.Reader); err == nil {
		m.vapid = base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
	}
	return m
}

func mockJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func mockErr(w http.ResponseWriter, code int, msg string, fields map[string]string) {
	body := map[string]any{"error": msg}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	mockJSON(w, code, body)
}

// guard applies the contract's rules for state-changing requests. It returns
// the decoded body (<= 16 KB) or false after writing the error response.
func guard(w http.ResponseWriter, r *http.Request, want string, out any) bool {
	if r.Method != want {
		w.Header().Set("Allow", want)
		mockErr(w, http.StatusMethodNotAllowed, "Método no permitido", nil)
		return false
	}
	if want == http.MethodGet {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "//"+r.Host) {
		mockErr(w, http.StatusForbidden, "Origen no permitido", nil)
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		mockErr(w, http.StatusUnsupportedMediaType, "Se esperaba application/json", nil)
		return false
	}
	if r.Header.Get("X-Runlight") != "1" {
		mockErr(w, http.StatusForbidden, "Falta la cabecera X-Runlight", nil)
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 16*1024+1))
	if err != nil || len(b) > 16*1024 {
		mockErr(w, http.StatusRequestEntityTooLarge, "Cuerpo demasiado grande", nil)
		return false
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			mockErr(w, http.StatusBadRequest, "JSON no válido", nil)
			return false
		}
	}
	return true
}

func fmtNum(v float64, step float64) string {
	if step > 0 && step < 1 || v != math.Trunc(v) {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
	}
	return fmt.Sprintf("%d", int64(v))
}

// validate mirrors internal/settings: unknown key, wrong type, non-finite or
// out-of-range values give a Spanish message per field. It returns the coerced
// value on success.
func (m *featureMock) validate(key string, raw any) (any, string) {
	fd, ok := m.byKey[key]
	if !ok {
		return nil, "Ajuste desconocido"
	}
	switch fd.Kind {
	case "bool":
		b, ok := raw.(bool)
		if !ok {
			return nil, "Debe ser sí o no"
		}
		return b, ""
	case "int", "float":
		n, ok := raw.(float64)
		if !ok {
			return nil, "Debe ser un número"
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, "Número no válido"
		}
		if fd.Kind == "int" && n != math.Trunc(n) {
			return nil, "Debe ser un número entero"
		}
		if n < fd.Min || n > fd.Max {
			return nil, fmt.Sprintf("Debe estar entre %s y %s %s", fmtNum(fd.Min, fd.Step), fmtNum(fd.Max, fd.Step), fd.Unit)
		}
		return n, ""
	}
	return nil, "Tipo no soportado"
}

func num(v any) float64 { f, _ := v.(float64); return f }

func (m *featureMock) settingsBody() map[string]any {
	defaults := map[string]any{}
	for _, f := range m.schema {
		defaults[f.Key] = f.Default
	}
	vals := map[string]any{}
	for k, v := range m.values {
		vals[k] = v
	}
	audit := append([]mockAudit(nil), m.audit...)
	return map[string]any{"values": vals, "schema": m.schema, "audit": audit, "defaults": defaults}
}

func (m *featureMock) alertsBody() map[string]any {
	now := time.Now()
	rules := make([]map[string]any, 0, len(mockRules))
	for _, r := range mockRules {
		thr := r.Default
		if v, ok := m.values["alert."+r.ID+".threshold"]; ok {
			thr = num(v)
		}
		rules = append(rules, map[string]any{
			"id": r.ID, "title": r.Title, "description": r.Description, "severity": r.Severity,
			"unit": r.Unit, "thresholdLabel": r.ThresholdLabel, "min": r.Min, "max": r.Max, "step": r.Step,
			"config": map[string]any{"enabled": m.values["alert."+r.ID+".enabled"] == true, "threshold": thr},
		})
	}
	active := []map[string]any{}
	if m.values["alert.queue_stuck.enabled"] == true {
		active = append(active, map[string]any{
			"id": "queue_stuck", "rule": "queue_stuck", "severity": "warning", "title": "Cola atascada",
			"body":  "Hay 6 jobs esperando desde hace 21 min. runlight tiene 3 de 3 runners ocupados y el límite lo marca la presión de CPU de producción.",
			"since": now.Add(-21 * time.Minute), "lastNotified": now.Add(-21 * time.Minute), "notifications": 1,
		})
	}
	if m.values["alert.disk_low.enabled"] == true {
		active = append(active, map[string]any{
			"id": "disk_low", "rule": "disk_low", "severity": "critical", "title": "Disco casi lleno",
			"body":  "Quedan 7.1 GB libres de 197 GB (3.6 %). runlight dejó de arrancar runners.",
			"since": now.Add(-95 * time.Minute), "lastNotified": now.Add(-35 * time.Minute), "notifications": 3,
		})
	}
	resolved := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	history := []map[string]any{
		{"id": "job_failures:asteby-hq/api", "rule": "job_failures", "severity": "warning", "title": "Fallos repetidos en asteby-hq/api",
			"body": "3 jobs fallaron en 30 min en asteby-hq/api (último: test).", "since": now.Add(-5 * time.Hour), "lastNotified": now.Add(-5 * time.Hour), "notifications": 1, "resolvedAt": resolved(4 * time.Hour)},
		{"id": "prod_pressure", "rule": "prod_pressure", "severity": "warning", "title": "Producción bajo presión",
			"body": "La CPU de producción llevó 4 min por encima del 25 %.", "since": now.Add(-27 * time.Hour), "lastNotified": now.Add(-27 * time.Hour), "notifications": 1, "resolvedAt": resolved(26*time.Hour + 30*time.Minute)},
		{"id": "daemon_restart", "rule": "daemon_restart", "severity": "critical", "title": "Reinicio inesperado",
			"body": "runlight se reinició sin apagarse de forma limpia.", "since": now.Add(-50 * time.Hour), "lastNotified": now.Add(-50 * time.Hour), "notifications": 1, "resolvedAt": resolved(49 * time.Hour)},
	}
	return map[string]any{"active": active, "history": history, "rules": rules}
}

func registerFeatureMocks(mux *http.ServeMux) {
	m := newFeatureMock()

	mux.HandleFunc("/api/alerts", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, http.MethodGet, nil) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		mockJSON(w, 200, m.alertsBody())
	})

	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			m.mu.Lock()
			defer m.mu.Unlock()
			mockJSON(w, 200, m.settingsBody())
			return
		}
		var req struct {
			Changes map[string]any `json:"changes"`
		}
		if !guard(w, r, http.MethodPut, &req) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		fields := map[string]string{}
		next := map[string]any{}
		for k, raw := range req.Changes {
			v, msg := m.validate(k, raw)
			if msg != "" {
				fields[k] = msg
				continue
			}
			next[k] = v
		}
		// Cross-field rule from the contract: minRunners <= maxRunners.
		get := func(k string) float64 {
			if v, ok := next[k]; ok {
				return num(v)
			}
			return num(m.values[k])
		}
		if _, bad := fields["scaling.minRunners"]; !bad && get("scaling.minRunners") > get("scaling.maxRunners") {
			if _, changed := next["scaling.minRunners"]; changed {
				fields["scaling.minRunners"] = "No puede superar el máximo de runners"
			} else {
				fields["scaling.maxRunners"] = "No puede ser menor que los runners en caliente"
			}
		}
		if len(fields) > 0 {
			mockErr(w, http.StatusBadRequest, "Hay ajustes no válidos; no se aplicó ninguno", fields)
			return
		}
		applied, unchanged := []string{}, []string{}
		var changes []mockChange
		keys := make([]string, 0, len(next))
		for k := range next {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if m.values[k] == next[k] {
				unchanged = append(unchanged, k)
				continue
			}
			changes = append(changes, mockChange{k, m.values[k], next[k]})
			m.values[k] = next[k]
			applied = append(applied, k)
		}
		if len(changes) > 0 {
			m.audit = append([]mockAudit{{At: time.Now(), Actor: "admin", Changes: changes}}, m.audit...)
			if len(m.audit) > 200 {
				m.audit = m.audit[:200]
			}
		}
		vals := map[string]any{}
		for k, v := range m.values {
			vals[k] = v
		}
		mockJSON(w, 200, map[string]any{"values": vals, "applied": applied, "unchanged": unchanged})
	})

	mux.HandleFunc("/api/actions/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !guard(w, r, http.MethodPost, &body) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/api/actions/") {
		case "pause":
			m.paused = true
			mockJSON(w, 200, map[string]any{"ok": true, "message": "runlight en pausa: no arrancará runners nuevos; los que están corriendo siguen."})
		case "resume":
			m.paused = false
			mockJSON(w, 200, map[string]any{"ok": true, "message": "runlight reanudado."})
		case "janitor-now":
			mockJSON(w, 200, map[string]any{"ok": true, "message": "Limpieza lanzada en segundo plano."})
		default:
			mockErr(w, http.StatusNotFound, "Acción desconocida", nil)
		}
	})

	mux.HandleFunc("/api/push/key", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, http.MethodGet, nil) {
			return
		}
		mockJSON(w, 200, map[string]any{"publicKey": m.vapid})
	})

	mux.HandleFunc("/api/push/devices", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, http.MethodGet, nil) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		mockJSON(w, 200, map[string]any{"devices": append([]mockDevice{}, m.devices...)})
	})

	mux.HandleFunc("/api/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Subscription struct {
				Endpoint string `json:"endpoint"`
				Keys     struct {
					P256dh string `json:"p256dh"`
					Auth   string `json:"auth"`
				} `json:"keys"`
			} `json:"subscription"`
			Label string `json:"label"`
		}
		if !guard(w, r, http.MethodPost, &req) {
			return
		}
		s := req.Subscription
		if !strings.HasPrefix(s.Endpoint, "https://") || s.Keys.P256dh == "" || s.Keys.Auth == "" {
			mockErr(w, http.StatusBadRequest, "Suscripción no válida", nil)
			return
		}
		sum := sha256.Sum256([]byte(s.Endpoint))
		id := hex.EncodeToString(sum[:])[:12]
		label := strings.TrimSpace(req.Label)
		if label == "" {
			label = "Dispositivo"
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.devices {
			if m.devices[i].ID == id {
				m.devices[i].Label = label
				mockJSON(w, 200, m.devices[i])
				return
			}
		}
		if len(m.devices) >= 20 {
			mockErr(w, http.StatusBadRequest, "Máximo de 20 dispositivos", nil)
			return
		}
		ua := r.UserAgent()
		if len(ua) > 160 {
			ua = ua[:160]
		}
		d := mockDevice{ID: id, Label: label, UserAgent: ua, CreatedAt: time.Now()}
		m.devices = append(m.devices, d)
		mockJSON(w, 200, d)
	})

	mux.HandleFunc("/api/push/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !guard(w, r, http.MethodPost, &req) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.devices {
			if m.devices[i].ID == req.ID {
				m.devices = append(m.devices[:i], m.devices[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		mockErr(w, http.StatusNotFound, "Dispositivo no encontrado", nil)
	})

	mux.HandleFunc("/api/push/test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if !guard(w, r, http.MethodPost, &req) {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		sent, failed := 0, 0
		now := time.Now()
		for i := range m.devices {
			if req.ID != "" && m.devices[i].ID != req.ID {
				continue
			}
			if strings.HasSuffix(m.devices[i].Label, "Portátil") {
				failed++
				m.devices[i].LastError = "El servicio de push devolvió 500"
				continue
			}
			sent++
			m.devices[i].LastOK, m.devices[i].LastError = &now, ""
		}
		if req.ID != "" && sent+failed == 0 {
			mockErr(w, http.StatusNotFound, "Dispositivo no encontrado", nil)
			return
		}
		mockJSON(w, 200, map[string]any{"sent": sent, "failed": failed, "removed": 0})
	})
}
