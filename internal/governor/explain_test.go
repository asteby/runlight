package governor

import (
	"testing"
	"time"
)

func find(d Decision, key string) (Check, bool) {
	for _, c := range d.Checks {
		if c.Key == key {
			return c, true
		}
	}
	return Check{}, false
}

func TestDecisionExplainsEveryInput(t *testing.T) {
	g := newGov(8)
	s := calmSignals()
	s.CPUSome = 40 // above the 60*0.5 calm line? no: below High(60), above calm(30)
	g.Update(t0.Add(time.Minute), s, 1)

	d := g.Decision()
	for _, k := range []string{"disk", "mem_reserve", "psi_cpu", "psi_mem", "psi_io", "mem_fit", "demand"} {
		if _, ok := find(d, k); !ok {
			t.Errorf("missing check %q", k)
		}
	}
	c, _ := find(d, "psi_cpu")
	if c.Value != 40 || c.Limit != 60 || c.Calm != 30 || !c.OK {
		t.Fatalf("psi_cpu = %+v", c)
	}
	if d.Max != 8 || d.Limit != g.Limit() {
		t.Fatalf("decision header wrong: %+v", d)
	}
}

func TestDecisionMarksFailingCheck(t *testing.T) {
	g := newGov(8)
	s := calmSignals()
	s.MemAvailMB = 100 // below reserve
	g.Update(t0.Add(time.Minute), s, 1)

	d := g.Decision()
	if c, _ := find(d, "mem_reserve"); c.OK {
		t.Fatal("mem_reserve should fail")
	}
	if d.Reason != ReasonLowMemory {
		t.Fatalf("reason = %s", d.Reason)
	}
}

func TestDecisionUsesLoadWhenNoPSI(t *testing.T) {
	g := newGov(8)
	g.Update(t0.Add(time.Minute), Signals{LoadPerCore: 0.4, MemAvailMB: 8192, DiskFreeMB: 50000, DiskTotalMB: 100000}, 0)
	d := g.Decision()
	if _, ok := find(d, "load"); !ok {
		t.Fatal("expected load check without PSI")
	}
	if _, ok := find(d, "psi_cpu"); ok {
		t.Fatal("psi checks must not appear without PSI")
	}
}

func TestSetMemPerRunnerChangesAdmission(t *testing.T) {
	g := newGov(8)
	s := calmSignals()
	s.MemAvailMB = 1100 // fits a light job, not the default 1024+256

	g.Update(t0.Add(time.Minute), s, 1)
	if g.Reason() != ReasonNoHeadroom {
		t.Fatalf("default footprint should not fit, reason=%s", g.Reason())
	}
	g.SetMemPerRunner(300) // learned: this queue is light
	g.Update(t0.Add(2*time.Minute), s, 1)
	if g.Limit() != 2 {
		t.Fatalf("a light profile should admit another runner, limit=%d", g.Limit())
	}
}

func TestDecisionIsACopy(t *testing.T) {
	g := newGov(8)
	g.Update(t0.Add(time.Minute), calmSignals(), 0)
	d := g.Decision()
	d.Checks[0].Value = -1
	if g.Decision().Checks[0].Value == -1 {
		t.Fatal("Decision must return a copy")
	}
}

// Regression: Params{} used to leave the memory reserve and the disk guard at
// zero, which silently disabled both protections in production.
func TestZeroValueParamsKeepTheSafetyGuards(t *testing.T) {
	g := New(Params{Max: 4})
	s := calmSignals()
	s.MemAvailMB = 100
	g.Update(t0.Add(time.Minute), s, 1)
	if g.Reason() != ReasonLowMemory {
		t.Fatalf("zero-value Params must still enforce a memory reserve, reason=%s", g.Reason())
	}

	g = New(Params{Max: 4})
	s = calmSignals()
	s.DiskFreeMB = 500
	g.Update(t0.Add(time.Minute), s, 1)
	if g.Reason() != ReasonLowDisk {
		t.Fatalf("zero-value Params must still enforce a disk guard, reason=%s", g.Reason())
	}
	if p := g.Params(); p.MemReserveMB == 0 || p.DiskMinFreeMB == 0 {
		t.Fatalf("effective params = %+v", p)
	}
}
