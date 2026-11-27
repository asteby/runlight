package dashboard

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pbkdf2Iterations is the work factor of new password hashes (OWASP 2023:
// 600 000 for PBKDF2-HMAC-SHA256). Tests lower it.
var pbkdf2Iterations = 600_000

const (
	hashPrefix  = "pbkdf2-sha256"
	cookieName  = "rl_session"
	sessionTTL  = 12 * time.Hour
	maxFailures = 5                // per client, per window
	failWindow  = 10 * time.Minute // lockout lasts as long as the window
)

// HashPassword returns a storable "pbkdf2-sha256$iters$salt$hash" string.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, pbkdf2Iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

type parsedHash struct {
	iters int
	salt  []byte
	key   []byte
}

func parseHash(s string) (parsedHash, error) {
	p := strings.Split(s, "$")
	if len(p) != 4 || p[0] != hashPrefix {
		return parsedHash{}, errors.New("not a " + hashPrefix + " hash")
	}
	iters, err := strconv.Atoi(p[1])
	if err != nil || iters < 1000 || iters > 10_000_000 {
		return parsedHash{}, errors.New("bad iteration count")
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	key, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(key) < 16 {
		return parsedHash{}, errors.New("bad salt or key")
	}
	return parsedHash{iters, salt, key}, nil
}

// Auth protects the dashboard with a login page and a signed session cookie.
type Auth struct {
	user string
	hash parsedHash
	key  []byte // HMAC key signing sessions
	ttl  time.Duration
	now  func() time.Time

	mu    sync.Mutex
	fails map[string]*failure
}

type failure struct {
	n     int
	since time.Time
}

// NewAuth builds an Auth. key signs the sessions; when empty a random one is
// generated, which invalidates every session on restart.
func NewAuth(user, hash string, key []byte) (*Auth, error) {
	if user == "" {
		return nil, errors.New("empty user")
	}
	ph, err := parseHash(hash)
	if err != nil {
		return nil, err
	}
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	}
	if len(key) < 16 {
		return nil, errors.New("session key too short (min 16 bytes)")
	}
	return &Auth{user: user, hash: ph, key: key, ttl: sessionTTL, now: time.Now, fails: map[string]*failure{}}, nil
}

// verify checks the credentials in constant time. It always runs the full
// PBKDF2 derivation, even for an unknown user, so response time does not
// reveal whether the user exists.
func (a *Auth) verify(user, password string) bool {
	key, err := pbkdf2.Key(sha256.New, password, a.hash.salt, a.hash.iters, len(a.hash.key))
	if err != nil {
		return false
	}
	okKey := subtle.ConstantTimeCompare(key, a.hash.key) == 1
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) == 1
	return okKey && okUser
}

func (a *Auth) sign(msg string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// token is "v1.<user>.<expiry>.<signature>" with the user base64url-encoded.
func (a *Auth) token(user string, exp time.Time) string {
	body := "v1." + base64.RawURLEncoding.EncodeToString([]byte(user)) + "." + strconv.FormatInt(exp.Unix(), 10)
	return body + "." + a.sign(body)
}

func (a *Auth) parseToken(t string) (string, bool) {
	i := strings.LastIndexByte(t, '.')
	if i < 0 {
		return "", false
	}
	body, sig := t[:i], t[i+1:]
	if !hmac.Equal([]byte(sig), []byte(a.sign(body))) {
		return "", false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || a.now().Unix() >= exp {
		return "", false
	}
	u, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(u, []byte(a.user)) != 1 {
		return "", false
	}
	return string(u), true
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *Auth) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(r),
	})
}

func (a *Auth) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	return a.parseToken(c.Value)
}

// clientIP trusts X-Real-IP only when the request comes from the local
// reverse proxy; otherwise a client could pick its own rate-limit bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(v) != nil {
			return v
		}
	}
	return host
}

// locked reports whether a client is locked out, and for how long.
func (a *Auth) locked(ip string) (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[ip]
	if f == nil {
		return 0, false
	}
	if a.now().Sub(f.since) > failWindow {
		delete(a.fails, ip)
		return 0, false
	}
	if f.n >= maxFailures {
		return failWindow - a.now().Sub(f.since), true
	}
	return 0, false
}

func (a *Auth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[ip]
	if f == nil || a.now().Sub(f.since) > failWindow {
		f = &failure{since: a.now()}
		a.fails[ip] = f
	}
	f.n++
	if len(a.fails) > 10_000 { // bound the table against address floods
		for k, v := range a.fails {
			if a.now().Sub(v.since) > failWindow {
				delete(a.fails, k)
			}
		}
	}
}

func (a *Auth) clearFailures(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
}

// sameOrigin rejects cross-site form posts: if the browser sends an Origin
// header it must match the Host we are serving.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // non-browser client; the SameSite cookie still protects state
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		jsonErr(w, http.StatusForbidden, "Origen no permitido")
		return
	}
	ip := clientIP(r)
	if wait, locked := a.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		jsonErr(w, http.StatusTooManyRequests, fmt.Sprintf("Demasiados intentos. Espera %d min e inténtalo de nuevo.", int(wait.Minutes())+1))
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "Solicitud no válida")
		return
	}
	if !a.verify(strings.TrimSpace(body.User), body.Password) {
		a.recordFailure(ip)
		time.Sleep(250 * time.Millisecond) // blunt online guessing
		jsonErr(w, http.StatusUnauthorized, "Usuario o contraseña incorrectos")
		return
	}
	a.clearFailures(ip)
	a.setCookie(w, r, a.token(a.user, a.now().Add(a.ttl)), int(a.ttl.Seconds()))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		jsonErr(w, http.StatusForbidden, "Origen no permitido")
		return
	}
	a.setCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// public lists what an unauthenticated visitor may fetch: the login page and
// the assets it needs. Everything else, including the API, needs a session.
var public = map[string]bool{
	"/login": true, "/login.html": true, "/login.js": true, "/style.css": true,
	"/logo-light.svg": true, "/logo-dark.svg": true, "/logo-lockup.svg": true, "/logo-lockup-dark.svg": true,
	"/healthz": true, "/api/login": true,
	"/manifest.webmanifest": true, "/sw.js": true, "/offline.html": true, "/icons.svg": true,
}

func isPublic(path string) bool {
	return public[path] || strings.HasPrefix(path, "/fonts/") || strings.HasPrefix(path, "/icons/")
}

// middleware guards next. Browsers asking for pages are sent to /login; API
// callers get a 401 they can act on.
func (a *Auth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.session(r); ok {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
			jsonErr(w, http.StatusUnauthorized, "Sesión requerida")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}
