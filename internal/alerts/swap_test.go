package alerts

import (
	"strings"
	"testing"
	"time"
)

// swapIn is a swap at `used` of 4096 MB with the given traffic (KB/s).
func swapIn(d time.Duration, used uint64, in, out float64) Inputs {
	x := healthy(d)
	x.SwapTotalMB, x.SwapUsedMB = 4096, used
	x.SwapInKBs, x.SwapOutKBs = in, out
	return x
}

func TestSwapFullFiresOnlyWhilePaging(t *testing.T) {
	e, r := newEngine()
	for i := 0; i <= 59; i++ { // 118 s: not yet
		run(e, swapIn(sec(2*i), 4000, 300, 400)) // 97.7 %, 700 KB/s
	}
	if len(r.take()) != 0 {
		t.Fatal("must wait 120 s")
	}
	run(e, swapIn(sec(120), 4000, 300, 400))
	got := r.take()
	if len(got) != 1 || got[0].Severity != Warning || got[0].Tag != "swap_full" {
		t.Fatalf("got %+v", got)
	}
	for _, want := range []string{"97.7 %", "Está paginando", "entran 300 KB/s", "salen 400 KB/s"} {
		if !strings.Contains(got[0].Body, want) {
			t.Fatalf("body %q lacks %q", got[0].Body, want)
		}
	}
	if strings.Contains(got[0].Body, "al límite de memoria") {
		t.Fatal("the old false claim must be gone")
	}
	// repeat only every 12 h
	run(e, swapIn(sec(120)+min(719), 4000, 300, 400))
	if len(r.take()) != 0 {
		t.Fatal("no repeat before 12 h")
	}
	run(e, swapIn(sec(120)+min(720), 4000, 300, 400))
	if len(r.take()) != 1 {
		t.Fatal("repeat at 12 h")
	}
}

func TestSwapFullIdleNeverFires(t *testing.T) {
	// The real VPS case: swap 100 % full, 12 GB of RAM free, no traffic.
	e, r := newEngine()
	for i := 0; i < 1800; i++ { // one hour
		run(e, swapIn(sec(2*i), 4096, 0, 0))
	}
	if got := r.take(); len(got) != 0 || len(e.Snapshot().Active) != 0 {
		t.Fatalf("full-but-idle swap must not alert: %+v", got)
	}
	// Light traffic below 512 KB/s is still not paging.
	for i := 0; i < 600; i++ {
		run(e, swapIn(sec(3600+2*i), 4096, 100, 411))
	}
	if len(r.take()) != 0 {
		t.Fatal("511 KB/s is below the activity threshold")
	}
}

func TestSwapFullBriefBurstDoesNotFire(t *testing.T) {
	e, r := newEngine()
	for i := 0; i < 30; i++ { // 58 s of paging, then calm
		run(e, swapIn(sec(2*i), 4000, 900, 0))
	}
	for i := 30; i < 400; i++ {
		run(e, swapIn(sec(2*i), 4000, 0, 0))
	}
	if len(r.take()) != 0 {
		t.Fatal("a burst shorter than 120 s must not alert")
	}
}

func TestSwapFullNeedsThreshold(t *testing.T) {
	e, r := newEngine()
	for i := 0; i < 300; i++ {
		run(e, swapIn(sec(2*i), 2048, 5000, 5000)) // 50 % but paging hard
	}
	if len(r.take()) != 0 {
		t.Fatal("swap below the threshold never alerts")
	}
}

func TestSwapFullLowMemoryWithoutTraffic(t *testing.T) {
	e, r := newEngine()
	mk := func(d time.Duration) Inputs {
		x := swapIn(d, 4000, 0, 0)
		x.MemAvailMB = 3000 // < 1.5 x 2048
		return x
	}
	for i := 0; i <= 60; i++ {
		run(e, mk(sec(2*i)))
	}
	var sw []Notification
	for _, n := range r.take() {
		if n.Tag == "swap_full" {
			sw = append(sw, n)
		}
	}
	if len(sw) != 1 || !strings.Contains(sw[0].Body, "poca memoria") {
		t.Fatalf("got %+v", sw)
	}
}

func TestSwapFullHysteresis(t *testing.T) {
	e, r := newEngine()
	for i := 0; i <= 60; i++ {
		run(e, swapIn(sec(2*i), 4000, 600, 0))
	}
	if len(r.take()) != 1 {
		t.Fatal("expected the alert")
	}
	// 90 % and still paging: inside the band, stays.
	run(e, swapIn(sec(130), 3686, 600, 0))
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("90 % while paging keeps the alert")
	}
	// Traffic between the fire (512) and clear (256) lines: stays.
	run(e, swapIn(sec(132), 4000, 300, 0))
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("300 KB/s keeps the alert")
	}
	// Paging stopped and memory is fine: resolves even though swap is full.
	run(e, swapIn(sec(134), 4000, 0, 0))
	got := r.take()
	if len(got) != 1 || !got[0].Resolved {
		t.Fatalf("resolve = %+v", got)
	}
	// Swap drained below threshold-10 resolves too.
	e2, r2 := newEngine()
	for i := 0; i <= 60; i++ {
		run(e2, swapIn(sec(2*i), 4000, 600, 0))
	}
	r2.take()
	run(e2, swapIn(sec(130), 3277, 600, 0)) // 80 %
	if g := r2.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("drain resolve = %+v", g)
	}
}

func tmpIn(d time.Duration, used, total uint64) Inputs {
	x := healthy(d)
	x.TmpfsUsedMB, x.TmpfsTotalMB = used, total
	return x
}

func TestTmpRAMFiresAndResolves(t *testing.T) {
	e, r := newEngine()
	for i := 0; i < 149; i++ { // 296 s
		run(e, tmpIn(sec(2*i), 6350, 12288))
	}
	if len(r.take()) != 0 {
		t.Fatal("must wait 300 s")
	}
	run(e, tmpIn(sec(300), 6350, 12288))
	got := r.take()
	if len(got) != 1 || got[0].Severity != Warning || got[0].Tag != "tmp_ram" {
		t.Fatalf("got %+v", got)
	}
	for _, want := range []string{"/tmp ocupa 6.2 GB de RAM", "51.7 %", "acaban en swap", "TMPDIR"} {
		if !strings.Contains(got[0].Body, want) {
			t.Fatalf("body %q lacks %q", got[0].Body, want)
		}
	}
	// repeat every 6 h
	run(e, tmpIn(sec(300)+min(359), 6350, 12288))
	if len(r.take()) != 0 {
		t.Fatal("no repeat before 6 h")
	}
	run(e, tmpIn(sec(300)+min(360), 6350, 12288))
	if len(r.take()) != 1 {
		t.Fatal("repeat at 6 h")
	}
	// 3.5 GB is below 4 but above 80 % (3.2 GB): stays.
	run(e, tmpIn(sec(300)+min(361), 3584, 12288))
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("3.5 GB is inside the hysteresis band")
	}
	run(e, tmpIn(sec(300)+min(362), 3000, 12288))
	if g := r.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("resolve = %+v", g)
	}
}

func TestTmpRAMDoesNotFire(t *testing.T) {
	e, r := newEngine()
	for i := 0; i < 600; i++ {
		run(e, tmpIn(sec(2*i), 4000, 12288)) // 3.9 GB: under 4
	}
	for i := 600; i < 1200; i++ {
		run(e, tmpIn(sec(2*i), 60000, 0)) // not a tmpfs: total 0
	}
	if len(r.take()) != 0 {
		t.Fatal("under the threshold or not tmpfs must not alert")
	}
}

func TestTmpRAMResolvesWhenTmpStopsBeingTmpfs(t *testing.T) {
	e, r := newEngine()
	for i := 0; i <= 150; i++ {
		run(e, tmpIn(sec(2*i), 6350, 12288))
	}
	r.take()
	run(e, tmpIn(sec(310), 0, 0))
	if g := r.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("resolve = %+v", g)
	}
}

func TestTmpRAMThresholdRespected(t *testing.T) {
	cfg := DefaultConfig()
	cfg[TmpRAM] = RuleConfig{Enabled: true, Threshold: 8}
	r := &rec{}
	e := New(cfg, r.sink)
	for i := 0; i < 300; i++ {
		run(e, tmpIn(sec(2*i), 6350, 12288))
	}
	if len(r.take()) != 0 {
		t.Fatal("6.2 GB is under a custom 8 GB threshold")
	}
}
