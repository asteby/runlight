package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dannyjp49/runlight/internal/governor"
	"github.com/dannyjp49/runlight/internal/scaler"
	"github.com/dannyjp49/runlight/internal/sysmon"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

// collector turns each control-loop tick into dashboard data: the time
// series, per-runner usage, the learned memory estimate and the decision log.
type collector struct {
	hub        *telemetry.Hub
	cores      int
	prodRel    string        // cgroup (relative) of production, e.g. system.slice
	runlightRl string        // cgroup of Runlight's runners, e.g. runlight.slice
	defMemMB   atomic.Uint64 // estimated memory per runner; changes live with the settings
	maxMemMB   uint64

	prev     map[string]cpuSample
	lastTick time.Time
	lastLim  int
	lastNote string

	// Swap section. The process scan walks /proc, so it runs at most every
	// swapScanEvery, never on each tick.
	scanSwap     func() []sysmon.SwapProc
	tmpfsOf      func(path string) (usedMB, totalMB uint64, ok bool)
	topSwap      []telemetry.SwapProc
	lastSwapScan time.Time
}

// swapScanEvery is how often the "who uses swap" list is refreshed.
const swapScanEvery = 30 * time.Second

type cpuSample struct {
	usec uint64
	at   time.Time
}

func newCollector(hub *telemetry.Hub, prodRel, runlightRel string, defMemMB, maxMemMB uint64) *collector {
	c := &collector{
		hub: hub, cores: runtime.NumCPU(),
		prodRel: prodRel, runlightRl: runlightRel,
		maxMemMB: maxMemMB,
		scanSwap: func() []sysmon.SwapProc { return sysmon.TopSwap(telemetry.MaxTopSwap) },
		tmpfsOf:  sysmon.Tmpfs,
		prev:     map[string]cpuSample{}, lastLim: -1,
	}
	c.defMemMB.Store(defMemMB)
	return c
}

// cpuPct converts the CPU time a cgroup used since the previous reading into
// a percentage of ONE core (so 200 means two cores busy).
func (c *collector) cpuPct(key string, usec uint64, now time.Time) float64 {
	p, ok := c.prev[key]
	c.prev[key] = cpuSample{usec, now}
	if !ok || usec < p.usec {
		return 0
	}
	dt := now.Sub(p.at).Microseconds()
	if dt <= 0 {
		return 0
	}
	return float64(usec-p.usec) / float64(dt) * 100
}

// UnitStats reads a runner's memory (bytes) for the scaler.
func (c *collector) UnitStats(name string) (current, peak uint64) {
	cg := sysmon.ReadCG(c.runlightRl + "/" + name + ".service")
	return cg.MemCurrent, cg.MemPeak
}

// record is called once per tick after the governor has decided.
func (c *collector) record(now time.Time, s governor.Signals, gov *governor.Governor, sc *scaler.Scaler, total, busy, limit int) {
	prod := sysmon.ReadCG(c.prodRel)
	rl := sysmon.ReadCG(c.runlightRl)
	whole := float64(c.cores) // % of one core -> % of the machine
	mb := func(b uint64) uint64 { return b >> 20 }

	p := telemetry.Point{
		T:       now.UnixMilli(),
		ProdCPU: s.CPUSome, ProdMem: s.MemSome, ProdIO: s.IOSome, HostCPU: s.HostCPUSome,
		Limit: limit, Runners: total, Busy: busy, Queued: c.hub.QueueDepth(),
		CPUProd:       c.cpuPct("prod", prod.CPUUsec, now) / whole,
		CPURunlight:   c.cpuPct("runlight", rl.CPUUsec, now) / whole,
		MemTotalMB:    s.MemTotalMB,
		MemProdMB:     mb(prod.MemCurrent),
		MemRunlightMB: mb(rl.MemCurrent),
		MemAvailMB:    s.MemAvailMB,
		SwapUsedMB:    s.SwapUsedMB, SwapTotalMB: s.SwapTotalMB,
		DiskFreeMB: s.DiskFreeMB, DiskTotalMB: s.DiskTotalMB,
		Load1: s.LoadPerCore * whole,
	}
	c.hub.AddPoint(p)
	c.hub.SetMemory(c.memoryInfo(now, s))

	for _, name := range sc.Names() {
		cg := sysmon.ReadCG(c.runlightRl + "/" + name + ".service")
		if !cg.OK {
			continue
		}
		c.hub.SetUsage(name, mb(cg.MemCurrent), mb(cg.MemPeak), c.cpuPct("r:"+name, cg.CPUUsec, now))
	}

	// Admission learns from history: size the next runner from the largest
	// profile among the jobs actually waiting.
	est := c.hub.EstimateMemMB(c.defMemMB.Load(), 256, c.maxMemMB)
	gov.SetMemPerRunner(est)

	dec := gov.Decision()
	c.hub.SetDecision(dec)
	if limit != c.lastLim {
		if c.lastLim >= 0 {
			c.hub.Eventf("capacity", "Capacidad %d → %d: %s.", c.lastLim, limit, explainES(dec, s))
		}
		c.lastLim = limit
	}
	c.lastTick = now
}

// explainES says, in plain Spanish, why the governor holds its current stance.
func explainES(d governor.Decision, s governor.Signals) string {
	check := func(key string) (governor.Check, bool) {
		for _, c := range d.Checks {
			if c.Key == key {
				return c, true
			}
		}
		return governor.Check{}, false
	}
	switch d.Reason {
	case governor.ReasonPressure:
		labels := map[string]string{"psi_cpu": "CPU", "psi_mem": "memoria", "psi_io": "E/S de disco", "load": "carga"}
		for _, k := range []string{"psi_cpu", "psi_mem", "psi_io", "load"} {
			if c, ok := check(k); ok && !c.OK {
				return fmt.Sprintf("la presión de %s en producción es %.0f%% y el límite es %.0f%%, así que se frena", labels[k], c.Value, c.Limit)
			}
		}
		return "producción sufre presión, así que se frena"
	case governor.ReasonLowMemory:
		c, _ := check("mem_reserve")
		return fmt.Sprintf("solo quedan %s de memoria disponible, por debajo de la reserva de %s", gb(c.Value), gb(c.Limit))
	case governor.ReasonLowDisk:
		c, _ := check("disk")
		return fmt.Sprintf("quedan %s de disco, por debajo del mínimo de %s", gb(c.Value), gb(c.Limit))
	case governor.ReasonNoHeadroom:
		c, _ := check("mem_fit")
		return fmt.Sprintf("no cabe otro runner: hacen falta %s libres y hay %s", gb(c.Limit), gb(c.Value))
	case governor.ReasonGrow:
		var parts []string
		if cpu, ok := check("psi_cpu"); ok {
			parts = append(parts, fmt.Sprintf("CPU %.0f%%", cpu.Value))
		}
		if mem, ok := check("psi_mem"); ok {
			parts = append(parts, fmt.Sprintf("memoria %.0f%%", mem.Value))
		}
		return fmt.Sprintf("producción está tranquila (%s) y hay %s libres, así que sube", strings.Join(parts, ", "), gb(float64(s.MemAvailMB)))
	}
	return "sin cambios"
}

func gb(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.0f MB", mb)
}

// memoryInfo builds the swap/tmpfs section for the dashboard. The expensive
// per-process swap scan is refreshed at most every swapScanEvery and only
// while something is in swap; the previous list is reused in between.
func (c *collector) memoryInfo(now time.Time, s governor.Signals) telemetry.MemoryInfo {
	if s.SwapUsedMB == 0 {
		c.topSwap, c.lastSwapScan = nil, time.Time{}
	} else if c.scanSwap != nil && now.Sub(c.lastSwapScan) >= swapScanEvery {
		c.lastSwapScan = now
		c.topSwap = c.topSwap[:0:0]
		for _, p := range c.scanSwap() {
			c.topSwap = append(c.topSwap, telemetry.SwapProc{PID: p.PID, Name: p.Name, MB: p.MB, Unit: p.Unit})
		}
	}
	var tmp []telemetry.TmpfsInfo
	if s.TmpfsTotalMB > 0 {
		tmp = append(tmp, telemetry.TmpfsInfo{Path: "/tmp", UsedMB: s.TmpfsUsedMB, TotalMB: s.TmpfsTotalMB})
	}
	if c.tmpfsOf != nil {
		if u, t, ok := c.tmpfsOf("/dev/shm"); ok && t > 0 {
			tmp = append(tmp, telemetry.TmpfsInfo{Path: "/dev/shm", UsedMB: u, TotalMB: t})
		}
	}
	psi := 0.0
	if s.HasPSI {
		psi = s.MemSome
	}
	return telemetry.BuildMemory(s.SwapUsedMB, s.SwapTotalMB, s.SwapInKBs, s.SwapOutKBs, psi, tmp, c.topSwap)
}
