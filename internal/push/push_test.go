package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newSub builds a valid browser subscription (P-256 key, 16-byte auth).
func newSub(t *testing.T, endpoint string) Subscription {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return Subscription{
		Endpoint: endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}
}

func openTest(t *testing.T, dir string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(dir, "https://runlight.example", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenCreatesPersistentVAPID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := openTest(t, dir)
	key := s.PublicKey()
	pub, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(pub) != 65 {
		t.Fatalf("public key invalid: %q (%v)", key, err)
	}
	s2 := openTest(t, dir)
	if s2.PublicKey() != key {
		t.Fatal("VAPID key changed across Open")
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{dir, filepath.Join(dir, vapidFile)} {
			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0o600)
			if fi.IsDir() {
				want = 0o700
			}
			if fi.Mode().Perm() != want {
				t.Errorf("%s mode %v, want %v", f, fi.Mode().Perm(), want)
			}
		}
	}
}

func TestCorruptFilesAreQuarantined(t *testing.T) {
	dir := t.TempDir()
	good := openTest(t, dir)
	oldKey := good.PublicKey()
	for _, f := range []string{vapidFile, subsFile} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := openTest(t, dir)
	if s.PublicKey() == "" || s.PublicKey() == oldKey {
		t.Fatal("expected a fresh VAPID key after corruption")
	}
	if len(s.Devices()) != 0 {
		t.Fatal("expected empty devices")
	}
	for _, f := range []string{vapidFile, subsFile} {
		if _, err := os.Stat(filepath.Join(dir, f+".corrupt-1")); err != nil {
			t.Errorf("%s not quarantined: %v", f, err)
		}
	}
	// A second corruption must not overwrite the first quarantine.
	if err := os.WriteFile(filepath.Join(dir, subsFile), []byte("[["), 0o600); err != nil {
		t.Fatal(err)
	}
	openTest(t, dir)
	if _, err := os.Stat(filepath.Join(dir, subsFile+".corrupt-2")); err != nil {
		t.Errorf("second quarantine missing: %v", err)
	}
}

func TestValidShapeButBadVAPIDIsRegenerated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, vapidFile), []byte(`{"private":"","public":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openTest(t, dir)
	if s.PublicKey() == "" {
		t.Fatal("no key generated")
	}
	if _, err := os.Stat(filepath.Join(dir, vapidFile+".corrupt-1")); err != nil {
		t.Fatal("bad VAPID file not set aside")
	}
}

func TestSubscribePersistsAndNeverLeaksSecrets(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, WithAllowAnyEndpoint())
	sub := newSub(t, "http://127.0.0.1:1/secret-endpoint-path")
	d, err := s.Subscribe(sub, "  Mi móvil\n", "UA/1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ID) != 12 || d.Label != "Mi móvil" || d.UserAgent != "UA/1.0" || d.CreatedAt.IsZero() {
		t.Fatalf("unexpected device %+v", d)
	}
	js, _ := json.Marshal(s.Devices())
	for _, secret := range []string{"secret-endpoint-path", sub.P256dh, sub.Auth, "endpoint"} {
		if strings.Contains(string(js), secret) {
			t.Errorf("device JSON leaks %q: %s", secret, js)
		}
	}
	for _, k := range []string{`"id"`, `"label"`, `"userAgent"`, `"createdAt"`} {
		if !strings.Contains(string(js), k) {
			t.Errorf("device JSON missing %s", k)
		}
	}
	// Reload from disk.
	s2 := openTest(t, dir, WithAllowAnyEndpoint())
	if got := s2.Devices(); len(got) != 1 || got[0].ID != d.ID {
		t.Fatalf("reload: %+v", got)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, subsFile))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("subscriptions mode %v", fi.Mode().Perm())
		}
	}
	// No temp leftovers.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestResubscribeSameEndpointUpdates(t *testing.T) {
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	sub := newSub(t, "http://127.0.0.1:1/a")
	d1, _ := s.Subscribe(sub, "uno", "ua")
	d2, err := s.Subscribe(sub, "dos", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if d1.ID != d2.ID || len(s.Devices()) != 1 || s.Devices()[0].Label != "dos" {
		t.Fatalf("resubscribe should update in place: %+v", s.Devices())
	}
	if !d1.CreatedAt.Equal(d2.CreatedAt) {
		t.Error("createdAt should be preserved")
	}
}

func TestDefaultLabelAndLimits(t *testing.T) {
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	d, err := s.Subscribe(newSub(t, "http://127.0.0.1:1/a"), "   ", strings.Repeat("x", 1000))
	if err != nil {
		t.Fatal(err)
	}
	if d.Label != "Dispositivo" || len([]rune(d.UserAgent)) != maxUARunes {
		t.Fatalf("got %+v", d)
	}
	d, _ = s.Subscribe(newSub(t, "http://127.0.0.1:1/b"), strings.Repeat("é", 500), "")
	if len([]rune(d.Label)) != maxLabelRunes {
		t.Fatalf("label not clipped: %d", len([]rune(d.Label)))
	}
}

func TestInvalidKeysRejected(t *testing.T) {
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	good := newSub(t, "http://127.0.0.1:1/a")
	cases := map[string]Subscription{
		"empty endpoint": {Endpoint: "", P256dh: good.P256dh, Auth: good.Auth},
		"bad p256dh b64": {Endpoint: good.Endpoint, P256dh: "!!!", Auth: good.Auth},
		"p256dh not on curve": {Endpoint: good.Endpoint,
			P256dh: base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...)), Auth: good.Auth},
		"p256dh short": {Endpoint: good.Endpoint, P256dh: "AAAA", Auth: good.Auth},
		"auth short":   {Endpoint: good.Endpoint, P256dh: good.P256dh, Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 8))},
		"auth bad b64": {Endpoint: good.Endpoint, P256dh: good.P256dh, Auth: "***"},
	}
	for name, sub := range cases {
		if _, err := s.Subscribe(sub, "x", "ua"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if len(s.Devices()) != 0 {
		t.Fatal("nothing should have been stored")
	}
}

func TestSSRFRejections(t *testing.T) {
	s := openTest(t, t.TempDir()) // allow-list active
	sub := func(ep string) Subscription { return newSub(t, ep) }
	bad := map[string]string{
		"http":              "http://fcm.googleapis.com/fcm/send/abc",
		"custom port":       "https://fcm.googleapis.com:8443/fcm/send/abc",
		"port 80":           "https://fcm.googleapis.com:80/x",
		"userinfo":          "https://user:pw@fcm.googleapis.com/x",
		"userinfo trick":    "https://fcm.googleapis.com@evil.example/x",
		"ipv4 literal":      "https://127.0.0.1/x",
		"ipv4 public":       "https://142.250.1.1/x",
		"ipv6 literal":      "https://[::1]/x",
		"ipv4-mapped v6":    "https://[::ffff:127.0.0.1]/x",
		"decimal ip":        "https://2130706433/x",
		"localhost":         "https://localhost/x",
		"localhost sub":     "https://foo.localhost/x",
		"localhost caps":    "https://LOCALHOST/x",
		"unlisted host":     "https://example.com/x",
		"suffix confusion":  "https://evilfcm.googleapis.com.evil.example/x",
		"prefix confusion":  "https://notfcm.googleapis.com/x",
		"bare suffix":       "https://push.services.mozilla.com/x",
		"lookalike mozilla": "https://updates.push.services.mozilla.com.evil.io/x",
		"apple lookalike":   "https://evilweb.push.apple.com.evil.io/x",
		"other google":      "https://www.googleapis.com/x",
		"ftp":               "ftp://fcm.googleapis.com/x",
		"no scheme":         "fcm.googleapis.com/x",
		"file":              "file:///etc/passwd",
		"whitespace":        "https://fcm.googleapis.com/x y",
		"newline":           "https://fcm.googleapis.com/x\r\nHost: evil",
		"empty":             "",
		"too long":          "https://fcm.googleapis.com/" + strings.Repeat("a", 3000),
		"metadata":          "https://169.254.169.254/latest/meta-data",
		"fullwidth slash":   "https://evil.example／.push.apple.com/x",
		"non-ascii host":    "https://fcm.googleapis.comé/x",
	}
	for name, ep := range bad {
		if _, err := s.Subscribe(sub(ep), "x", "ua"); err == nil {
			t.Errorf("%s: %q should be rejected", name, ep)
		}
	}
	if len(s.Devices()) != 0 {
		t.Fatal("rejected endpoints were stored")
	}
	good := []string{
		"https://fcm.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com:443/fcm/send/abc",
		"https://FCM.googleapis.com/fcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://wns2-par02p.notify.windows.com/w/?token=abc",
		"https://web.push.apple.com/abc",
		"https://api.push.apple.com/3/device/abc",
	}
	for _, ep := range good {
		if _, err := s.Subscribe(sub(ep), "x", "ua"); err != nil {
			t.Errorf("%q should be allowed: %v", ep, err)
		}
	}
}

func TestSSRFErrorsAreSpanish(t *testing.T) {
	s := openTest(t, t.TempDir())
	_, err := s.Subscribe(newSub(t, "http://127.0.0.1/x"), "x", "ua")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestSendRevalidatesStoredEndpoints(t *testing.T) {
	// A tampered subscriptions.json must not make the server call arbitrary hosts.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	dir := t.TempDir()
	sub := newSub(t, srv.URL+"/x")
	{
		s := openTest(t, dir, WithAllowAnyEndpoint())
		if _, err := s.Subscribe(sub, "x", "ua"); err != nil {
			t.Fatal(err)
		}
	}
	s := openTest(t, dir) // strict this time
	rep := s.Send(context.Background(), Notification{Title: "t", Body: "b"})
	if rep.Failed != 1 || rep.Sent != 0 || hits.Load() != 0 {
		t.Fatalf("rep=%+v hits=%d", rep, hits.Load())
	}
}

func TestMaxDevices(t *testing.T) {
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	for i := 0; i < maxDevices; i++ {
		if _, err := s.Subscribe(newSub(t, fmt.Sprintf("http://127.0.0.1:1/%d", i)), "d", "ua"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Subscribe(newSub(t, "http://127.0.0.1:1/extra"), "d", "ua"); err == nil {
		t.Fatal("device 21 should be refused")
	}
	// Refreshing an existing one is still fine at the limit.
	if _, err := s.Subscribe(newSub(t, "http://127.0.0.1:1/0"), "again", "ua"); err != nil {
		t.Fatalf("refresh at limit: %v", err)
	}
	if len(s.Devices()) != maxDevices {
		t.Fatalf("got %d devices", len(s.Devices()))
	}
}

func TestUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, WithAllowAnyEndpoint())
	d, _ := s.Subscribe(newSub(t, "http://127.0.0.1:1/a"), "x", "ua")
	if err := s.Unsubscribe("nope"); err != nil {
		t.Fatalf("unknown id should be a no-op: %v", err)
	}
	if err := s.Unsubscribe(d.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Devices()) != 0 || len(openTest(t, dir, WithAllowAnyEndpoint()).Devices()) != 0 {
		t.Fatal("device still present")
	}
}

// pushServer answers each path with a configured status and records requests.
type pushServer struct {
	*httptest.Server
	mu      sync.Mutex
	status  map[string]int
	reqs    []*http.Request
	bodies  [][]byte
	active  atomic.Int32
	maxSeen atomic.Int32
	delay   time.Duration
}

func newPushServer(t *testing.T, status map[string]int) *pushServer {
	t.Helper()
	ps := &pushServer{status: status}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := ps.active.Add(1)
		for {
			m := ps.maxSeen.Load()
			if cur <= m || ps.maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		defer ps.active.Add(-1)
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		ps.mu.Lock()
		ps.reqs = append(ps.reqs, r.Clone(context.Background()))
		ps.bodies = append(ps.bodies, buf[:n])
		code := ps.status[r.URL.Path]
		d := ps.delay
		ps.mu.Unlock()
		if d > 0 {
			time.Sleep(d)
		}
		if code == 0 {
			code = 201
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(ps.Close)
	return ps
}

func TestSendSuccess(t *testing.T) {
	ps := newPushServer(t, nil)
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	d, err := s.Subscribe(newSub(t, ps.URL+"/ok"), "Móvil", "ua")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ level, urgency string }{
		{"critical", "high"}, {"warning", "normal"}, {"info", "low"}, {"", "low"}, {"weird", "low"},
	}
	for _, c := range cases {
		rep := s.Send(context.Background(), Notification{Title: "T", Body: "B", Level: c.level})
		if rep != (Report{Sent: 1}) {
			t.Fatalf("level %q: report %+v", c.level, rep)
		}
		ps.mu.Lock()
		r := ps.reqs[len(ps.reqs)-1]
		ps.mu.Unlock()
		if got := r.Header.Get("Urgency"); got != c.urgency {
			t.Errorf("level %q: Urgency=%q want %q", c.level, got, c.urgency)
		}
		if got := r.Header.Get("TTL"); got != "14400" {
			t.Errorf("TTL=%q want 14400", got)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Errorf("unexpected request %s %v", r.Method, r.Header)
		}
		if a := r.Header.Get("Authorization"); !strings.HasPrefix(a, "vapid ") ||
			!strings.Contains(a, "k="+s.PublicKey()) {
			t.Errorf("bad VAPID header %q", a)
		}
	}
	got := s.Devices()[0]
	if got.ID != d.ID || got.LastOK.IsZero() || got.LastError != "" {
		t.Fatalf("device after success: %+v", got)
	}
}

func TestSendRemoves404And410(t *testing.T) {
	ps := newPushServer(t, map[string]int{"/gone": 410, "/missing": 404, "/ok": 201})
	dir := t.TempDir()
	s := openTest(t, dir, WithAllowAnyEndpoint())
	for _, p := range []string{"/gone", "/missing", "/ok"} {
		if _, err := s.Subscribe(newSub(t, ps.URL+p), p, "ua"); err != nil {
			t.Fatal(err)
		}
	}
	rep := s.Send(context.Background(), Notification{Title: "t", Body: "b"})
	if rep != (Report{Sent: 1, Removed: 2}) {
		t.Fatalf("report %+v", rep)
	}
	devs := s.Devices()
	if len(devs) != 1 || devs[0].Label != "/ok" {
		t.Fatalf("remaining %+v", devs)
	}
	if got := openTest(t, dir, WithAllowAnyEndpoint()).Devices(); len(got) != 1 {
		t.Fatalf("removal not persisted: %+v", got)
	}
}

func TestSend500KeepsDeviceAndRecordsError(t *testing.T) {
	ps := newPushServer(t, map[string]int{"/boom": 500})
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	d, _ := s.Subscribe(newSub(t, ps.URL+"/boom"), "x", "ua")
	rep := s.Send(context.Background(), Notification{Title: "t", Body: "b"})
	if rep != (Report{Failed: 1}) {
		t.Fatalf("report %+v", rep)
	}
	devs := s.Devices()
	if len(devs) != 1 || devs[0].ID != d.ID || !strings.Contains(devs[0].LastError, "500") {
		t.Fatalf("device %+v", devs)
	}
	if strings.Contains(devs[0].LastError, ps.URL) {
		t.Error("LastError leaks the endpoint")
	}
	// Recovers after a later success.
	ps.mu.Lock()
	ps.status["/boom"] = 201
	ps.mu.Unlock()
	if rep := s.Send(context.Background(), Notification{Title: "t"}); rep.Sent != 1 {
		t.Fatalf("report %+v", rep)
	}
	if devs := s.Devices(); devs[0].LastError != "" || devs[0].LastOK.IsZero() {
		t.Fatalf("device did not recover: %+v", devs[0])
	}
}

func TestTransportErrorDoesNotLeakEndpoint(t *testing.T) {
	ps := newPushServer(t, nil)
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	if _, err := s.Subscribe(newSub(t, ps.URL+"/secret-token-123"), "x", "ua"); err != nil {
		t.Fatal(err)
	}
	ps.Close() // connection refused from now on
	rep := s.Send(context.Background(), Notification{Title: "t"})
	if rep != (Report{Failed: 1}) {
		t.Fatalf("report %+v", rep)
	}
	e := s.Devices()[0].LastError
	if e == "" || strings.Contains(e, "secret-token-123") || strings.Contains(e, "127.0.0.1") {
		t.Fatalf("LastError %q", e)
	}
}

func TestSendTo(t *testing.T) {
	ps := newPushServer(t, map[string]int{"/b": 410})
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	a, _ := s.Subscribe(newSub(t, ps.URL+"/a"), "a", "ua")
	b, _ := s.Subscribe(newSub(t, ps.URL+"/b"), "b", "ua")
	rep, err := s.SendTo(context.Background(), a.ID, Notification{Title: "t"})
	if err != nil || rep != (Report{Sent: 1}) {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if len(ps.reqs) != 1 {
		t.Fatalf("only one device should be hit, got %d", len(ps.reqs))
	}
	rep, err = s.SendTo(context.Background(), b.ID, Notification{Title: "t"})
	if err != nil || rep != (Report{Removed: 1}) {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if _, err := s.SendTo(context.Background(), "zzzzzzzzzzzz", Notification{}); err == nil {
		t.Fatal("unknown id should error")
	}
}

func TestSendWithNoDevices(t *testing.T) {
	s := openTest(t, t.TempDir())
	if rep := s.Send(context.Background(), Notification{Title: "t"}); rep != (Report{}) {
		t.Fatalf("%+v", rep)
	}
}

func TestSendConcurrencyCappedAtFour(t *testing.T) {
	ps := newPushServer(t, nil)
	ps.delay = 60 * time.Millisecond
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	for i := 0; i < 12; i++ {
		if _, err := s.Subscribe(newSub(t, fmt.Sprintf("%s/d%d", ps.URL, i)), "d", "ua"); err != nil {
			t.Fatal(err)
		}
	}
	rep := s.Send(context.Background(), Notification{Title: "t"})
	if rep.Sent != 12 {
		t.Fatalf("report %+v", rep)
	}
	if m := ps.maxSeen.Load(); m > maxConcurrent || m < 2 {
		t.Fatalf("max concurrent requests = %d, want 2..%d", m, maxConcurrent)
	}
}

func TestSendRespectsContextCancel(t *testing.T) {
	ps := newPushServer(t, nil)
	ps.delay = 2 * time.Second
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	_, _ = s.Subscribe(newSub(t, ps.URL+"/slow"), "x", "ua")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	rep := s.Send(ctx, Notification{Title: "t"})
	if rep.Failed != 1 || time.Since(start) > time.Second {
		t.Fatalf("rep=%+v elapsed=%v", rep, time.Since(start))
	}
	if len(s.Devices()) != 1 {
		t.Fatal("a timeout must keep the device")
	}
}

type recordingClient struct {
	mu   sync.Mutex
	urls []string
	resp int
}

func (c *recordingClient) Do(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.urls = append(c.urls, r.URL.String())
	c.mu.Unlock()
	return &http.Response{StatusCode: c.resp, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestWithHTTPClientAndStrictEndpoint(t *testing.T) {
	c := &recordingClient{resp: 201}
	s := openTest(t, t.TempDir(), WithHTTPClient(c))
	if _, err := s.Subscribe(newSub(t, "https://fcm.googleapis.com/fcm/send/abc"), "x", "ua"); err != nil {
		t.Fatal(err)
	}
	if rep := s.Send(context.Background(), Notification{Title: "t"}); rep.Sent != 1 {
		t.Fatalf("%+v", rep)
	}
	if len(c.urls) != 1 || c.urls[0] != "https://fcm.googleapis.com/fcm/send/abc" {
		t.Fatalf("urls %v", c.urls)
	}
}

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	var target atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { target.Add(1) }))
	defer internal.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	_, _ = s.Subscribe(newSub(t, redir.URL+"/x"), "x", "ua")
	rep := s.Send(context.Background(), Notification{Title: "t"})
	if target.Load() != 0 || rep.Sent != 0 || rep.Failed != 1 {
		t.Fatalf("redirect followed: hits=%d rep=%+v", target.Load(), rep)
	}
}

func TestPayloadTruncation(t *testing.T) {
	long := strings.Repeat("ñ\"\n<", 5000)
	data := buildPayload(Notification{Title: "Aviso", Body: long, Tag: "disk_low", URL: "/#/alertas", Level: "critical", Renotify: true})
	if len(data) > maxPayload {
		t.Fatalf("payload %d bytes > %d", len(data), maxPayload)
	}
	var n Notification
	if err := json.Unmarshal(data, &n); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if n.Title != "Aviso" || n.Tag != "disk_low" || n.URL != "/#/alertas" || n.Level != "critical" || !n.Renotify {
		t.Fatalf("metadata lost: %+v", n)
	}
	if n.Body == "" || !strings.HasPrefix(long, n.Body) {
		t.Fatalf("body should be a non-empty prefix, got %d bytes", len(n.Body))
	}
	// Short bodies are untouched.
	data = buildPayload(Notification{Title: "a", Body: "b"})
	_ = json.Unmarshal(data, &n)
	if n.Body != "b" || n.Level != "info" {
		t.Fatalf("%+v", n)
	}
	// Hostile giant metadata still fits.
	data = buildPayload(Notification{Title: strings.Repeat("t", 10000), Body: "x", Tag: strings.Repeat("g", 9999), URL: strings.Repeat("u", 9999)})
	if len(data) > maxPayload {
		t.Fatalf("giant metadata: %d bytes", len(data))
	}
	// Invalid UTF-8 is cleaned.
	data = buildPayload(Notification{Title: "a\xff", Body: "b\xfe"})
	if !json.Valid(data) {
		t.Fatal("invalid JSON")
	}
}

func TestPayloadSentOverWireIsEncryptedAndSmall(t *testing.T) {
	ps := newPushServer(t, nil)
	s := openTest(t, t.TempDir(), WithAllowAnyEndpoint())
	_, _ = s.Subscribe(newSub(t, ps.URL+"/a"), "x", "ua")
	s.Send(context.Background(), Notification{Title: "t", Body: strings.Repeat("z", 100000)})
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if n := len(ps.bodies[0]); n == 0 || n > 4096 {
		t.Fatalf("encrypted body %d bytes", n)
	}
	if strings.Contains(string(ps.bodies[0]), "zzzz") {
		t.Fatal("body not encrypted")
	}
}

func TestConcurrentUse(t *testing.T) {
	ps := newPushServer(t, map[string]int{"/gone": 410})
	dir := t.TempDir()
	s := openTest(t, dir, WithAllowAnyEndpoint())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				ep := fmt.Sprintf("%s/g%d-%d", ps.URL, g, i%3)
				if i%4 == 3 {
					ep = ps.URL + "/gone"
				}
				d, err := s.Subscribe(newSub(t, ep), "d", "ua")
				if err != nil {
					continue // limit reached is fine
				}
				s.Send(context.Background(), Notification{Title: "t"})
				_ = s.Devices()
				if i%2 == 0 {
					_ = s.Unsubscribe(d.ID)
				}
			}
		}(g)
	}
	wg.Wait()
	// State on disk is valid and loads.
	s2 := openTest(t, dir, WithAllowAnyEndpoint())
	if len(s2.Devices()) != len(s.Devices()) {
		t.Fatalf("disk has %d devices, memory %d", len(s2.Devices()), len(s.Devices()))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt") || strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("unexpected file %s", e.Name())
		}
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open("", "x"); err == nil {
		t.Fatal("empty dir should fail")
	}
	file := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := Open(filepath.Join(file, "sub"), "x"); err == nil {
		t.Fatal("dir under a file should fail")
	}
}

func TestLoadIgnoresTamperedRecords(t *testing.T) {
	dir := t.TempDir()
	openTest(t, dir) // create vapid
	bad := `[{"id":"deadbeef0000","endpoint":"https://fcm.googleapis.com/x","p256dh":"a","auth":"b"},{"id":"","endpoint":""}]`
	_ = os.WriteFile(filepath.Join(dir, subsFile), []byte(bad), 0o600)
	s := openTest(t, dir)
	if len(s.Devices()) != 0 {
		t.Fatalf("tampered records loaded: %+v", s.Devices())
	}
}
