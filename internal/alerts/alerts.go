// Package alerts is a pure, deterministic alert engine. It never reads the
// clock: every decision is a function of the Inputs handed to Evaluate (and of
// the state left by earlier calls).
package alerts

import (
	"context"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Severity string

const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

func (s Severity) rank() int {
	switch s {
	case Critical:
		return 2
	case Warning:
		return 1
	}
	return 0
}

type RuleID string

const (
	DiskLow       RuleID = "disk_low"
	SwapFull      RuleID = "swap_full"
	MemLow        RuleID = "mem_low"
	QueueStuck    RuleID = "queue_stuck"
	JobFailures   RuleID = "job_failures"
	ProdPressure  RuleID = "prod_pressure"
	DaemonRestart RuleID = "daemon_restart"
	JanitorStuck  RuleID = "janitor_stuck"
)

const (
	maxHistory     = 100
	noiseWindow    = 10 * time.Minute
	noiseMax       = 6
	maxHeld        = 50
	summaryTag     = "alerts-summary"
	summaryMaxList = 6
)

type FailedJob struct {
	Repo, Job string
	At        time.Time
}

type Inputs struct {
	Now                                               time.Time
	DiskFreeMB, DiskTotalMB, MemAvailMB, MemReserveMB uint64
	SwapUsedMB, SwapTotalMB                           uint64
	Runners, Busy, Limit                              int
	GovernorReason                                    string
	QueueDepth, QueueOldestWaitSec                    int
	ProdCPUPressure, ProdCPULimit                     float64
	FailedJobs                                        []FailedJob
	UnexpectedRestart                                 bool
	JanitorRuns                                       int
	JanitorFreedMBLast                                uint64
}

type RuleConfig struct {
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"`
}

type Config map[RuleID]RuleConfig

// DefaultConfig returns every rule enabled with its default threshold.
func DefaultConfig() Config {
	c := make(Config, len(specs))
	for _, s := range specs {
		c[s.id] = RuleConfig{Enabled: true, Threshold: s.def}
	}
	return c
}

type Notification struct {
	Title, Body, Tag, URL string
	Severity              Severity
	Resolved              bool
}

// Sink receives notifications. It is called synchronously from Evaluate (never
// while the engine lock is held), so a slow sink should hand off internally.
type Sink func(ctx context.Context, n Notification)

type Alert struct {
	ID            string     `json:"id"`
	Rule          RuleID     `json:"rule"`
	Severity      Severity   `json:"severity"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Since         time.Time  `json:"since"`
	LastNotified  time.Time  `json:"lastNotified"`
	Notifications int        `json:"notifications"`
	ResolvedAt    *time.Time `json:"resolvedAt"`
}

type RuleInfo struct {
	ID             RuleID     `json:"id"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	Severity       Severity   `json:"severity"`
	Unit           string     `json:"unit"`
	ThresholdLabel string     `json:"thresholdLabel"`
	Min            float64    `json:"min"`
	Max            float64    `json:"max"`
	Step           float64    `json:"step"`
	Config         RuleConfig `json:"config"`
}

type Snapshot struct {
	Active  []Alert    `json:"active"`
	History []Alert    `json:"history"`
	Rules   []RuleInfo `json:"rules"`
}

// RuleCatalog returns the static metadata of every rule with default config.
func RuleCatalog() []RuleInfo { return catalog(DefaultConfig()) }

func catalog(cfg Config) []RuleInfo {
	out := make([]RuleInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, RuleInfo{
			ID: s.id, Title: s.title, Description: s.desc, Severity: s.sev,
			Unit: s.unit, ThresholdLabel: s.label,
			Min: s.min, Max: s.max, Step: s.step,
			Config: cfg[s.id],
		})
	}
	return out
}

// keyState is the engine's memory for one alert key.
type keyState struct {
	pendingSince time.Time // condition true since (not yet alerting)
	clearSince   time.Time // clear condition true since (alerting)
}

type Engine struct {
	mu       sync.Mutex
	cfg      Config
	sink     Sink
	active   map[string]*Alert
	state    map[string]*keyState
	lastFail map[string]time.Time
	history  []Alert // newest first
	sent     []time.Time
	held     []Notification // resolved notices waiting for noise budget
}

func New(cfg Config, sink Sink) *Engine {
	return &Engine{
		cfg:      sanitize(cfg),
		sink:     sink,
		active:   map[string]*Alert{},
		state:    map[string]*keyState{},
		lastFail: map[string]time.Time{},
	}
}

// sanitize returns a complete, in-range copy: missing rules get defaults,
// NaN/Inf/non-positive thresholds fall back to the default, others are clamped.
func sanitize(in Config) Config {
	out := DefaultConfig()
	for _, s := range specs {
		rc, ok := in[s.id]
		if !ok {
			continue
		}
		th := s.def
		if s.max > 0 && !math.IsNaN(rc.Threshold) && !math.IsInf(rc.Threshold, 0) && rc.Threshold > 0 {
			th = math.Min(math.Max(rc.Threshold, s.min), s.max)
		}
		out[s.id] = RuleConfig{Enabled: rc.Enabled, Threshold: th}
	}
	return out
}

func copyCfg(c Config) Config {
	out := make(Config, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// SetConfig applies a new configuration live. Alerts of rules that became
// disabled are resolved silently (no notification).
func (e *Engine) SetConfig(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = sanitize(cfg)
	// Disabled rules are cleaned on the next Evaluate (we need a clock for
	// ResolvedAt); nothing else to do here.
}

func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return copyCfg(e.cfg)
}

func (e *Engine) st(key string) *keyState {
	s := e.state[key]
	if s == nil {
		s = &keyState{}
		e.state[key] = s
	}
	return s
}

// Evaluate advances the engine to in.Now. Calling it again with the same
// inputs never produces duplicate notifications.
func (e *Engine) Evaluate(ctx context.Context, in Inputs) {
	out := e.step(in)
	if e.sink == nil {
		return
	}
	for _, n := range out {
		e.sink(ctx, n)
	}
}

type pending struct {
	n   Notification
	key string // active alert key to mark as notified ("" for resolved/summary)
}

func (e *Engine) step(in Inputs) []Notification {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := in.Now

	var resolved []Notification
	for _, s := range specs {
		rc := e.cfg[s.id]
		if !rc.Enabled {
			e.silenceRule(s.id, now)
			continue
		}
		checks := s.eval(e, rc.Threshold, in)
		for _, c := range checks {
			if c.unknown {
				continue
			}
			if n, ok := e.apply(s, c, now); ok {
				resolved = append(resolved, n)
			}
		}
	}

	// Candidates for (re)notification, in rule order then key order.
	keys := make([]string, 0, len(e.active))
	for k := range e.active {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := ruleOrder(e.active[keys[i]].Rule), ruleOrder(e.active[keys[j]].Rule)
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	var cands []pending
	for _, k := range keys {
		a := e.active[k]
		s, _ := specByID(a.Rule)
		due := a.Notifications == 0 ||
			(s.repeat > 0 && now.Sub(a.LastNotified) >= s.repeat)
		if due {
			cands = append(cands, pending{n: Notification{
				Title: a.Title, Body: a.Body, Tag: a.ID, URL: alertsURL, Severity: a.Severity,
			}, key: k})
		}
	}
	return e.dispatch(now, cands, resolved)
}

// silenceRule resolves every active alert of a disabled rule without notice.
func (e *Engine) silenceRule(id RuleID, now time.Time) {
	for k, a := range e.active {
		if a.Rule == id {
			e.resolve(k, now)
		}
	}
	for k := range e.state {
		if k == string(id) || hasPrefix(k, string(id)+":") {
			delete(e.state, k)
		}
	}
	for k := range e.lastFail {
		if hasPrefix(k, string(id)+":") {
			delete(e.lastFail, k)
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// apply advances one key's state machine. It returns a "resolved" notice when
// the alert had been announced and the rule wants one.
func (e *Engine) apply(s ruleSpec, c check, now time.Time) (Notification, bool) {
	st := e.st(c.key)
	a := e.active[c.key]
	if a == nil {
		if !c.fire {
			st.pendingSince = time.Time{}
			if st.clearSince.IsZero() {
				delete(e.state, c.key)
			}
			return Notification{}, false
		}
		if st.pendingSince.IsZero() {
			st.pendingSince = now
		}
		hold := s.forDur
		if c.forOver > 0 {
			hold = c.forOver
		}
		if now.Sub(st.pendingSince) < hold {
			return Notification{}, false
		}
		e.active[c.key] = &Alert{
			ID: c.key, Rule: s.id, Severity: s.sev,
			Title: c.title, Body: c.body, Since: st.pendingSince,
		}
		st.pendingSince = time.Time{}
		st.clearSince = time.Time{}
		return Notification{}, false
	}

	a.Title, a.Body = c.title, c.body
	if !c.clear {
		st.clearSince = time.Time{}
		return Notification{}, false
	}
	if st.clearSince.IsZero() {
		st.clearSince = now
	}
	if now.Sub(st.clearSince) < s.clearDur {
		return Notification{}, false
	}
	announced := a.Notifications > 0
	e.resolve(c.key, now)
	if announced && !s.noResolved {
		return Notification{
			Title: "Resuelto: " + c.title, Body: c.okBody, Tag: c.key,
			URL: alertsURL, Severity: s.sev, Resolved: true,
		}, true
	}
	return Notification{}, false
}

func (e *Engine) resolve(key string, now time.Time) {
	a := e.active[key]
	if a == nil {
		return
	}
	t := now
	done := *a
	done.ResolvedAt = &t
	e.history = append([]Alert{done}, e.history...)
	if len(e.history) > maxHistory {
		e.history = e.history[:maxHistory]
	}
	delete(e.active, key)
	delete(e.state, key)
	delete(e.lastFail, key)
}

// dispatch applies the global noise budget (6 per 10 min). Firing/repeat
// candidates are regenerated every tick until announced, so only "resolved"
// notices need to be held across ticks.
func (e *Engine) dispatch(now time.Time, cands []pending, resolved []Notification) []Notification {
	// Merge held resolved notices with new ones; the latest per tag wins.
	all := append(append([]Notification{}, e.held...), resolved...)
	e.held = nil
	// A held resolved notice for an alert that fired again is stale.
	live := all[:0]
	for _, n := range all {
		if _, again := e.active[n.Tag]; again {
			continue
		}
		live = append(live, n)
	}
	all = dedupeByTag(live)

	items := make([]pending, 0, len(cands)+len(all))
	items = append(items, cands...)
	for _, n := range all {
		items = append(items, pending{n: n})
	}
	// Critical first; announcements before resolutions; stable otherwise.
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].n, items[j].n
		if a.Resolved != b.Resolved {
			return !a.Resolved
		}
		return a.Severity.rank() > b.Severity.rank()
	})

	// Prune the budget window.
	kept := e.sent[:0]
	for _, t := range e.sent {
		if now.Sub(t) < noiseWindow {
			kept = append(kept, t)
		}
	}
	e.sent = kept
	budget := noiseMax - len(e.sent)

	var out []Notification
	var send []pending
	switch {
	case len(items) == 0:
		return nil
	case budget <= 0:
		// Nothing can go out; keep resolved notices for later.
		e.holdResolved(items)
		return nil
	case len(items) <= budget:
		send = items
		for _, it := range send {
			out = append(out, it.n)
		}
	default:
		singles := items[:budget-1]
		rest := items[budget-1:]
		for _, it := range singles {
			out = append(out, it.n)
		}
		out = append(out, summarize(rest))
		send = items
	}
	for _, it := range send {
		if it.key != "" {
			if a := e.active[it.key]; a != nil {
				a.LastNotified = now
				a.Notifications++
			}
		}
	}
	for range out {
		e.sent = append(e.sent, now)
	}
	return out
}

func (e *Engine) holdResolved(items []pending) {
	for _, it := range items {
		if it.key == "" {
			e.held = append(e.held, it.n)
		}
	}
	if len(e.held) > maxHeld {
		e.held = e.held[len(e.held)-maxHeld:]
	}
}

func dedupeByTag(in []Notification) []Notification {
	last := map[string]int{}
	for i, n := range in {
		last[n.Tag] = i
	}
	out := make([]Notification, 0, len(in))
	for i, n := range in {
		if last[n.Tag] == i {
			out = append(out, n)
		}
	}
	return out
}

func summarize(rest []pending) Notification {
	sev := Info
	titles := make([]string, 0, len(rest))
	for _, it := range rest {
		if it.n.Severity.rank() > sev.rank() {
			sev = it.n.Severity
		}
		titles = append(titles, it.n.Title)
	}
	n := len(rest)
	list := limitList(titles, summaryMaxList)
	body := joinSemi(list)
	return Notification{
		Title:    strconv.Itoa(n) + " " + plural(n, "aviso más", "avisos más"),
		Body:     strconv.Itoa(n) + " " + plural(n, "aviso más", "avisos más") + ": " + body + ".",
		Tag:      summaryTag,
		URL:      alertsURL,
		Severity: sev,
	}
}

func joinSemi(l []string) string {
	s := ""
	for i, x := range l {
		if i > 0 {
			s += "; "
		}
		s += x
	}
	return s
}

// Snapshot returns copies of the active alerts (critical first, then newest),
// the history (newest first, max 100) and the rule catalog with live config.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	active := make([]Alert, 0, len(e.active))
	for _, a := range e.active {
		active = append(active, *a)
	}
	sort.Slice(active, func(i, j int) bool {
		if ri, rj := active[i].Severity.rank(), active[j].Severity.rank(); ri != rj {
			return ri > rj
		}
		if !active[i].Since.Equal(active[j].Since) {
			return active[i].Since.After(active[j].Since)
		}
		return active[i].ID < active[j].ID
	})
	hist := make([]Alert, len(e.history))
	copy(hist, e.history)
	return Snapshot{Active: active, History: hist, Rules: catalog(e.cfg)}
}
