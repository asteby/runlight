package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/governor"
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
