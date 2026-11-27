// Package telemetry is the in-memory flight recorder behind the dashboard:
// time series, the decision log, live runners, the queue and the per-job
// resource profiles Runlight learns. Everything is bounded and every method
// is safe on a nil *Hub, so callers never need to guard.
package telemetry

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	maxPoints = 1800 // one hour at a 2s tick
	maxEvents = 400
	maxJobs   = 100
	maxFails  = 200
	ewmaAlpha = 0.3
)

// Config is static, set once at startup.
type Config struct {
	Version    string `json:"version"`
	Backend    string `json:"backend"`
	Host       string `json:"host"`
	ScaleSet   string `json:"scaleSet"`
	URL        string `json:"url"`
	Cores      int    `json:"cores"`
	MemTotalMB uint64 `json:"memTotalMb"`
	Min        int    `json:"min"`
	Max        int    `json:"max"`
	PSIScope   string `json:"psiScope"`
	IdleTTLSec int    `json:"idleTtlSec"`
	MaxRunMin  int    `json:"maxRunMin"`
}

// Point is one sample of the time series.
type Point struct {
	T int64 `json:"t"` // unix ms

	// Pressure the governor acts on (production scope) vs the whole host.
	ProdCPU float64 `json:"prodCpu"`
	ProdMem float64 `json:"prodMem"`
	ProdIO  float64 `json:"prodIo"`
	HostCPU float64 `json:"hostCpu"`

	Limit   int `json:"limit"`
	Runners int `json:"runners"`
	Busy    int `json:"busy"`
	Queued  int `json:"queued"`

	// CPU use as a percentage of the whole machine, per group.
	CPUProd     float64 `json:"cpuProd"`
	CPURunlight float64 `json:"cpuRunlight"`

	// Memory in MB, per group.
	MemTotalMB    uint64  `json:"memTotalMb"`
	MemProdMB     uint64  `json:"memProdMb"`
	MemRunlightMB uint64  `json:"memRunlightMb"`
	MemAvailMB    uint64  `json:"memAvailMb"`
	SwapUsedMB    uint64  `json:"swapUsedMb"`
	SwapTotalMB   uint64  `json:"swapTotalMb"`
	DiskFreeMB    uint64  `json:"diskFreeMb"`
	DiskTotalMB   uint64  `json:"diskTotalMb"`
	Load1         float64 `json:"load1"`
}

// Event is one line of the decision log.
type Event struct {
	T    int64  `json:"t"`
	Kind string `json:"kind"` // capacity | scale | job | reap | keep | warn | info
	Msg  string `json:"msg"`
}

// RunnerInfo is a live runner.
type RunnerInfo struct {
	Name       string  `json:"name"`
	State      string  `json:"state"` // idle | busy
	StartedMs  int64   `json:"startedMs"`
	Why        string  `json:"why"`
	Repo       string  `json:"repo,omitempty"`
	Job        string  `json:"job,omitempty"`
	JobStartMs int64   `json:"jobStartMs,omitempty"`
	MemMB      uint64  `json:"memMb"`
	PeakMB     uint64  `json:"peakMb"`
	CPUPct     float64 `json:"cpuPct"` // of one core
}

// QueuedJob is a job waiting for a runner.
type QueuedJob struct {
	ID       int64    `json:"id"`
	Repo     string   `json:"repo"`
	Job      string   `json:"job"`
	Labels   []string `json:"labels"`
	QueuedMs int64    `json:"queuedMs"`
}

// JobRecord is a finished job.
type JobRecord struct {
	Runner     string `json:"runner"`
	Repo       string `json:"repo"`
	Job        string `json:"job"`
	Result     string `json:"result"`
	QueueSec   int    `json:"queueSec"`
	RunSec     int    `json:"runSec"`
	PeakMemMB  uint64 `json:"peakMemMb"`
	FinishedMs int64  `json:"finishedMs"`
}

// Profile is what Runlight has learned about a job from past runs.
type Profile struct {
	Key       string  `json:"key"`
	Runs      int     `json:"runs"`
	AvgRunSec float64 `json:"avgRunSec"`
	PeakMemMB float64 `json:"peakMemMb"` // moving average of observed peaks
	MaxMemMB  uint64  `json:"maxMemMb"`
	LastMs    int64   `json:"lastMs"`
}

// Totals counts outcomes since start.
type Totals struct {
	Started   int `json:"started"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
}

// CacheStat is one capped cache as the janitor last left it.
type CacheStat struct {
	Name        string `json:"name"`
	Dir         string `json:"dir"`
	Mode        string `json:"mode"`
	SizeMB      uint64 `json:"sizeMb"`
	CapMB       uint64 `json:"capMb"`
	FreedMB     uint64 `json:"freedMb"` // in the last pass
	LastPruneMs int64  `json:"lastPruneMs"`
}

// JanitorStat is the janitor's running tally.
type JanitorStat struct {
	Runs      int    `json:"runs"`
	FreedMB   uint64 `json:"freedMb"`     // since start
	LastFreed uint64 `json:"lastFreedMb"` // in the last pass
	LastRunMs int64  `json:"lastRunMs"`
	DryRun    bool   `json:"dryRun"`
	Enabled   bool   `json:"enabled"`
}

// State is the JSON the dashboard polls.
type State struct {
	Now       int64        `json:"now"`
	StartedMs int64        `json:"startedMs"`
	Config    Config       `json:"config"`
	Decision  any          `json:"decision"`
	Point     *Point       `json:"point"`
	Runners   []RunnerInfo `json:"runners"`
	Queue     []QueuedJob  `json:"queue"`
	Jobs      []JobRecord  `json:"jobs"`
	Profiles  []Profile    `json:"profiles"`
	Events    []Event      `json:"events"`
	Totals    Totals       `json:"totals"`
	Caches    []CacheStat  `json:"caches"`
	Janitor   JanitorStat  `json:"janitor"`
	Paused    bool         `json:"paused"` // scaling is paused: no new runners start
}

// Failure is a job that ended badly, kept briefly for the alert engine.
type Failure struct {
	Repo, Job string
	At        time.Time
}

// Hub holds everything. Create it with New.
type Hub struct {
	mu       sync.Mutex
	cfg      Config
	started  time.Time
	decision any
	points   []Point
	events   []Event
	runners  map[string]*RunnerInfo
	queue    map[int64]*QueuedJob
	jobs     []JobRecord
	profiles map[string]*Profile
	totals   Totals
	caches   []CacheStat
	janitor  JanitorStat
	failures []Failure
	paused   bool
}

func New(cfg Config) *Hub {
	return &Hub{
		cfg: cfg, started: time.Now(),
		runners:  map[string]*RunnerInfo{},
		queue:    map[int64]*QueuedJob{},
		profiles: map[string]*Profile{},
	}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// SetConfig replaces the static configuration (e.g. once the limits are known).
func (h *Hub) SetConfig(c Config) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.cfg = c
	h.mu.Unlock()
}

// SetDecision stores the governor's latest explanation (any JSON-able value).
func (h *Hub) SetDecision(d any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.decision = d
	h.mu.Unlock()
}

// SetCaches replaces the cache table shown by the dashboard.
func (h *Hub) SetCaches(c []CacheStat) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.caches = append([]CacheStat(nil), c...)
	h.janitor.Enabled = true
	h.mu.Unlock()
}

// JanitorRan accounts for one janitor pass.
func (h *Hub) JanitorRan(freedMB uint64, dry bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.janitor.Runs++
	h.janitor.FreedMB += freedMB
	h.janitor.LastFreed = freedMB
	h.janitor.LastRunMs = ms(time.Now())
	h.janitor.DryRun = dry
	h.janitor.Enabled = true
	h.mu.Unlock()
}

// AddPoint appends a time-series sample, dropping the oldest beyond one hour.
func (h *Hub) AddPoint(p Point) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.points = append(h.points, p)
	if n := len(h.points); n > maxPoints {
		h.points = append([]Point(nil), h.points[n-maxPoints:]...)
	}
	h.mu.Unlock()
}

// Event records a line in the decision log.
func (h *Hub) Event(kind, msg string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.events = append(h.events, Event{T: ms(time.Now()), Kind: kind, Msg: msg})
	if n := len(h.events); n > maxEvents {
		h.events = append([]Event(nil), h.events[n-maxEvents:]...)
	}
	h.mu.Unlock()
}

// Eventf is Event with formatting.
func (h *Hub) Eventf(kind, format string, a ...any) {
	if h == nil {
		return
	}
	h.Event(kind, fmt.Sprintf(format, a...))
}

func (h *Hub) RunnerStarted(name, why string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.runners[name] = &RunnerInfo{Name: name, State: "idle", StartedMs: ms(time.Now()), Why: why}
	h.totals.Started++
	h.mu.Unlock()
}

func (h *Hub) RunnerGone(name string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	delete(h.runners, name)
	h.mu.Unlock()
}

// SetUsage records live resource use for a runner.
func (h *Hub) SetUsage(name string, memMB, peakMB uint64, cpuPct float64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if r, ok := h.runners[name]; ok {
		r.MemMB, r.CPUPct = memMB, cpuPct
		r.PeakMB = max(r.PeakMB, peakMB, memMB)
	}
	h.mu.Unlock()
}

func (h *Hub) JobQueued(id int64, repo, job string, labels []string, at time.Time) {
	if h == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	h.mu.Lock()
	if _, ok := h.queue[id]; !ok {
		h.queue[id] = &QueuedJob{ID: id, Repo: repo, Job: job, Labels: labels, QueuedMs: ms(at)}
	}
	h.mu.Unlock()
}

func (h *Hub) JobStarted(id int64, runner, repo, job string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	delete(h.queue, id)
	if r, ok := h.runners[runner]; ok {
		r.State, r.Repo, r.Job, r.JobStartMs = "busy", repo, job, ms(time.Now())
	}
	h.mu.Unlock()
}

// JobDropped removes a job that left the queue without ever running (for
// example cancelled while waiting).
func (h *Hub) JobDropped(id int64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	delete(h.queue, id)
	h.mu.Unlock()
}

// JobDone records a finished job and folds its footprint into the profile.
func (h *Hub) JobDone(id int64, runner, repo, job, result string, queue, run time.Duration, peakMB uint64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.queue, id)
	h.jobs = append(h.jobs, JobRecord{
		Runner: runner, Repo: repo, Job: job, Result: result,
		QueueSec: int(queue.Seconds()), RunSec: int(run.Seconds()), PeakMemMB: peakMB, FinishedMs: ms(time.Now()),
	})
	if n := len(h.jobs); n > maxJobs {
		h.jobs = append([]JobRecord(nil), h.jobs[n-maxJobs:]...)
	}
	switch result {
	case "succeeded":
		h.totals.Succeeded++
	case "canceled", "cancelled", "abandoned":
		h.totals.Canceled++
	default:
		h.totals.Failed++
		h.failures = append(h.failures, Failure{Repo: repo, Job: job, At: time.Now()})
		if n := len(h.failures); n > maxFails {
			h.failures = append([]Failure(nil), h.failures[n-maxFails:]...)
		}
	}
	// Only successful, measured runs teach us a footprint: a job killed early
	// would understate its needs.
	if result != "succeeded" || peakMB == 0 || repo == "" {
		return
	}
	key := repo + " · " + job
	p := h.profiles[key]
	if p == nil {
		p = &Profile{Key: key, PeakMemMB: float64(peakMB), AvgRunSec: run.Seconds()}
		h.profiles[key] = p
	} else {
		p.PeakMemMB = ewmaAlpha*float64(peakMB) + (1-ewmaAlpha)*p.PeakMemMB
		p.AvgRunSec = ewmaAlpha*run.Seconds() + (1-ewmaAlpha)*p.AvgRunSec
	}
	p.Runs++
	p.MaxMemMB = max(p.MaxMemMB, peakMB)
	p.LastMs = ms(time.Now())
}

// EstimateMemMB predicts the footprint of the next runner from what is
// queued: the largest learned profile among waiting jobs (with 20% headroom),
// or def when nothing queued has been seen before. The result is clamped.
func (h *Hub) EstimateMemMB(def, lo, hi uint64) uint64 {
	if h == nil {
		return def
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var best float64
	for _, q := range h.queue {
		if p := h.profiles[q.Repo+" · "+q.Job]; p != nil && p.PeakMemMB > best {
			best = p.PeakMemMB
		}
	}
	if best == 0 {
		return def
	}
	return min(max(uint64(best*1.2), lo), hi)
}

// QueueDepth is the number of jobs waiting for a runner.
func (h *Hub) QueueDepth() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue)
}

// History returns the samples newer than sinceMs.
func (h *Hub) History(sinceMs int64) []Point {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	i := sort.Search(len(h.points), func(i int) bool { return h.points[i].T > sinceMs })
	return append([]Point(nil), h.points[i:]...)
}

// Snapshot builds the dashboard state.
func (h *Hub) Snapshot() State {
	if h == nil {
		return State{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := State{
		Now: ms(time.Now()), StartedMs: ms(h.started), Config: h.cfg, Decision: h.decision, Totals: h.totals,
		Runners: []RunnerInfo{}, Queue: []QueuedJob{}, Jobs: []JobRecord{}, Profiles: []Profile{}, Events: []Event{},
		Caches: append([]CacheStat{}, h.caches...), Janitor: h.janitor, Paused: h.paused,
	}
	if n := len(h.points); n > 0 {
		p := h.points[n-1]
		st.Point = &p
	}
	for _, r := range h.runners {
		st.Runners = append(st.Runners, *r)
	}
	sort.Slice(st.Runners, func(i, j int) bool { return st.Runners[i].StartedMs < st.Runners[j].StartedMs })
	for _, q := range h.queue {
		st.Queue = append(st.Queue, *q)
	}
	sort.Slice(st.Queue, func(i, j int) bool { return st.Queue[i].QueuedMs < st.Queue[j].QueuedMs })
	for i := len(h.jobs) - 1; i >= 0 && len(st.Jobs) < 30; i-- { // newest first
		st.Jobs = append(st.Jobs, h.jobs[i])
	}
	for _, p := range h.profiles {
		st.Profiles = append(st.Profiles, *p)
	}
	sort.Slice(st.Profiles, func(i, j int) bool { return st.Profiles[i].Runs > st.Profiles[j].Runs })
	if len(st.Profiles) > 25 {
		st.Profiles = st.Profiles[:25]
	}
	for i := len(h.events) - 1; i >= 0 && len(st.Events) < 150; i-- { // newest first
		st.Events = append(st.Events, h.events[i])
	}
	return st
}

// SetPaused records whether scaling is paused, for the dashboard.
func (h *Hub) SetPaused(p bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.paused = p
	h.mu.Unlock()
}

// RecentFailures returns the failed jobs recorded at or after since, oldest
// first.
func (h *Hub) RecentFailures(since time.Time) []Failure {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Failure
	for _, f := range h.failures {
		if !f.At.Before(since) {
			out = append(out, f)
		}
	}
	return out
}

// QueueOldestWaitSec is how long the longest-waiting queued job has waited.
func (h *Hub) QueueOldestWaitSec(now time.Time) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest int64
	for _, q := range h.queue {
		if oldest == 0 || q.QueuedMs < oldest {
			oldest = q.QueuedMs
		}
	}
	if oldest == 0 {
		return 0
	}
	return max(0, int((ms(now)-oldest)/1000))
}

// JanitorStats returns the janitor's running tally.
func (h *Hub) JanitorStats() JanitorStat {
	if h == nil {
		return JanitorStat{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.janitor
}
