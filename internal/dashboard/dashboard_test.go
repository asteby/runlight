package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

func newServer(t *testing.T) (*httptest.Server, *telemetry.Hub) {
	t.Helper()
	hub := telemetry.New(telemetry.Config{Version: "test", ScaleSet: "runlight"})
	srv := httptest.NewServer(Handler(hub))
	t.Cleanup(srv.Close)
	return srv, hub
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestServesTheUIWithStrictHeaders(t *testing.T) {
	srv, _ := newServer(t)
	resp, body := get(t, srv.URL+"/")
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>runlight") {
		t.Fatalf("status=%d body=%.80q", resp.StatusCode, body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline' ") && strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("weak CSP: %s", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing hardening headers")
	}
	for _, f := range []string{"/app.js", "/style.css", "/ui.css", "/logo-light.svg", "/logo-dark.svg"} {
		if r, _ := get(t, srv.URL+f); r.StatusCode != 200 {
			t.Fatalf("%s -> %d", f, r.StatusCode)
		}
	}
}

func TestStateIsValidJSONWithoutCaching(t *testing.T) {
	srv, hub := newServer(t)
	hub.Event("capacity", "Capacidad 1 → 2")
	hub.AddPoint(telemetry.Point{T: time.Now().UnixMilli(), Limit: 2})

	resp, body := get(t, srv.URL+"/api/state")
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("state must not be cached")
	}
	var st telemetry.State
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, body)
	}
	if st.Config.Version != "test" || st.Point == nil || st.Point.Limit != 2 || len(st.Events) != 1 {
		t.Fatalf("state = %+v", st)
	}
}

func TestHistoryWindow(t *testing.T) {
	srv, hub := newServer(t)
	now := time.Now()
	hub.AddPoint(telemetry.Point{T: now.Add(-40 * time.Minute).UnixMilli()})
	hub.AddPoint(telemetry.Point{T: now.Add(-5 * time.Minute).UnixMilli()})

	_, body := get(t, srv.URL+"/api/history?minutes=10")
	var out struct{ Points []telemetry.Point }
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Points) != 1 {
		t.Fatalf("err=%v points=%d", err, len(out.Points))
	}
	_, body = get(t, srv.URL+"/api/history?minutes=999") // out of range falls back to the default
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Points) != 1 {
		t.Fatalf("bad fallback: err=%v points=%d", err, len(out.Points))
	}
}

func TestMetricsAreExposed(t *testing.T) {
	srv, hub := newServer(t)
	hub.AddPoint(telemetry.Point{T: 1, Limit: 3, Busy: 2, ProdCPU: 7.5})
	_, body := get(t, srv.URL+"/metrics")
	for _, want := range []string{"runlight_limit 3", "runlight_runners_busy 2", "runlight_prod_cpu_pressure_percent 7.5"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	srv, _ := newServer(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL+"/api/state", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("%s must be rejected, got %d", m, resp.StatusCode)
		}
	}
}

func TestParseTopTakesTheLiveFrame(t *testing.T) {
	out := `top - 23:50:01 up 65 days
Tasks: 400 total
    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
      1 root      20   0  100000  10000   5000 S 99.0   0.1   1:00.00 oldframe
top - 23:50:02 up 65 days
Tasks: 400 total
    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
2672213 ubuntu    30  10  1.5g   392740  12000 R 183.0   1.6   0:05.00 compile
 627808 ubuntu    20   0  2.0g    1.1g   30000 S   2.9   4.8  12:00.00 tienda-main
   4242 ubuntu    20   0   10000   2000   1000 R   0.3   0.0   0:00.01 top
   9999 root      20   0   10000   53m    1000 S   0.0   0.0   0:00.01 my proc name
`
	procs := parseTop(out)
	if len(procs) != 3 {
		t.Fatalf("got %d procs: %+v", len(procs), procs)
	}
	if procs[0].Name != "compile" || procs[0].CPU != 183 {
		t.Fatalf("first = %+v", procs[0])
	}
	if procs[1].RSSMB < 1126 || procs[1].RSSMB > 1127 { // 1.1g = 1153433.6 KiB ≈ 1126.4 MB
		t.Fatalf("1.1g parsed as %.1f MB", procs[1].RSSMB)
	}
	if procs[2].Name != "my proc name" || procs[2].RSSMB != 53 {
		t.Fatalf("third = %+v", procs[2])
	}
	for _, p := range procs {
		if p.Name == "top" || p.Name == "oldframe" {
			t.Fatalf("must skip the sampler itself and stale frames: %+v", p)
		}
	}
}

func TestTopNOrdersAndCaps(t *testing.T) {
	all := []Proc{{PID: 1, CPU: 5}, {PID: 2, CPU: 50}, {PID: 3, CPU: 20}}
	got := topN(all, 2, func(a, b Proc) bool { return a.CPU > b.CPU })
	if len(got) != 2 || got[0].PID != 2 || got[1].PID != 3 {
		t.Fatalf("got %+v", got)
	}
	if all[0].PID != 1 {
		t.Fatal("topN must not reorder its input")
	}
}
