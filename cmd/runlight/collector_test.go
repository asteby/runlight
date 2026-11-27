package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/governor"
	"github.com/dannyjp49/runlight/internal/sysmon"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

func decisionFor(t *testing.T, s governor.Signals, inUse int) (governor.Decision, governor.Signals) {
	t.Helper()
	g := governor.New(governor.Params{Max: 8})
	g.Update(time.Unix(1_000_000, 0), s, inUse)
	return g.Decision(), s
}

func calm() governor.Signals {
	return governor.Signals{HasPSI: true, MemAvailMB: 8192, DiskFreeMB: 50000, DiskTotalMB: 100000}
}

func TestExplainPressureNamesTheCulprit(t *testing.T) {
	s := calm()
	s.CPUSome = 72
	d, s := decisionFor(t, s, 1)
	got := explainES(d, s)
	if !strings.Contains(got, "CPU") || !strings.Contains(got, "72%") || !strings.Contains(got, "60%") {
		t.Fatalf("explanation = %q", got)
	}
}

func TestExplainLowMemoryAndDisk(t *testing.T) {
	s := calm()
	s.MemAvailMB = 100
	d, s := decisionFor(t, s, 1)
	if got := explainES(d, s); !strings.Contains(got, "memoria disponible") {
		t.Fatalf("memory: %q", got)
	}

	s = calm()
	s.DiskFreeMB = 500
	d, s = decisionFor(t, s, 1)
	if got := explainES(d, s); !strings.Contains(got, "disco") {
		t.Fatalf("disk: %q", got)
	}
}

func TestExplainNoHeadroomMentionsBothNumbers(t *testing.T) {
	s := calm()
	s.MemAvailMB = 1100
	d, s := decisionFor(t, s, 1)
	got := explainES(d, s)
	if !strings.Contains(got, "no cabe") || !strings.Contains(got, "1.2 GB") || !strings.Contains(got, "1.1 GB") {
		t.Fatalf("explanation = %q", got)
	}
}

func TestExplainGrow(t *testing.T) {
	// One calm update with demand grows the limit.
	g := governor.New(governor.Params{Max: 8})
	g.Update(time.Unix(1_000_000, 0), calm(), 1)
	if got := explainES(g.Decision(), calm()); !strings.Contains(got, "tranquila") {
		t.Fatalf("explanation = %q", got)
	}
}

func TestCPUPctIsAPercentageOfOneCore(t *testing.T) {
	c := newCollector(nil, "system.slice", "runlight.slice", 1024, 8192)
	t0 := time.Unix(1_000_000, 0)
	if v := c.cpuPct("x", 0, t0); v != 0 {
		t.Fatalf("first reading must be 0, got %v", v)
	}
	// 2 seconds of CPU time over 1 second of wall clock = 200% of one core.
	if v := c.cpuPct("x", 2_000_000, t0.Add(time.Second)); v < 199.9 || v > 200.1 {
		t.Fatalf("got %v, want 200", v)
	}
	// A counter reset (cgroup recreated) must not produce a negative spike.
	if v := c.cpuPct("x", 10, t0.Add(2*time.Second)); v != 0 {
		t.Fatalf("counter reset gave %v", v)
	}
}

func newMemCollector(scans *int) *collector {
	c := newCollector(telemetry.New(telemetry.Config{}), "system.slice", "runlight.slice", 1024, 4096)
	c.scanSwap = func() []sysmon.SwapProc {
		*scans++
		return []sysmon.SwapProc{{PID: 7, Name: "mariadbd", MB: 2310, Unit: "mariadb"}}
	}
	c.tmpfsOf = func(p string) (uint64, uint64, bool) { return 0, 0, false }
	return c
}

func TestMemoryInfoFullIdleSwapIsNotAWarning(t *testing.T) {
	scans := 0
	c := newMemCollector(&scans)
	s := governor.Signals{HasPSI: true, SwapUsedMB: 6144, SwapTotalMB: 6144, TmpfsUsedMB: 6350, TmpfsTotalMB: 12288}
	m := c.memoryInfo(time.Unix(1000, 0), s)
	if m.Verdict != "idle" || !strings.Contains(m.Note, "6.2 GB") || len(m.TopSwap) != 1 || m.TopSwap[0].Name != "mariadbd" ||
		len(m.Tmpfs) != 1 || m.Tmpfs[0].Path != "/tmp" {
		t.Fatalf("%+v", m)
	}
}

func TestMemoryInfoScansSwapEvery30sNotEveryTick(t *testing.T) {
	scans := 0
	c := newMemCollector(&scans)
	s := governor.Signals{SwapUsedMB: 100, SwapTotalMB: 6144}
	t0 := time.Unix(1000, 0)
	for i := 0; i < 15; i++ { // 30 s of 2 s ticks
		c.memoryInfo(t0.Add(time.Duration(2*i)*time.Second), s)
	}
	if scans != 1 {
		t.Fatalf("scans in the first 30 s = %d, want 1", scans)
	}
	c.memoryInfo(t0.Add(30*time.Second), s)
	if scans != 2 {
		t.Fatalf("scans after 30 s = %d, want 2", scans)
	}
	// The cached list is served between scans.
	if m := c.memoryInfo(t0.Add(32*time.Second), s); len(m.TopSwap) != 1 || scans != 2 {
		t.Fatalf("cached = %+v scans=%d", m.TopSwap, scans)
	}
}

func TestMemoryInfoNoSwapNeverScans(t *testing.T) {
	scans := 0
	c := newMemCollector(&scans)
	m := c.memoryInfo(time.Unix(1000, 0), governor.Signals{SwapTotalMB: 6144})
	if scans != 0 || m.Verdict != "none" || len(m.TopSwap) != 0 {
		t.Fatalf("scans=%d %+v", scans, m)
	}
}

func TestMemoryInfoActiveAndShm(t *testing.T) {
	scans := 0
	c := newMemCollector(&scans)
	c.tmpfsOf = func(p string) (uint64, uint64, bool) { return 12, 11731, p == "/dev/shm" }
	s := governor.Signals{SwapUsedMB: 6144, SwapTotalMB: 6144, SwapInKBs: 800, SwapOutKBs: 120}
	m := c.memoryInfo(time.Unix(1000, 0), s)
	if m.Verdict != "active" || !strings.Contains(m.Note, "entran 800 KB/s y salen 120 KB/s") ||
		len(m.Tmpfs) != 1 || m.Tmpfs[0].Path != "/dev/shm" {
		t.Fatalf("%+v", m)
	}
	// A memory stall in production alone also means paging.
	s = governor.Signals{HasPSI: true, MemSome: 3, SwapUsedMB: 10, SwapTotalMB: 6144}
	if c.memoryInfo(time.Unix(2000, 0), s).Verdict != "active" {
		t.Fatal("PSI mem 3 % must be active")
	}
}
