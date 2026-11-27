package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

func init() { pbkdf2Iterations = 2000 } // keep tests fast; production uses 600 000

const testPassword = "correct horse battery"

func newAuth(t *testing.T) *Auth {
	t.Helper()
	h, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuth("admin", h, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func authServer(t *testing.T, a *Auth) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(telemetry.New(telemetry.Config{Version: "t"}), WithAuth(a)))
	t.Cleanup(srv.Close)
	return srv
}

// client that keeps cookies but does not follow redirects, so tests see them.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func login(t *testing.T, c *http.Client, base, user, pass string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"user": user, "password": pass})
	resp, err := c.Post(base+"/api/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func status(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHashRoundTripAndFormat(t *testing.T) {
	h, err := HashPassword("s3cret-pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$") || strings.Contains(h, "s3cret") {
		t.Fatalf("hash = %q", h)
	}
	h2, _ := HashPassword("s3cret-pass")
	if h == h2 {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
	a, err := NewAuth("u", h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !a.verify("u", "s3cret-pass") || a.verify("u", "wrong") || a.verify("other", "s3cret-pass") {
		t.Fatal("verify accepted or rejected the wrong credentials")
	}
}

func TestNewAuthRejectsBadInput(t *testing.T) {
	for name, tc := range map[string][2]string{
		"empty user":   {"", "pbkdf2-sha256$2000$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5"},
		"empty hash":   {"u", ""},
		"plain text":   {"u", "hunter2"},
		"wrong scheme": {"u", "bcrypt$10$abc$def"},
		"low rounds":   {"u", "pbkdf2-sha256$10$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5"},
	} {
		if _, err := NewAuth(tc[0], tc[1], nil); err == nil {
			t.Errorf("%s: NewAuth should fail", name)
		}
	}
	h, _ := HashPassword("longenough")
	if _, err := NewAuth("u", h, []byte("short")); err == nil {
		t.Error("a short session key must be refused")
	}
}

func TestTokensCannotBeForgedOrReplayedAfterExpiry(t *testing.T) {
	a := newAuth(t)
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }

	tok := a.token("admin", now.Add(time.Hour))
	if u, ok := a.parseToken(tok); !ok || u != "admin" {
		t.Fatal("a fresh token must be valid")
	}
	// Tampering with any part invalidates the signature.
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + parts[1] + "." + "9999999999" + "." + parts[3]
	if _, ok := a.parseToken(forged); ok {
		t.Fatal("a token with an extended expiry must be rejected")
	}
	if _, ok := a.parseToken(tok + "x"); ok {
		t.Fatal("a token with a modified signature must be rejected")
	}
	// A token signed with another key is worthless.
	other := newAuth(t)
	other.key = []byte("ffffffffffffffffffffffffffffffff")
	if _, ok := a.parseToken(other.token("admin", now.Add(time.Hour))); ok {
		t.Fatal("a token signed with a different key must be rejected")
	}
	// Expiry.
	a.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, ok := a.parseToken(tok); ok {
		t.Fatal("an expired token must be rejected")
	}
	for _, junk := range []string{"", ".", "v1", "v1.a.b", "garbage"} {
		if _, ok := a.parseToken(junk); ok {
			t.Errorf("junk token %q accepted", junk)
		}
	}
}

func TestEverythingExceptTheLoginScreenNeedsASession(t *testing.T) {
	srv := authServer(t, newAuth(t))
	c := newClient(t)

	// Pages redirect to the login screen; API callers get a 401.
	resp, _ := c.Get(srv.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("/ -> %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range []string{"/api/state", "/api/history", "/api/procs", "/api/me", "/metrics"} {
		if got := status(t, c, srv.URL+p); got != http.StatusUnauthorized {
			t.Errorf("%s without a session -> %d, want 401", p, got)
		}
	}
	for _, p := range []string{"/app.js", "/ui.css", "/index.html"} {
		if got := status(t, c, srv.URL+p); got != http.StatusFound {
			t.Errorf("%s without a session -> %d, want a redirect", p, got)
		}
	}
	// What the login screen needs stays public.
	for _, p := range []string{"/login", "/login.js", "/style.css", "/logo-lockup.svg", "/logo-lockup-dark.svg", "/logo-light.svg", "/fonts/space-grotesk-latin-wght.woff2", "/healthz"} {
		if got := status(t, c, srv.URL+p); got != http.StatusOK {
			t.Errorf("%s should be public, got %d", p, got)
		}
	}
}

func TestLoginGrantsAccessWithAHardenedCookie(t *testing.T) {
	srv := authServer(t, newAuth(t))
	c := newClient(t)

	resp := login(t, c, srv.URL, "admin", testPassword)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login -> %d", resp.StatusCode)
	}
	var ck *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == cookieName {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.MaxAge <= 0 {
		t.Fatalf("session cookie not hardened: %+v", ck)
	}
	if got := status(t, c, srv.URL+"/api/state"); got != http.StatusOK {
		t.Fatalf("/api/state with a session -> %d", got)
	}
	if got := status(t, c, srv.URL+"/"); got != http.StatusOK {
		t.Fatalf("/ with a session -> %d", got)
	}
	// Already logged in: the login screen sends you to the dashboard.
	r, _ := c.Get(srv.URL + "/login")
	r.Body.Close()
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/" {
		t.Fatalf("/login while logged in -> %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestSecureFlagFollowsTheForwardedProtocol(t *testing.T) {
	srv := authServer(t, newAuth(t))
	body := `{"user":"admin","password":"` + testPassword + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(body))
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName && !ck.Secure {
			t.Fatal("behind HTTPS the session cookie must be Secure")
		}
	}
}

func TestWrongCredentialsAreRejectedWithAGenericMessage(t *testing.T) {
	srv := authServer(t, newAuth(t))
	c := newClient(t)
	for _, cred := range [][2]string{{"admin", "nope"}, {"intruso", testPassword}, {"", ""}} {
		resp := login(t, c, srv.URL, cred[0], cred[1])
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%v -> %d, want 401", cred, resp.StatusCode)
		}
		if len(resp.Cookies()) != 0 {
			t.Fatal("a failed login must not set a cookie")
		}
	}
	if got := status(t, c, srv.URL+"/api/state"); got != http.StatusUnauthorized {
		t.Fatal("failed logins must not grant access")
	}
}

func TestBruteForceIsLockedOutEvenWithTheRightPassword(t *testing.T) {
	a := newAuth(t)
	srv := authServer(t, a)
	c := newClient(t)
	for i := 0; i < maxFailures; i++ {
		login(t, c, srv.URL, "admin", "wrong")
	}
	resp := login(t, c, srv.URL, "admin", testPassword)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("after %d failures the client must be locked out: %d", maxFailures, resp.StatusCode)
	}
	// The lockout expires.
	a.now = func() time.Time { return time.Now().Add(failWindow + time.Minute) }
	if resp := login(t, c, srv.URL, "admin", testPassword); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("after the window the right password must work: %d", resp.StatusCode)
	}
}

func TestLockoutIsPerClientAndForwardedIPsAreOnlyTrustedFromTheProxy(t *testing.T) {
	a := newAuth(t)
	srv := authServer(t, a)
	post := func(ip, pass string) int {
		body := `{"user":"admin","password":"` + pass + `"}`
		req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(body))
		req.Header.Set("X-Real-IP", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// The test server listens on loopback, i.e. it plays the reverse proxy,
	// so X-Real-IP is honoured and each client gets its own bucket.
	for i := 0; i < maxFailures; i++ {
		post("203.0.113.7", "wrong")
	}
	if got := post("203.0.113.7", testPassword); got != http.StatusTooManyRequests {
		t.Fatalf("the abusive client should be locked out, got %d", got)
	}
	if got := post("198.51.100.9", testPassword); got != http.StatusNoContent {
		t.Fatalf("a different client must not be affected, got %d", got)
	}
}

func TestClientIPIgnoresForwardedHeaderFromNonLoopbackPeers(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.20:5555"
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if got := clientIP(r); got != "198.51.100.20" {
		t.Fatalf("a direct client must not choose its own address: %q", got)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("the local proxy's header should be trusted: %q", got)
	}
	r.Header.Set("X-Real-IP", "not-an-ip")
	if got := clientIP(r); got != "127.0.0.1" {
		t.Fatalf("a malformed header must be ignored: %q", got)
	}
}

func TestCrossSiteLoginAndLogoutAreRefused(t *testing.T) {
	srv := authServer(t, newAuth(t))
	body := `{"user":"admin","password":"` + testPassword + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(body))
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a login posted from another origin -> %d, want 403", resp.StatusCode)
	}
	// With a valid session, a logout forged by another site is refused too.
	c := newClient(t)
	login(t, c, srv.URL, "admin", testPassword)
	req, _ = http.NewRequest("POST", srv.URL+"/api/logout", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout from another origin -> %d, want 403", resp.StatusCode)
	}
	if status(t, c, srv.URL+"/api/state") != http.StatusOK {
		t.Fatal("a refused cross-site logout must not end the session")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	srv := authServer(t, newAuth(t))
	c := newClient(t)
	login(t, c, srv.URL, "admin", testPassword)
	if status(t, c, srv.URL+"/api/state") != http.StatusOK {
		t.Fatal("setup: not logged in")
	}
	resp, _ := c.Post(srv.URL+"/api/logout", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout -> %d", resp.StatusCode)
	}
	if got := status(t, c, srv.URL+"/api/state"); got != http.StatusUnauthorized {
		t.Fatalf("after logout the API must refuse the old cookie jar, got %d", got)
	}
}

func TestOversizedAndMalformedLoginBodiesAreRejected(t *testing.T) {
	srv := authServer(t, newAuth(t))
	for name, body := range map[string]string{
		"not json": "usuario=admin",
		"huge":     `{"user":"admin","password":"` + strings.Repeat("a", 10_000) + `"}`,
	} {
		resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestWithoutAuthTheDashboardIsOpenAndSaysSo(t *testing.T) {
	srv := httptest.NewServer(Handler(telemetry.New(telemetry.Config{})))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var me struct{ Auth bool }
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil || me.Auth {
		t.Fatalf("me = %+v err=%v", me, err)
	}
	if status(t, http.DefaultClient, srv.URL+"/api/state") != 200 {
		t.Fatal("without login configured the API stays reachable")
	}
}
