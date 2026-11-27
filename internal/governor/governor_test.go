package governor

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func calmSignals() Signals {
	return Signals{HasPSI: true, MemAvailMB: 8192, DiskFreeMB: 50000, DiskTotalMB: 100000}
}

func newGov(max int) *Governor {
	p := Defaults()
	p.Max = max
	return New(p)
}

func TestStartsCautiously(t *testing.T) {
	if got := newGov(8).Limit(); got != 1 {
		t.Fatalf("initial limit = %d, want 1", got)
	}
}

func TestGrowsOnlyWithDemand(t *testing.T) {
	g := newGov(8)
	// Idle host (inUse 0 < limit 1): must not grow even when calm.
	now := t0.Add(time.Minute)
	if got := g.Update(now, calmSignals(), 0); got != 1 {
		t.Fatalf("grew without demand: %d", got)
	}
	// Saturated and calm: +1 per IncreaseEvery.
	if got := g.Update(now, calmSignals(), 1); got != 2 {
		t.Fatalf("limit = %d, want 2", got)
	}
	// Cooldown: immediate second update does not grow again.
	if got := g.Update(now.Add(time.Second), calmSignals(), 2); got != 2 {
		t.Fatalf("grew during cooldown: %d", got)
	}
	if got := g.Update(now.Add(16*time.Second), calmSignals(), 2); got != 3 {
		t.Fatalf("limit = %d, want 3", got)
	}
}

func TestNeverExceedsMax(t *testing.T) {
	g := newGov(2)
	now := t0.Add(time.Minute)
	for range 10 {
		g.Update(now, calmSignals(), g.Limit())
		now = now.Add(20 * time.Second)
	}
	if got := g.Limit(); got != 2 {
		t.Fatalf("limit = %d, want 2", got)
	}
}

func TestPressureHalvesWithCooldown(t *testing.T) {
	g := newGov(16)
	now := t0.Add(time.Minute)
	for range 15 {
		g.Update(now, calmSignals(), g.Limit())
		now = now.Add(20 * time.Second)
	}
	before := g.Limit()
	if before < 8 {
		t.Fatalf("setup: limit = %d", before)
	}
	hot := calmSignals()
	hot.MemSome = 20
	after := g.Update(now, hot, before)
	if after != before/2 {
		t.Fatalf("after pressure = %d, want %d", after, before/2)
	}
	if g.Reason() != ReasonPressure {
		t.Fatalf("reason = %s", g.Reason())
	}
	// Within DecreaseEvery: no second halving.
	if again := g.Update(now.Add(time.Second), hot, after); again != after {
		t.Fatalf("halved twice inside cooldown: %d", again)
	}
	// After cooldown: halves again.
	if again := g.Update(now.Add(6*time.Second), hot, after); again != after/2 {
		t.Fatalf("limit = %d, want %d", again, after/2)
	}
}

func TestPressureNeverBelowMin(t *testing.T) {
	p := Defaults()
	p.Min, p.Max = 2, 8
	g := New(p)
	hot := calmSignals()
	hot.CPUSome = 99
	now := t0
	for range 10 {
		g.Update(now, hot, 2)
		now = now.Add(10 * time.Second)
	}
	if got := g.Limit(); got != 2 {
		t.Fatalf("limit = %d, want floor 2", got)
	}
}

func TestLowMemoryFreezesAtInUse(t *testing.T) {
	g := newGov(8)
	now := t0.Add(time.Minute)
	for range 6 {
		g.Update(now, calmSignals(), g.Limit())
		now = now.Add(20 * time.Second)
	}
	s := calmSignals()
	s.MemAvailMB = 100 // below reserve
	got := g.Update(now, s, 3)
	if got != 3 {
		t.Fatalf("limit = %d, want frozen at inUse=3", got)
	}
	if g.Reason() != ReasonLowMemory {
		t.Fatalf("reason = %s", g.Reason())
	}
}

func TestLowDiskFreezes(t *testing.T) {
	g := newGov(8)
	s := calmSignals()
	s.DiskFreeMB = 500
	g.Update(t0.Add(time.Minute), s, 1)
	if g.Reason() != ReasonLowDisk {
		t.Fatalf("reason = %s", g.Reason())
	}
}

func TestNoGrowthWhenMemoryDoesNotFit(t *testing.T) {
	g := newGov(8)
	s := calmSignals()
	s.MemAvailMB = 1100 // above reserve, below perRunner+reserve (1280)
	got := g.Update(t0.Add(time.Minute), s, 1)
	if got != 1 || g.Reason() != ReasonNoHeadroom {
		t.Fatalf("limit=%d reason=%s", got, g.Reason())
	}
}

func TestLoadFallbackWithoutPSI(t *testing.T) {
	g := newGov(8)
	now := t0.Add(time.Minute)
	for range 4 {
		g.Update(now, Signals{LoadPerCore: 0.2, MemAvailMB: 8192, DiskFreeMB: 50000, DiskTotalMB: 100000}, g.Limit())
		now = now.Add(20 * time.Second)
	}
	grown := g.Limit()
	if grown < 3 {
		t.Fatalf("did not grow on calm load: %d", grown)
	}
	g.Update(now, Signals{LoadPerCore: 3, MemAvailMB: 8192, DiskFreeMB: 50000, DiskTotalMB: 100000}, grown)
	if g.Limit() >= grown {
		t.Fatalf("did not shrink on high load: %d", g.Limit())
	}
}

func TestParamsSanitised(t *testing.T) {
	g := New(Params{Min: 10, Max: 3})
	if g.Limit() > 3 {
		t.Fatalf("limit %d exceeds max", g.Limit())
	}
}
