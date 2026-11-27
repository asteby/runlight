package governor

// Check is one input the governor weighed, with the numbers it saw. The
// dashboard renders these as a checklist so every decision can be audited.
type Check struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	Limit float64 `json:"limit"` // threshold that flips OK
	Calm  float64 `json:"calm"`  // below this the host counts as calm (growth allowed); 0 = n/a
	Unit  string  `json:"unit"`
	OK    bool    `json:"ok"`
}

// Decision is a snapshot of the last Update: the limit it chose, why, and
// the evidence.
type Decision struct {
	Limit          int     `json:"limit"`
	Min            int     `json:"min"`
	Max            int     `json:"max"`
	InUse          int     `json:"inUse"`
	Reason         Reason  `json:"reason"`
	PSI            bool    `json:"psi"`
	MemPerRunnerMB uint64  `json:"memPerRunnerMb"`
	Checks         []Check `json:"checks"`
}

// Decision returns the explanation of the most recent Update.
func (g *Governor) Decision() Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.dec
	d.Checks = append([]Check(nil), g.dec.Checks...)
	return d
}

// Params returns the effective (defaulted) parameters.
func (g *Governor) Params() Params {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.p
}

// SetMemPerRunner updates the expected footprint of the next runner, e.g.
// from profiles learned on previous runs of the queued jobs.
func (g *Governor) SetMemPerRunner(mb uint64) {
	if mb == 0 {
		return
	}
	g.mu.Lock()
	g.p.MemPerRunnerMB = mb
	g.mu.Unlock()
}

// explain builds the checklist for the signals just evaluated. Caller holds mu.
func (g *Governor) explain(s Signals, inUse int) Decision {
	d := Decision{
		Limit: g.limit, Min: g.p.Min, Max: g.p.Max, InUse: inUse,
		Reason: g.lastReason, PSI: s.HasPSI, MemPerRunnerMB: g.p.MemPerRunnerMB,
	}
	add := func(key string, v, lim, calm float64, unit string, ok bool) {
		d.Checks = append(d.Checks, Check{Key: key, Value: v, Limit: lim, Calm: calm, Unit: unit, OK: ok})
	}
	if g.p.DiskMinFreeMB > 0 && s.DiskTotalMB > 0 {
		add("disk", float64(s.DiskFreeMB), float64(g.p.DiskMinFreeMB), 0, "MB", s.DiskFreeMB >= g.p.DiskMinFreeMB)
	}
	add("mem_reserve", float64(s.MemAvailMB), float64(g.p.MemReserveMB), 0, "MB", s.MemAvailMB >= g.p.MemReserveMB)
	f := g.p.CalmFactor
	if s.HasPSI {
		add("psi_cpu", s.CPUSome, g.p.CPUHigh, g.p.CPUHigh*f, "%", s.CPUSome < g.p.CPUHigh)
		add("psi_mem", s.MemSome, g.p.MemHigh, g.p.MemHigh*f, "%", s.MemSome < g.p.MemHigh)
		add("psi_io", s.IOSome, g.p.IOHigh, g.p.IOHigh*f, "%", s.IOSome < g.p.IOHigh)
	} else {
		add("load", s.LoadPerCore, g.p.LoadHigh, g.p.LoadHigh*f, "x", s.LoadPerCore < g.p.LoadHigh)
	}
	need := float64(g.p.MemPerRunnerMB + g.p.MemReserveMB)
	add("mem_fit", float64(s.MemAvailMB), need, 0, "MB", g.fitsAnother(s))
	add("demand", float64(inUse), float64(g.limit), 0, "runners", true)
	return d
}
