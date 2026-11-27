package dashboard

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dannyjp49/runlight/internal/alerts"
	"github.com/dannyjp49/runlight/internal/push"
	"github.com/dannyjp49/runlight/internal/settings"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

type fakeActions struct{ calls []string }

func (f *fakeActions) Pause() (string, error) {
	f.calls = append(f.calls, "pause")
	return "Pausado.", nil
}
func (f *fakeActions) Resume() (string, error) {
	f.calls = append(f.calls, "resume")
	return "Reanudado.", nil
}
func (f *fakeActions) JanitorNow() (string, error) {
	f.calls = append(f.calls, "janitor-now")
	return "", errors.New("No hay cachés configuradas")
}

type rig struct {
	srv     *httptest.Server
	client  *http.Client
	actions *fakeActions
	push    *push.Store
	dir     string
	setts   *settings.Manager
}

const secretEndpointToken = "SECRETENDPOINTTOKEN123"

// newRig wires every optional service. With auth it also logs in.
func newRig(t *testing.T, withAuth bool) *rig {
	t.Helper()
	dir := t.TempDir()
	ps, err := push.Open(dir, "https://example.test", push.WithAllowAnyEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	eng := alerts.New(alerts.DefaultConfig(), func(context.Context, alerts.Notification) {})
	fields := append(settings.StaticFields(settings.Defaults{
		MaxRunners: 4, MinRunners: 0, MemPerRunnerMB: 1024, MemReserveMB: 1024, DiskMinFreeMB: 4096,
		IdleTTLSec: 30, JanitorIntervalMin: 10, PSICPU: 60, PSIMem: 8, PSIIO: 50,
	}), settings.AlertFields(alerts.RuleCatalog())...)
	mgr, err := settings.Open(filepath.Join(dir, "settings.json"), fields)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{actions: &fakeActions{}, push: ps, dir: dir, setts: mgr}
	opts := []Option{WithPush(ps), WithAlerts(eng), WithSettings(mgr), WithActions(r.actions)}
	r.client = newClient(t)
	if withAuth {
		opts = append(opts, WithAuth(newAuth(t)))
	}
	r.srv = httptest.NewServer(Handler(telemetry.New(telemetry.Config{Version: "t"}), opts...))
	t.Cleanup(r.srv.Close)
	if withAuth {
		if resp := login(t, r.client, r.srv.URL, "admin", testPassword); resp.StatusCode != 204 {
			t.Fatalf("login = %d", resp.StatusCode)
		}
	}
	return r
}

type req struct {
	method, path, ctype, body string
	noHeader                  bool
	origin                    string
}

func (r *rig) do(t *testing.T, q req) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if q.body != "" {
		body = strings.NewReader(q.body)
	}
	hr, err := http.NewRequest(q.method, r.srv.URL+q.path, body)
	if err != nil {
		t.Fatal(err)
	}
	if q.ctype != "" {
		hr.Header.Set("Content-Type", q.ctype)
	}
	if !q.noHeader {
		hr.Header.Set("X-Runlight", "1")
	}
	if q.origin != "" {
		hr.Header.Set("Origin", q.origin)
	}
	resp, err := r.client.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func jsonReq(method, path, body string) req {
	return req{method: method, path: path, ctype: "application/json", body: body}
}

func validSubscription(t *testing.T) string {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	b, _ := json.Marshal(map[string]any{
		"subscription": map[string]any{
			"endpoint": "https://fcm.googleapis.com/fcm/send/" + secretEndpointToken,
			"keys": map[string]string{
				"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
				"auth":   base64.RawURLEncoding.EncodeToString(auth),
			},
		},
		"label": "Mi móvil",
	})
	return string(b)
}

var allRoutes = []req{
	{method: "GET", path: "/api/state"},
	{method: "GET", path: "/api/history"},
	{method: "GET", path: "/api/procs"},
	{method: "GET", path: "/metrics"},
	{method: "GET", path: "/api/push/key"},
	{method: "GET", path: "/api/push/devices"},
	{method: "POST", path: "/api/push/subscribe", body: "{}"},
	{method: "POST", path: "/api/push/unsubscribe", body: "{}"},
	{method: "POST", path: "/api/push/test", body: "{}"},
	{method: "GET", path: "/api/alerts"},
	{method: "GET", path: "/api/settings"},
	{method: "PUT", path: "/api/settings", body: `{"changes":{"scaling.maxRunners":3}}`},
	{method: "POST", path: "/api/actions/pause", body: "{}"},
	{method: "POST", path: "/api/actions/resume", body: "{}"},
	{method: "POST", path: "/api/actions/janitor-now", body: "{}"},
}

func TestEveryRouteWithoutSessionIs401(t *testing.T) {
	r := newRig(t, true)
	anon := newClient(t)
	for _, q := range allRoutes {
		hr, _ := http.NewRequest(q.method, r.srv.URL+q.path, strings.NewReader(q.body))
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("X-Runlight", "1")
		resp, err := anon.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s without session = %d, want 401", q.method, q.path, resp.StatusCode)
		}
		if !strings.Contains(string(b), "Sesión requerida") {
			t.Errorf("%s %s body = %q", q.method, q.path, b)
		}
	}
	if len(r.actions.calls) != 0 {
		t.Fatalf("actions ran without a session: %v", r.actions.calls)
	}
}

func TestSPAModulesNeedASession(t *testing.T) {
	r := newRig(t, true)
	anon := newClient(t)
	for _, p := range []string{"/", "/app.js", "/app.css", "/style.css-x", "/ui.css", "/ui-extra.css", "/index.html"} {
		resp, err := anon.Get(r.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s without session = %d -> %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestPWAShellIsPublic(t *testing.T) {
	r := newRig(t, true)
	anon := newClient(t)
	cases := map[string]string{
		"/manifest.webmanifest":       "application/manifest+json",
		"/sw.js":                      "text/javascript",
		"/offline.html":               "text/html",
		"/icons/icon-192.png":         "image/png",
		"/icons/apple-touch-icon.png": "image/png",
		"/logo-light.svg":             "image/svg+xml",
	}
	for p, ctype := range cases {
		resp, err := anon.Get(r.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s without session = %d", p, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, ctype) {
			t.Errorf("%s content-type = %q, want %q", p, got, ctype)
		}
	}
	resp, _ := anon.Get(r.srv.URL + "/sw.js")
	if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("Service-Worker-Allowed") != "/" {
		t.Errorf("sw.js headers: %v", resp.Header)
	}
	resp, _ = anon.Get(r.srv.URL + "/icons/icon-192.png")
	if !strings.Contains(resp.Header.Get("Cache-Control"), "max-age") {
		t.Errorf("icons not cacheable: %v", resp.Header)
	}
	// No directory listing of a public directory.
	resp, _ = anon.Get(r.srv.URL + "/icons/")
	if resp.StatusCode == 200 {
		t.Error("/icons/ lists the directory")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"worker-src 'self'", "manifest-src 'self'", "default-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
}

func TestCSRFGuard(t *testing.T) {
	r := newRig(t, true)
	good := jsonReq("PUT", "/api/settings", `{"changes":{"scaling.maxRunners":3}}`)

	t.Run("missing X-Runlight", func(t *testing.T) {
		q := good
		q.noHeader = true
		if resp, _ := r.do(t, q); resp.StatusCode != 403 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("foreign origin", func(t *testing.T) {
		q := good
		q.origin = "https://evil.example"
		if resp, _ := r.do(t, q); resp.StatusCode != 403 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("same origin is fine", func(t *testing.T) {
		q := good
		q.origin = r.srv.URL
		if resp, body := r.do(t, q); resp.StatusCode != 200 {
			t.Fatalf("status %d %s", resp.StatusCode, body)
		}
	})
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		t.Run("content-type "+ct, func(t *testing.T) {
			q := good
			q.ctype = ct
			if resp, _ := r.do(t, q); resp.StatusCode != 415 {
				t.Fatalf("status %d", resp.StatusCode)
			}
		})
	}
	t.Run("json with charset is fine", func(t *testing.T) {
		q := good
		q.ctype = "application/json; charset=utf-8"
		if resp, body := r.do(t, q); resp.StatusCode != 200 {
			t.Fatalf("status %d %s", resp.StatusCode, body)
		}
	})
	t.Run("body too large", func(t *testing.T) {
		big := `{"changes":{"scaling.maxRunners":3},"pad":"` + strings.Repeat("a", 17<<10) + `"}`
		if resp, _ := r.do(t, jsonReq("PUT", "/api/settings", big)); resp.StatusCode != 413 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("chunked body too large", func(t *testing.T) {
		big := `{"changes":{"scaling.maxRunners":3},"pad":"` + strings.Repeat("a", 17<<10) + `"}`
		hr, _ := http.NewRequest("PUT", r.srv.URL+"/api/settings", io.NopCloser(strings.NewReader(big))) // unknown length
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("X-Runlight", "1")
		resp, err := r.client.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		if resp, _ := r.do(t, jsonReq("PUT", "/api/settings", `{`)); resp.StatusCode != 400 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("every mutating route is guarded", func(t *testing.T) {
		for _, q := range allRoutes {
			if q.method == "GET" {
				continue
			}
			q.noHeader = true
			q.ctype = "application/json"
			if resp, _ := r.do(t, q); resp.StatusCode != 403 {
				t.Errorf("%s %s without X-Runlight = %d", q.method, q.path, resp.StatusCode)
			}
		}
		if len(r.actions.calls) != 0 {
			t.Fatalf("an action ran through a rejected request: %v", r.actions.calls)
		}
	})
}

func TestSettingsPutValidAndInvalid(t *testing.T) {
	r := newRig(t, true)

	resp, body := r.do(t, jsonReq("PUT", "/api/settings", `{"changes":{"scaling.maxRunners":3,"pressure.cpuHigh":55.5}}`))
	if resp.StatusCode != 200 {
		t.Fatalf("valid PUT = %d %s", resp.StatusCode, body)
	}
	var ok struct {
		Values    map[string]any `json:"values"`
		Applied   []string       `json:"applied"`
		Unchanged []string       `json:"unchanged"`
	}
	if err := json.Unmarshal([]byte(body), &ok); err != nil {
		t.Fatal(err)
	}
	if len(ok.Applied) != 2 || ok.Values["scaling.maxRunners"] != float64(3) {
		t.Fatalf("response = %s", body)
	}
	if got := r.setts.Values()["scaling.maxRunners"]; got != 3 {
		t.Fatalf("not persisted: %v", got)
	}
	if a := r.setts.Audit(); len(a) != 1 || a[0].Actor != "admin" {
		t.Fatalf("audit = %+v", a)
	}

	// Same value again: unchanged, and the key must be listed.
	_, body = r.do(t, jsonReq("PUT", "/api/settings", `{"changes":{"scaling.maxRunners":3}}`))
	if !strings.Contains(body, `"unchanged":["scaling.maxRunners"]`) {
		t.Fatalf("unchanged missing: %s", body)
	}

	// Invalid: nothing may be applied, per-field errors are reported.
	resp, body = r.do(t, jsonReq("PUT", "/api/settings", `{"changes":{"scaling.maxRunners":2,"pressure.cpuHigh":500,"nope.key":1,"scaling.minRunners":"x"}}`))
	if resp.StatusCode != 400 {
		t.Fatalf("invalid PUT = %d %s", resp.StatusCode, body)
	}
	var bad struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(body), &bad); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pressure.cpuHigh", "nope.key", "scaling.minRunners"} {
		if bad.Fields[k] == "" {
			t.Errorf("no field error for %s: %s", k, body)
		}
	}
	if bad.Error == "" {
		t.Error("no top-level error")
	}
	if got := r.setts.Values()["scaling.maxRunners"]; got != 3 {
		t.Fatalf("partial apply: maxRunners = %v", got)
	}

	for _, b := range []string{`{}`, `{"changes":{}}`, `{"changes":[1]}`} {
		if resp, _ := r.do(t, jsonReq("PUT", "/api/settings", b)); resp.StatusCode != 400 {
			t.Errorf("PUT %s = %d, want 400", b, resp.StatusCode)
		}
	}

	resp, body = r.do(t, req{method: "GET", path: "/api/settings"})
	var got struct {
		Values   map[string]any   `json:"values"`
		Schema   []map[string]any `json:"schema"`
		Audit    []any            `json:"audit"`
		Defaults map[string]any   `json:"defaults"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET settings: %v %s", err, body)
	}
	if len(got.Schema) == 0 || len(got.Values) == 0 || got.Defaults["scaling.maxRunners"] != float64(4) || len(got.Audit) != 1 {
		t.Fatalf("GET settings = %s", body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("settings cacheable")
	}
}

func TestActionsAndPushEndpoints(t *testing.T) {
	r := newRig(t, true)

	resp, body := r.do(t, jsonReq("POST", "/api/actions/pause", `{}`))
	if resp.StatusCode != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("pause = %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.do(t, jsonReq("POST", "/api/actions/resume", `{}`)); resp.StatusCode != 200 {
		t.Fatalf("resume = %d", resp.StatusCode)
	}
	if resp, body := r.do(t, jsonReq("POST", "/api/actions/janitor-now", `{}`)); resp.StatusCode != 409 || !strings.Contains(body, "cachés") {
		t.Fatalf("janitor-now = %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.do(t, jsonReq("POST", "/api/actions/explode", `{}`)); resp.StatusCode != 404 {
		t.Fatalf("unknown action = %d", resp.StatusCode)
	}
	if want := "pause,resume,janitor-now"; strings.Join(r.actions.calls, ",") != want {
		t.Fatalf("calls = %v", r.actions.calls)
	}

	if resp, body := r.do(t, req{method: "GET", path: "/api/push/key"}); resp.StatusCode != 200 || !strings.Contains(body, `"publicKey":"`) {
		t.Fatalf("key = %d %s", resp.StatusCode, body)
	}
	resp, body = r.do(t, jsonReq("POST", "/api/push/subscribe", validSubscription(t)))
	if resp.StatusCode != 200 {
		t.Fatalf("subscribe = %d %s", resp.StatusCode, body)
	}
	var dev push.Device
	if err := json.Unmarshal([]byte(body), &dev); err != nil || dev.ID == "" || dev.Label != "Mi móvil" {
		t.Fatalf("device = %s", body)
	}
	if resp, body := r.do(t, jsonReq("POST", "/api/push/subscribe", `{"subscription":{"endpoint":"http://169.254.169.254/x","keys":{}}}`)); resp.StatusCode != 400 {
		t.Fatalf("bad subscribe = %d %s", resp.StatusCode, body)
	}
	if _, body := r.do(t, req{method: "GET", path: "/api/push/devices"}); !strings.Contains(body, dev.ID) {
		t.Fatalf("devices = %s", body)
	}
	if resp, _ := r.do(t, jsonReq("POST", "/api/push/test", `{"id":"nonexistent"}`)); resp.StatusCode != 404 {
		t.Fatalf("test unknown device = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, jsonReq("POST", "/api/push/unsubscribe", `{"id":"`+dev.ID+`"}`)); resp.StatusCode != 204 {
		t.Fatalf("unsubscribe = %d", resp.StatusCode)
	}
	if _, body := r.do(t, req{method: "GET", path: "/api/push/devices"}); !strings.Contains(body, `"devices":[]`) {
		t.Fatalf("devices after unsubscribe = %s", body)
	}
	if resp, body := r.do(t, req{method: "GET", path: "/api/alerts"}); resp.StatusCode != 200 || !strings.Contains(body, `"active":[]`) || !strings.Contains(body, `"rules":[{`) {
		t.Fatalf("alerts = %d %s", resp.StatusCode, body)
	}
	if _, body := r.do(t, req{method: "GET", path: "/api/state"}); !strings.Contains(body, `"paused":false`) || !strings.Contains(body, `"alerts":{"active":0,"critical":0}`) {
		t.Fatalf("state = %.300s", body)
	}
}

func TestNoSecretEverLeaks(t *testing.T) {
	r := newRig(t, true)
	if resp, body := r.do(t, jsonReq("POST", "/api/push/subscribe", validSubscription(t))); resp.StatusCode != 200 {
		t.Fatalf("subscribe = %d %s", resp.StatusCode, body)
	}
	r.do(t, jsonReq("PUT", "/api/settings", `{"changes":{"scaling.maxRunners":3}}`))

	var vapid struct {
		Private string `json:"private"`
	}
	raw, err := os.ReadFile(filepath.Join(r.dir, "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &vapid)
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	secrets := []string{secretEndpointToken, "pbkdf2-sha256", "0123456789abcdef0123456789abcdef"}
	for k, v := range generic {
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(k), "priv") && len(s) > 8 {
			secrets = append(secrets, s)
		}
	}
	if len(secrets) < 4 {
		t.Fatalf("could not find the VAPID private key in %s", raw)
	}

	for _, q := range allRoutes {
		if q.method != "GET" {
			continue
		}
		resp, body := r.do(t, q)
		for _, s := range secrets {
			if strings.Contains(body, s) {
				t.Errorf("%s leaks %q", q.path, s)
			}
		}
		for k, vs := range resp.Header {
			for _, v := range vs {
				for _, s := range secrets {
					if strings.Contains(v, s) && k != "Set-Cookie" {
						t.Errorf("%s header %s leaks a secret", q.path, k)
					}
				}
			}
		}
	}
	// The public key is public; the endpoint must appear nowhere, not even partially.
	_, body := r.do(t, req{method: "GET", path: "/api/push/devices"})
	if strings.Contains(body, "fcm.googleapis.com") || strings.Contains(body, "p256dh") || strings.Contains(body, "auth") {
		t.Errorf("devices expose subscription data: %s", body)
	}
}

func TestOptionalServicesAbsent(t *testing.T) {
	srv := httptest.NewServer(Handler(telemetry.New(telemetry.Config{Version: "t"})))
	defer srv.Close()
	for _, q := range allRoutes {
		if !strings.HasPrefix(q.path, "/api/push") && !strings.HasPrefix(q.path, "/api/alerts") &&
			!strings.HasPrefix(q.path, "/api/settings") && !strings.HasPrefix(q.path, "/api/actions") {
			continue
		}
		hr, _ := http.NewRequest(q.method, srv.URL+q.path, bytes.NewReader([]byte(q.body)))
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("X-Runlight", "1")
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 503 {
			t.Errorf("%s %s without service = %d, want 503", q.method, q.path, resp.StatusCode)
		}
	}
}
