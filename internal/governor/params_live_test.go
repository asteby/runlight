package governor

import (
	"testing"
	"time"
)

func TestUpdateParamsClampsLimitIntoNewRange(t *testing.T) {
	g := newGov(8)
	g.mu.Lock()
	g.limit = 6
	g.mu.Unlock()
	g.UpdateParams(func(p *Params) { p.Max = 3 })
	if got := g.Limit(); got != 3 {
		t.Fatalf("limit = %d, want clamped to 3", got)
	}
	g.UpdateParams(func(p *Params) { p.Min = 2 })
	g.mu.Lock()
	g.limit = 0
	g.mu.Unlock()
	g.UpdateParams(func(p *Params) { p.Max = 4 })
	if got := g.Limit(); got != 2 {
		t.Fatalf("limit = %d, want raised to min 2", got)
	}
}

func TestUpdateParamsKeepsProtectionsOnZero(t *testing.T) {
	g := newGov(4)
	g.UpdateParams(func(p *Params) { p.MemReserveMB = 0; p.DiskMinFreeMB = 0; p.CPUHigh = 0 })
	p := g.Params()
	if p.MemReserveMB == 0 || p.DiskMinFreeMB == 0 || p.CPUHigh == 0 {
		t.Fatalf("zeros must fall back to defaults: %+v", p)
	}
}

func TestUpdateParamsAppliesThresholdsAndNilIsNoop(t *testing.T) {
	g := newGov(4)
	g.UpdateParams(nil)
	g.UpdateParams(func(p *Params) { p.CPUHigh = 10 })
	s := calmSignals()
	s.CPUSome = 20
	g.Update(t0.Add(time.Minute), s, 1)
	if g.Reason() != ReasonPressure {
		t.Fatalf("reason = %s, want pressure with the lowered threshold", g.Reason())
	}
}

func TestUpdateParamsConcurrent(t *testing.T) {
	g := newGov(8)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			g.UpdateParams(func(p *Params) { p.Max = 2 + i%6 })
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		g.Update(t0.Add(time.Duration(i)*time.Minute), calmSignals(), 1)
		_ = g.Limit()
	}
	<-done
}
