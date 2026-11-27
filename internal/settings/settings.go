// Package settings holds the live-editable configuration of runlight: a
// schema of typed fields, validation per field (messages in Spanish), atomic
// persistence of the overrides (only what differs from the defaults) and an
// audit trail of who changed what.
//
// The package is a pure store: it knows nothing about the governor, scaler or
// janitor. Callers register OnChange and apply the new values themselves.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is the value type of a field.
type Kind string

const (
	KindInt   Kind = "int"
	KindFloat Kind = "float"
	KindBool  Kind = "bool"
)

const (
	auditCap     = 200
	maxFileBytes = 1 << 20
	maxActorLen  = 64
)

// Field describes one editable setting.
type Field struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Help    string  `json:"help,omitempty"`
	Group   string  `json:"group"`
	Unit    string  `json:"unit,omitempty"`
	Kind    Kind    `json:"kind"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step,omitempty"`
	Default any     `json:"default"`
}

// Change is one modified key.
type Change struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// AuditEntry records one successful Update.
type AuditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Changes []Change  `json:"changes"`
}

// FieldErrors maps a key to a human-readable (Spanish) problem.
type FieldErrors map[string]string

func (e FieldErrors) Error() string {
	keys := slices.Sorted(maps.Keys(e))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e[k])
	}
	return "Ajustes no válidos: " + strings.Join(parts, "; ")
}

// Result is what Update reports.
type Result struct {
	Values    map[string]any
	Applied   []string
	Unchanged []string
}

// Option customises a Manager.
type Option func(*Manager)

// WithValidator adds cross-field rules (min <= max...). It receives the full
// set of effective values after the proposed change and returns errors keyed by
// the offending field.
func WithValidator(fn func(values map[string]any) FieldErrors) Option {
	return func(m *Manager) { m.validate = fn }
}

// Manager is safe for concurrent use.
type Manager struct {
	path     string
	fields   []Field
	byKey    map[string]Field
	validate func(map[string]any) FieldErrors

	mu        sync.Mutex
	defaults  map[string]any
	overrides map[string]any
	audit     []AuditEntry // newest first
	hooks     []func(changed []string, values map[string]any)
}

type overridesFile struct {
	Version   int            `json:"version"`
	Overrides map[string]any `json:"overrides"`
}

// Open loads (or starts) the settings stored at path. A missing file means "all
// defaults"; a corrupt one is set aside as <path>.corrupt-<n> and ignored.
// Stored keys that are no longer in the schema, or whose value is no longer
// valid, are dropped.
func Open(path string, fields []Field, opts ...Option) (*Manager, error) {
	if path == "" {
		return nil, errors.New("settings: se necesita una ruta de archivo")
	}
	m := &Manager{
		path:      path,
		byKey:     map[string]Field{},
		defaults:  map[string]any{},
		overrides: map[string]any{},
	}
	for _, o := range opts {
		o(m)
	}
	for _, f := range fields {
		if f.Key == "" {
			return nil, errors.New("settings: campo sin clave")
		}
		if _, dup := m.byKey[f.Key]; dup {
			return nil, fmt.Errorf("settings: clave duplicada %q", f.Key)
		}
		switch f.Kind {
		case KindInt, KindFloat, KindBool:
		default:
			return nil, fmt.Errorf("settings: %s: tipo desconocido %q", f.Key, f.Kind)
		}
		// Defaults come from the daemon's flags and may legitimately sit outside
		// the editable range, so only their type is checked.
		def, msg := coerce(f, f.Default, false)
		if msg != "" {
			return nil, fmt.Errorf("settings: %s: valor por defecto no válido: %s", f.Key, msg)
		}
		f.Default = def
		m.byKey[f.Key] = f
		m.fields = append(m.fields, f)
		m.defaults[f.Key] = def
	}

	raw, err := readLimited(path)
	switch {
	case err == nil:
		var file overridesFile
		if json.Unmarshal(raw, &file) != nil {
			quarantine(path)
		} else {
			for k, v := range file.Overrides {
				f, ok := m.byKey[k]
				if !ok {
					continue
				}
				if cv, msg := coerce(f, v, true); msg == "" && cv != m.defaults[k] {
					m.overrides[k] = cv
				}
			}
		}
	case errors.Is(err, os.ErrNotExist):
	case errors.Is(err, errTooBig):
		quarantine(path)
	default:
		return nil, fmt.Errorf("settings: leyendo %s: %w", path, err)
	}

	// Cross rules may have changed since the file was written: drop the
	// overrides they reject instead of starting with an inconsistent config.
	if m.validate != nil {
		for k := range m.validate(m.effective(m.overrides)) {
			delete(m.overrides, k)
		}
	}

	auditPath := path + ".audit.json"
	if raw, err := readLimited(auditPath); err == nil {
		if json.Unmarshal(raw, &m.audit) != nil {
			m.audit = nil
			quarantine(auditPath)
		}
	} else if errors.Is(err, errTooBig) {
		quarantine(auditPath)
	}
	if len(m.audit) > auditCap {
		m.audit = m.audit[:auditCap]
	}
	return m, nil
}

// Schema returns the fields in declaration order.
func (m *Manager) Schema() []Field { return slices.Clone(m.fields) }

// Defaults returns the default value of every key (an addition to the original
// contract; the HTTP layer exposes it as "defaults").
func (m *Manager) Defaults() map[string]any { return maps.Clone(m.defaults) }

// Values returns the effective values: defaults plus overrides.
func (m *Manager) Values() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.effective(m.overrides)
}

// Audit returns the change history, newest first (at most 200 entries).
func (m *Manager) Audit() []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AuditEntry, len(m.audit))
	for i, e := range m.audit {
		e.Changes = slices.Clone(e.Changes)
		out[i] = e
	}
	return out
}

// OnChange registers a callback invoked after a successful, persisted Update
// with the changed keys and the full effective values. It runs outside the
// manager's lock, so it may call Values or Audit.
func (m *Manager) OnChange(fn func(changed []string, values map[string]any)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.hooks = append(m.hooks, fn)
	m.mu.Unlock()
}

// Update validates every change and applies them all, or none: when anything is
// invalid the returned error is a FieldErrors and nothing is stored.
func (m *Manager) Update(actor string, changes map[string]any) (Result, error) {
	actor = cleanActor(actor)

	m.mu.Lock()
	cur := m.effective(m.overrides)

	errs := FieldErrors{}
	next := maps.Clone(cur)
	for k, v := range changes {
		f, ok := m.byKey[k]
		if !ok {
			errs[k] = "Ajuste desconocido."
			continue
		}
		cv, msg := coerce(f, v, true)
		if msg != "" {
			errs[k] = msg
			continue
		}
		next[k] = cv
	}
	if len(errs) == 0 && m.validate != nil {
		for k, msg := range m.validate(maps.Clone(next)) {
			errs[k] = msg
		}
	}
	if len(errs) > 0 {
		m.mu.Unlock()
		return Result{Values: cur}, errs
	}

	var applied, unchanged []string
	for k := range changes {
		if next[k] == cur[k] {
			unchanged = append(unchanged, k)
		} else {
			applied = append(applied, k)
		}
	}
	sort.Strings(applied)
	sort.Strings(unchanged)
	if len(applied) == 0 {
		m.mu.Unlock()
		return Result{Values: cur, Unchanged: unchanged}, nil
	}
	diff := make([]Change, 0, len(applied))
	for _, k := range applied {
		diff = append(diff, Change{Key: k, From: cur[k], To: next[k]})
	}

	newOver := maps.Clone(m.overrides)
	for _, k := range applied {
		if next[k] == m.defaults[k] {
			delete(newOver, k)
		} else {
			newOver[k] = next[k]
		}
	}
	if err := writeJSON(m.path, overridesFile{Version: 1, Overrides: newOver}); err != nil {
		m.mu.Unlock()
		return Result{Values: cur}, fmt.Errorf("no se pudo guardar la configuración: %w", err)
	}
	m.overrides = newOver

	m.audit = append([]AuditEntry{{At: time.Now().UTC(), Actor: actor, Changes: diff}}, m.audit...)
	if len(m.audit) > auditCap {
		m.audit = m.audit[:auditCap]
	}
	// The change is already stored and live; a failed audit write must not turn
	// it into an error for the caller.
	_ = writeJSON(m.path+".audit.json", m.audit)

	hooks := slices.Clone(m.hooks)
	m.mu.Unlock()

	for _, h := range hooks {
		call(h, slices.Clone(applied), maps.Clone(next))
	}
	return Result{Values: next, Applied: applied, Unchanged: unchanged}, nil
}

func call(h func([]string, map[string]any), changed []string, values map[string]any) {
	defer func() { _ = recover() }() // a faulty subscriber must not break the others
	h(changed, values)
}

func (m *Manager) effective(over map[string]any) map[string]any {
	out := make(map[string]any, len(m.defaults))
	maps.Copy(out, m.defaults)
	maps.Copy(out, over)
	return out
}

func cleanActor(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return "desconocido"
	}
	r := []rune(a)
	if len(r) > maxActorLen {
		r = r[:maxActorLen]
	}
	return string(r)
}

// coerce converts v to the field's Go type (int, float64 or bool). With check
// it also enforces the range.
func coerce(f Field, v any, check bool) (any, string) {
	if f.Kind == KindBool {
		b, ok := v.(bool)
		if !ok {
			return nil, "Debe ser sí o no (verdadero o falso)."
		}
		return b, ""
	}
	n, ok := toFloat(v)
	if !ok {
		return nil, "Debe ser un número."
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, "Debe ser un número finito."
	}
	if f.Kind == KindInt && n != math.Trunc(n) {
		return nil, "Debe ser un número entero."
	}
	if check && (n < f.Min || n > f.Max) {
		return nil, fmt.Sprintf("Debe estar entre %g y %g%s.", f.Min, f.Max, unitSuffix(f.Unit))
	}
	if f.Kind == KindInt {
		if math.Abs(n) > 1<<53 {
			return nil, "El número es demasiado grande."
		}
		return int(n), ""
	}
	return n, ""
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func unitSuffix(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}

var errTooBig = errors.New("archivo demasiado grande")

func readLimited(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxFileBytes {
		return nil, errTooBig
	}
	return os.ReadFile(path)
}

// quarantine moves an unreadable file aside so it is never lost nor retried.
func quarantine(path string) {
	for i := 1; i < 1000; i++ {
		dst := fmt.Sprintf("%s.corrupt-%d", path, i)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		_ = os.Rename(path, dst)
		return
	}
}

// writeJSON stores v atomically (temp file in the same directory + rename)
// with mode 0600; the directory is created 0700.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*") // created 0600
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
