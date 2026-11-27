// Package governor decides how many runners the host can afford right now.
//
// It is an AIMD controller (additive increase, multiplicative decrease — the
// same idea as TCP congestion control) driven by Linux Pressure Stall
// Information. PSI measures time tasks actually spent stalled waiting for
// CPU / memory / IO, which is a far better "is this box suffering?" signal
// than utilisation percentages.
//
// The governor is pure: it owns no goroutines, clocks or I/O. Callers feed it
// Signals and a timestamp, which makes it trivially testable.
package governor

import (
	"sync"
	"time"
)

// Signals is a point-in-time view of host health.
type Signals struct {
	// PSI "some" avg10 values, in percent (0-100). Valid only if HasPSI.
	CPUSome float64
	MemSome float64
	IOSome  float64
	HasPSI  bool

	// Fallback when PSI is unavailable: 1-minute load average divided by cores.
	LoadPerCore float64

	// Host-wide CPU stall, kept for comparison when the pressure above is
	// scoped to a production cgroup (shows what a naive governor would see).
	HostCPUSome float64

	MemTotalMB  uint64
	MemAvailMB  uint64
	SwapTotalMB uint64
	SwapUsedMB  uint64
	DiskFreeMB  uint64
	DiskTotalMB uint64

	// Swap traffic (KB/s, mean between samples, from /proc/vmstat pswpin/pswpout)
	// and the RAM-backed /tmp (statfs; 0 when /tmp is not a tmpfs).
	SwapInKBs, SwapOutKBs     float64
	TmpfsUsedMB, TmpfsTotalMB uint64
}

// Params tunes the controller. Zero values are replaced by Defaults().
type Params struct {
	Min int // floor for the limit (0 allows scale-to-zero)
	Max int // ceiling for the limit

	// Pressure thresholds (percent of time stalled over 10s).
	CPUHigh, MemHigh, IOHigh float64
	// Fraction of *High below which the host counts as calm.
	CalmFactor float64
	// Fallback load/core thresholds when PSI is missing.
	LoadHigh float64

	// Expected memory footprint of one more runner. Used to veto growth.
	MemPerRunnerMB uint64
	// Never let available memory drop below this because of us.
	MemReserveMB uint64
	// Stop admitting runners below this much free disk.
	DiskMinFreeMB uint64

	IncreaseEvery time.Duration // min gap between +1 steps
	DecreaseEvery time.Duration // min gap between halvings
}

// Defaults returns conservative parameters suited to small VPSs.
func Defaults() Params {
	return Params{
		Min:            0,
		Max:            4,
		CPUHigh:        60,
		MemHigh:        8,
		IOHigh:         50,
		CalmFactor:     0.5,
		LoadHigh:       1.5,
		MemPerRunnerMB: 1024,
		MemReserveMB:   256,
		DiskMinFreeMB:  2048,
		IncreaseEvery:  15 * time.Second,
		DecreaseEvery:  5 * time.Second,
	}
}

func (p Params) withDefaults() Params {
	d := Defaults()
	if p.Max <= 0 {
		p.Max = d.Max
	}
	if p.Min < 0 {
		p.Min = 0
	}
	if p.Min > p.Max {
		p.Min = p.Max
	}
	if p.CPUHigh <= 0 {
		p.CPUHigh = d.CPUHigh
	}
	if p.MemHigh <= 0 {
		p.MemHigh = d.MemHigh
	}
	if p.IOHigh <= 0 {
		p.IOHigh = d.IOHigh
	}
	if p.CalmFactor <= 0 || p.CalmFactor >= 1 {
		p.CalmFactor = d.CalmFactor
	}
	if p.LoadHigh <= 0 {
		p.LoadHigh = d.LoadHigh
	}
	if p.MemPerRunnerMB == 0 {
		p.MemPerRunnerMB = d.MemPerRunnerMB
	}
	// A zero must never mean "protection off": a hand-built Params once ran in
	// production with no memory reserve and no disk guard.
	if p.MemReserveMB == 0 {
		p.MemReserveMB = d.MemReserveMB
	}
	if p.DiskMinFreeMB == 0 {
		p.DiskMinFreeMB = d.DiskMinFreeMB
	}
	if p.IncreaseEvery <= 0 {
		p.IncreaseEvery = d.IncreaseEvery
	}
	if p.DecreaseEvery <= 0 {
		p.DecreaseEvery = d.DecreaseEvery
	}
	return p
}

// Reason explains the last decision; surfaced in logs and metrics.
type Reason string

const (
	ReasonSteady     Reason = "steady"
	ReasonGrow       Reason = "grow"
	ReasonPressure   Reason = "pressure"
	ReasonLowMemory  Reason = "low-memory"
	ReasonLowDisk    Reason = "low-disk"
	ReasonNoHeadroom Reason = "no-headroom"
)

// Governor holds the current concurrency limit.
type Governor struct {
	mu         sync.Mutex
	p          Params
	limit      int
	lastUp     time.Time
	lastDown   time.Time
	lastReason Reason
	dec        Decision
}

// New returns a governor that starts cautiously at max(Min, 1).
func New(p Params) *Governor {
	p = p.withDefaults()
	start := max(p.Min, 1)
	start = min(start, p.Max)
	return &Governor{p: p, limit: start, lastReason: ReasonSteady}
}

// UpdateParams changes the tuning live. fn receives a copy of the current
// parameters; the result is normalised like in New (zeros never mean "off") and
// the current limit is clamped into [Min, Max]. Durations and thresholds take
// effect on the next Update.
func (g *Governor) UpdateParams(fn func(*Params)) {
	if fn == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.p
	fn(&p)
	g.p = p.withDefaults()
	g.limit = max(g.p.Min, min(g.limit, g.p.Max))
}

// Limit is the number of runners that may exist concurrently.
func (g *Governor) Limit() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// Reason returns why the limit last changed (or stayed).
func (g *Governor) Reason() Reason {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastReason
}

// Update feeds fresh signals and returns the new limit. inUse is the number of
// runners currently alive (idle + busy). Growth only happens when there is
// demand (inUse has reached the limit), so an idle host never inflates it.
// The governor never revokes running jobs: lowering the limit only blocks
// *new* runners.
func (g *Governor) Update(now time.Time, s Signals, inUse int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	limit := g.update(now, s, inUse)
	g.dec = g.explain(s, inUse)
	return limit
}

// update is the control law; the caller holds mu.
func (g *Governor) update(now time.Time, s Signals, inUse int) int {
	// Hard guards first: these freeze growth immediately, no cooldown.
	if g.p.DiskMinFreeMB > 0 && s.DiskTotalMB > 0 && s.DiskFreeMB < g.p.DiskMinFreeMB {
		g.limit = max(g.p.Min, min(g.limit, inUse))
		g.lastReason = ReasonLowDisk
		return g.limit
	}
	if s.MemAvailMB < g.p.MemReserveMB {
		g.limit = max(g.p.Min, min(g.limit, inUse))
		g.lastReason = ReasonLowMemory
		return g.limit
	}

	switch {
	case g.stressed(s):
		if now.Sub(g.lastDown) >= g.p.DecreaseEvery {
			g.limit = max(g.p.Min, g.limit/2)
			g.lastDown = now
		}
		g.lastReason = ReasonPressure
	case g.calm(s) && inUse >= g.limit && g.limit < g.p.Max:
		if !g.fitsAnother(s) {
			g.lastReason = ReasonNoHeadroom
			break
		}
		if now.Sub(g.lastUp) >= g.p.IncreaseEvery && now.Sub(g.lastDown) >= g.p.IncreaseEvery {
			g.limit++
			g.lastUp = now
			g.lastReason = ReasonGrow
		}
	default:
		g.lastReason = ReasonSteady
	}
	return g.limit
}

func (g *Governor) stressed(s Signals) bool {
	if s.HasPSI {
		return s.CPUSome >= g.p.CPUHigh || s.MemSome >= g.p.MemHigh || s.IOSome >= g.p.IOHigh
	}
	return s.LoadPerCore >= g.p.LoadHigh
}

func (g *Governor) calm(s Signals) bool {
	f := g.p.CalmFactor
	if s.HasPSI {
		return s.CPUSome < g.p.CPUHigh*f && s.MemSome < g.p.MemHigh*f && s.IOSome < g.p.IOHigh*f
	}
	return s.LoadPerCore < g.p.LoadHigh*f
}

// fitsAnother reports whether memory can absorb one more runner while
// keeping the reserve intact.
func (g *Governor) fitsAnother(s Signals) bool {
	return s.MemAvailMB >= g.p.MemPerRunnerMB+g.p.MemReserveMB
}
