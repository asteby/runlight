// Package push delivers Web Push notifications (VAPID) to the browsers that
// subscribed from the dashboard. Subscriptions and the VAPID key pair are
// persisted under a state directory.
//
// The server POSTs to an endpoint chosen by the browser, so endpoints are
// restricted to the well-known push services (anti-SSRF) and never logged.
package push

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const (
	maxDevices     = 20
	maxPayload     = 3 * 1024
	sendTTL        = 4 * 60 * 60 // seconds
	sendTimeout    = 10 * time.Second
	maxConcurrent  = 4
	maxEndpointLen = 2048
	maxLabelRunes  = 60
	maxUARunes     = 200

	vapidFile = "vapid.json"
	subsFile  = "subscriptions.json"
)

// Notification is the payload shown by the service worker.
type Notification struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Tag      string `json:"tag,omitempty"`   // same tag replaces the previous notification
	URL      string `json:"url,omitempty"`   // route inside the dashboard opened on click
	Level    string `json:"level,omitempty"` // "info" | "warning" | "critical"
	Renotify bool   `json:"renotify,omitempty"`
}

// Subscription is the browser PushSubscription as the server needs it.
type Subscription struct{ Endpoint, P256dh, Auth string }

// Device is the public view of a subscription: never keys nor the endpoint.
type Device struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	UserAgent string    `json:"userAgent"`
	CreatedAt time.Time `json:"createdAt"`
	LastOK    time.Time `json:"lastOk,omitzero"`
	LastError string    `json:"lastError,omitempty"`
}

// Report summarises a Send.
type Report struct {
	Sent    int `json:"sent"`
	Failed  int `json:"failed"`
	Removed int `json:"removed"`
}

// HTTPClient is the subset of *http.Client used to reach push services.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Option configures a Store.
type Option func(*Store)

// WithHTTPClient replaces the HTTP client used to deliver messages.
func WithHTTPClient(c HTTPClient) Option { return func(s *Store) { s.client = c } }

// WithAllowAnyEndpoint disables the endpoint allow-list. Tests only.
func WithAllowAnyEndpoint() Option { return func(s *Store) { s.allowAny = true } }

type vapidKeys struct {
	Private string `json:"private"`
	Public  string `json:"public"`
}

// record is the on-disk form of a subscription.
type record struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	UserAgent string    `json:"userAgent"`
	CreatedAt time.Time `json:"createdAt"`
	LastOK    time.Time `json:"lastOk,omitzero"`
	LastError string    `json:"lastError,omitempty"`
	Endpoint  string    `json:"endpoint"`
	P256dh    string    `json:"p256dh"`
	Auth      string    `json:"auth"`
}

func (r *record) device() Device {
	return Device{ID: r.ID, Label: r.Label, UserAgent: r.UserAgent,
		CreatedAt: r.CreatedAt, LastOK: r.LastOK, LastError: r.LastError}
}

// Store keeps the VAPID keys and the subscriptions. Safe for concurrent use.
type Store struct {
	dir        string
	subscriber string
	vapid      vapidKeys
	client     HTTPClient
	allowAny   bool

	mu   sync.Mutex
	subs map[string]*record
}

// Open loads (or creates) the state in dir. subscriber is the VAPID subject
// (a https: or mailto: URL).
func Open(dir, subscriber string, opts ...Option) (*Store, error) {
	if dir == "" {
		return nil, errors.New("push: se necesita un directorio de estado")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("push: no se pudo crear %s: %w", dir, err)
	}
	s := &Store{dir: dir, subscriber: subscriber, subs: map[string]*record{}}
	for _, o := range opts {
		o(s)
	}
	if s.client == nil {
		s.client = &http.Client{
			Timeout: sendTimeout,
			// A push service never needs to redirect us; following would
			// sidestep the allow-list.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if err := s.loadVAPID(); err != nil {
		return nil, err
	}
	s.loadSubs()
	return s, nil
}

func (s *Store) loadVAPID() error {
	path := filepath.Join(s.dir, vapidFile)
	var k vapidKeys
	if readJSON(path, &k) {
		if validVAPID(k) {
			s.vapid = k
			return nil
		}
		quarantine(path)
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return fmt.Errorf("push: no se pudieron generar las claves VAPID: %w", err)
	}
	s.vapid = vapidKeys{Private: priv, Public: pub}
	return writeAtomic(path, s.vapid)
}

func validVAPID(k vapidKeys) bool {
	priv, err1 := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.Private, "="))
	pub, err2 := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.Public, "="))
	return err1 == nil && err2 == nil && len(priv) == 32 && len(pub) == 65
}

func (s *Store) loadSubs() {
	path := filepath.Join(s.dir, subsFile)
	var list []record
	if !readJSON(path, &list) {
		return
	}
	for i := range list {
		r := list[i]
		if r.Endpoint == "" || r.ID != endpointID(r.Endpoint) || len(s.subs) >= maxDevices {
			continue
		}
		s.subs[r.ID] = &r
	}
}

// readJSON decodes path into v. A missing file returns false silently; a
// file that does not parse is moved aside as <path>.corrupt-<n> and returns
// false.
func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		quarantine(path)
		return false
	}
	return true
}

// quarantine renames path to the first free <path>.corrupt-<n>.
func quarantine(path string) {
	for n := 1; n < 1000; n++ {
		dst := fmt.Sprintf("%s.corrupt-%d", path, n)
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			if os.Rename(path, dst) == nil {
				return
			}
		}
	}
	_ = os.Remove(path)
}

// writeAtomic writes v as JSON through a temp file + rename, mode 0600.
func writeAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("push: no se pudo escribir %s: %w", filepath.Base(path), err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("push: no se pudo guardar %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(name, path); err != nil {
		return fail(err)
	}
	return nil
}

// saveLocked persists subscriptions; s.mu must be held.
func (s *Store) saveLocked() error {
	list := make([]record, 0, len(s.subs))
	for _, r := range s.subs {
		list = append(list, *r)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
	return writeAtomic(filepath.Join(s.dir, subsFile), list)
}

// PublicKey returns the VAPID public key (base64url) for pushManager.subscribe.
func (s *Store) PublicKey() string { return s.vapid.Public }

func endpointID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])[:12]
}

var allowedHostSuffixes = []string{
	".push.services.mozilla.com",
	".notify.windows.com",
	".push.apple.com",
}

var allowedHosts = []string{"fcm.googleapis.com", "web.push.apple.com"}

// validateEndpoint implements the anti-SSRF policy.
func validateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("falta la dirección (endpoint) de la suscripción")
	}
	if len(raw) > maxEndpointLen {
		return errors.New("la dirección de la suscripción es demasiado larga")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return errors.New("la dirección de la suscripción contiene caracteres no válidos")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("la dirección de la suscripción no es una URL válida")
	}
	if u.Scheme != "https" {
		return errors.New("la dirección de la suscripción debe usar https")
	}
	if u.User != nil {
		return errors.New("la dirección de la suscripción no puede incluir usuario ni contraseña")
	}
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("la dirección de la suscripción debe usar el puerto 443")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return errors.New("la dirección de la suscripción no tiene servidor")
	}
	for i := 0; i < len(host); i++ {
		// Only plain ASCII hostnames: IDNA mapping could fold look-alike
		// characters (e.g. a fullwidth slash) into something else at dial time.
		if c := host[i]; c >= 0x80 || !(c == '.' || c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z') {
			return errors.New("la dirección de la suscripción tiene un servidor no válido")
		}
	}
	if _, err := netip.ParseAddr(host); err == nil || strings.ContainsAny(host, ":[]") {
		return errors.New("la dirección de la suscripción no puede ser una IP")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("la dirección de la suscripción no puede apuntar a localhost")
	}
	for _, h := range allowedHosts {
		if host == h {
			return nil
		}
	}
	for _, suf := range allowedHostSuffixes {
		if strings.HasSuffix(host, suf) {
			return nil
		}
	}
	return errors.New("el servicio de notificaciones de esa suscripción no está permitido")
}

func validateKeys(p256dh, auth string) error {
	dh, err := decodeB64(p256dh)
	if err != nil {
		return errors.New("la clave p256dh no es base64 válido")
	}
	if _, err := ecdh.P256().NewPublicKey(dh); err != nil {
		return errors.New("la clave p256dh no es una clave pública P-256 válida")
	}
	a, err := decodeB64(auth)
	if err != nil {
		return errors.New("el secreto auth no es base64 válido")
	}
	if len(a) != 16 {
		return errors.New("el secreto auth debe tener 16 bytes")
	}
	return nil
}

func decodeB64(v string) ([]byte, error) {
	v = strings.TrimRight(v, "=")
	if b, err := base64.RawURLEncoding.DecodeString(v); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(v)
}

// clip strips control characters and limits v to n runes.
func clip(v string, n int) string {
	v = strings.ToValidUTF8(v, "")
	v = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, v)
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

// Subscribe registers (or refreshes) a browser subscription.
func (s *Store) Subscribe(sub Subscription, label, userAgent string) (Device, error) {
	if !s.allowAny {
		if err := validateEndpoint(sub.Endpoint); err != nil {
			return Device{}, err
		}
	} else if sub.Endpoint == "" {
		return Device{}, errors.New("falta la dirección (endpoint) de la suscripción")
	}
	if err := validateKeys(sub.P256dh, sub.Auth); err != nil {
		return Device{}, err
	}
	label = clip(label, maxLabelRunes)
	if label == "" {
		label = "Dispositivo"
	}
	userAgent = clip(userAgent, maxUARunes)
	id := endpointID(sub.Endpoint)

	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.subs[id]
	if prev == nil && len(s.subs) >= maxDevices {
		return Device{}, fmt.Errorf("se alcanzó el máximo de %d dispositivos; quita alguno antes de añadir otro", maxDevices)
	}
	r := &record{ID: id, Label: label, UserAgent: userAgent, CreatedAt: time.Now().UTC(),
		Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth}
	if prev != nil {
		r.CreatedAt, r.LastOK = prev.CreatedAt, prev.LastOK
	}
	s.subs[id] = r
	if err := s.saveLocked(); err != nil {
		if prev != nil {
			s.subs[id] = prev
		} else {
			delete(s.subs, id)
		}
		return Device{}, err
	}
	return r.device(), nil
}

// Unsubscribe removes a device. Unknown ids are not an error.
func (s *Store) Unsubscribe(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.subs[id]
	if !ok {
		return nil
	}
	delete(s.subs, id)
	if err := s.saveLocked(); err != nil {
		s.subs[id] = r
		return err
	}
	return nil
}

// Devices lists the registered devices, oldest first.
func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.subs))
	for _, r := range s.subs {
		out = append(out, r.device())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Send delivers n to every device.
func (s *Store) Send(ctx context.Context, n Notification) Report {
	s.mu.Lock()
	targets := make([]record, 0, len(s.subs))
	for _, r := range s.subs {
		targets = append(targets, *r)
	}
	s.mu.Unlock()
	return s.deliver(ctx, targets, n)
}

// SendTo delivers n to a single device.
func (s *Store) SendTo(ctx context.Context, id string, n Notification) (Report, error) {
	s.mu.Lock()
	r, ok := s.subs[id]
	var targets []record
	if ok {
		targets = []record{*r}
	}
	s.mu.Unlock()
	if !ok {
		return Report{}, errors.New("dispositivo no encontrado")
	}
	return s.deliver(ctx, targets, n), nil
}

type outcome struct {
	id      string
	err     string
	gone    bool
	success bool
}

func (s *Store) deliver(ctx context.Context, targets []record, n Notification) Report {
	var rep Report
	if len(targets) == 0 {
		return rep
	}
	payload := buildPayload(n)
	urgency := urgencyFor(n.Level)

	results := make([]outcome, len(targets))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.sendOne(ctx, targets[i], payload, urgency)
		}(i)
	}
	wg.Wait()

	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, o := range results {
		cur, ok := s.subs[o.id]
		switch {
		case o.gone:
			rep.Removed++
			if ok {
				delete(s.subs, o.id)
				changed = true
			}
		case o.success:
			rep.Sent++
			if ok {
				cur.LastOK, cur.LastError = now, ""
				changed = true
			}
		default:
			rep.Failed++
			if ok {
				cur.LastError = o.err
				changed = true
			}
		}
	}
	if changed {
		_ = s.saveLocked() // best effort: delivery already happened
	}
	return rep
}

func (s *Store) sendOne(ctx context.Context, r record, payload []byte, urgency webpush.Urgency) outcome {
	o := outcome{id: r.ID}
	if !s.allowAny {
		if err := validateEndpoint(r.Endpoint); err != nil {
			o.err = "dirección no permitida"
			return o
		}
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: r.Endpoint,
		Keys:     webpush.Keys{P256dh: r.P256dh, Auth: r.Auth},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subscriber,
		TTL:             sendTTL,
		Urgency:         urgency,
		VAPIDPublicKey:  s.vapid.Public,
		VAPIDPrivateKey: s.vapid.Private,
	})
	if err != nil {
		o.err = describeErr(err)
		return o
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		o.gone = true
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		o.success = true
	default:
		o.err = fmt.Sprintf("el servicio respondió HTTP %d", resp.StatusCode)
	}
	return o
}

// describeErr turns a transport error into a short message without the URL
// (which would leak the endpoint).
func describeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "tiempo de espera agotado"
	case errors.Is(err, context.Canceled):
		return "envío cancelado"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "tiempo de espera agotado"
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return "no se pudo conectar con el servicio"
	}
	// Raw messages can embed the endpoint or its IP; keep it generic.
	return "error de envío"
}

func urgencyFor(level string) webpush.Urgency {
	switch level {
	case "critical":
		return webpush.UrgencyHigh
	case "warning":
		return webpush.UrgencyNormal
	default:
		return webpush.UrgencyLow
	}
}

// buildPayload serialises n, truncating Body so the JSON stays <= maxPayload.
func buildPayload(n Notification) []byte {
	n.Title = clip(n.Title, 120)
	n.Tag = clip(n.Tag, 100)
	n.URL = clip(n.URL, 200)
	switch n.Level {
	case "info", "warning", "critical":
	default:
		n.Level = "info"
	}
	n.Body = strings.ToValidUTF8(n.Body, "")
	data, _ := json.Marshal(n)
	for len(data) > maxPayload && n.Body != "" {
		runes := []rune(n.Body)
		cut := (len(data)-maxPayload)/6 + 1 // JSON escapes can widen a rune up to 6 bytes
		if cut >= len(runes) {
			n.Body = ""
		} else {
			n.Body = string(runes[:len(runes)-cut])
		}
		data, _ = json.Marshal(n)
	}
	if len(data) > maxPayload { // cannot happen with the clips above
		data, _ = json.Marshal(Notification{Title: "runlight", Level: n.Level})
	}
	return data
}
