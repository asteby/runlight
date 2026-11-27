package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type rec struct {
	mu  sync.Mutex
	got []Notification
}

func (r *rec) sink(_ context.Context, n Notification) {
	r.mu.Lock()
	r.got = append(r.got, n)
	r.mu.Unlock()
}

func (r *rec) take() []Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

func newEngine() (*Engine, *rec) {
	r := &rec{}
	return New(DefaultConfig(), r.sink), r
}

// healthy returns inputs that trigger nothing, at t0+d.
func healthy(d time.Duration) Inputs {
	return Inputs{
		Now:         t0.Add(d),
		DiskFreeMB:  100 * 1024,
		DiskTotalMB: 200 * 1024,
		MemAvailMB:  8192, MemReserveMB: 2048,
		SwapUsedMB: 0, SwapTotalMB: 4096,
		Runners: 2, Busy: 1, Limit: 3,
		GovernorReason:  "steady",
		ProdCPUPressure: 5, ProdCPULimit: 60,
	}
}

// lowDisk: 3.6 % free of 197 GB.
func lowDisk(d time.Duration) Inputs {
	in := healthy(d)
	in.DiskTotalMB = 197 * 1024
	in.DiskFreeMB = 7270 // 7.1 GB = 3.6 %
	return in
}

func run(e *Engine, in Inputs) { e.Evaluate(context.Background(), in) }

func sec(n int) time.Duration { return time.Duration(n) * time.Second }
func min(n int) time.Duration { return time.Duration(n) * time.Minute }

func activeIDs(e *Engine) []string {
	var ids []string
	for _, a := range e.Snapshot().Active {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestHealthyIsSilent(t *testing.T) {
	e, r := newEngine()
	for i := 0; i < 200; i++ {
		run(e, healthy(sec(2*i)))
	}
	if n := r.take(); len(n) != 0 {
		t.Fatalf("unexpected notifications: %+v", n)
	}
	s := e.Snapshot()
	if len(s.Active) != 0 || len(s.History) != 0 || len(s.Rules) != 9 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestDiskLowFiresAfterFor(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(30)))
	run(e, lowDisk(sec(59)))
	if len(r.take()) != 0 || len(e.Snapshot().Active) != 0 {
		t.Fatal("must not alert before 60 s")
	}
	in := lowDisk(sec(60))
	in.GovernorReason = "low-disk"
	run(e, in)
	got := r.take()
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	n := got[0]
	if n.Severity != Critical || n.Resolved || n.Tag != "disk_low" || n.URL != "/#/alertas" {
		t.Fatalf("bad notification %+v", n)
	}
	want := "Quedan 7.1 GB libres de 197 GB (3.6 %). runlight dejó de arrancar runners."
	if n.Body != want {
		t.Fatalf("body = %q, want %q", n.Body, want)
	}
	a := e.Snapshot().Active
	if len(a) != 1 || a[0].Notifications != 1 || !a[0].Since.Equal(t0) || a[0].ResolvedAt != nil {
		t.Fatalf("active = %+v", a)
	}
}

func TestPendingResetsWhenConditionFlickers(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(50)))
	run(e, healthy(sec(52))) // recovers
	run(e, lowDisk(sec(54)))
	run(e, lowDisk(sec(100))) // only 46 s since the new start
	if len(r.take()) != 0 {
		t.Fatal("flicker must restart the for timer")
	}
	run(e, lowDisk(sec(114)))
	if len(r.take()) != 1 {
		t.Fatal("must fire 60 s after the condition restarted")
	}
}

func TestIdempotentPerTick(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	for i := 0; i < 5; i++ {
		run(e, lowDisk(sec(60)))
	}
	if n := r.take(); len(n) != 1 {
		t.Fatalf("same tick repeated gave %d notifications", len(n))
	}
}

func TestDiskLowRepeatsEvery30Min(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	r.take()
	run(e, lowDisk(sec(60)+min(29)))
	if len(r.take()) != 0 {
		t.Fatal("must not repeat before 30 min")
	}
	run(e, lowDisk(sec(60)+min(30)))
	got := r.take()
	if len(got) != 1 || got[0].Tag != "disk_low" || got[0].Resolved {
		t.Fatalf("repeat = %+v", got)
	}
	if e.Snapshot().Active[0].Notifications != 2 {
		t.Fatal("notifications counter must be 2")
	}
}

func TestDiskLowHysteresisAndResolution(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	r.take()
	mid := func(d time.Duration) Inputs { // 14 %: above 12 but not above 15
		in := healthy(d)
		in.DiskFreeMB = 28 * 1024
		return in
	}
	for i := 0; i < 60; i++ {
		run(e, mid(sec(62+2*i)))
	}
	if len(e.Snapshot().Active) != 1 || len(r.take()) != 0 {
		t.Fatal("inside the hysteresis band the alert must stay, quietly")
	}
	base := sec(200)
	run(e, healthy(base)) // 50 % starts clear timer
	run(e, healthy(base+sec(59)))
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("must stay active until clear condition held 60 s")
	}
	run(e, healthy(base+sec(60)))
	got := r.take()
	if len(got) != 1 || !got[0].Resolved || got[0].Tag != "disk_low" ||
		!strings.HasPrefix(got[0].Title, "Resuelto: ") || got[0].Severity != Critical {
		t.Fatalf("resolve notice = %+v", got)
	}
	s := e.Snapshot()
	if len(s.Active) != 0 || len(s.History) != 1 || s.History[0].ResolvedAt == nil ||
		!s.History[0].ResolvedAt.Equal(t0.Add(base+sec(60))) {
		t.Fatalf("snapshot after resolve = %+v", s)
	}
}

func TestClearTimerResetsOnRelapse(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	r.take()
	run(e, healthy(sec(100)))
	run(e, lowDisk(sec(130))) // relapse
	run(e, healthy(sec(140)))
	run(e, healthy(sec(190))) // only 50 s of clear
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("clear timer must restart after a relapse")
	}
	run(e, healthy(sec(200)))
	if len(e.Snapshot().Active) != 0 {
		t.Fatal("should have resolved")
	}
}

func TestUnknownDiskDataIsIgnored(t *testing.T) {
	e, r := newEngine()
	in := healthy(0)
	in.DiskTotalMB, in.DiskFreeMB = 0, 0
	for i := 0; i < 100; i++ {
		in.Now = t0.Add(sec(2 * i))
		run(e, in)
	}
	if len(r.take()) != 0 {
		t.Fatal("missing data must not alert")
	}
}

func TestSwapWithoutSwapNeverAlerts(t *testing.T) {
	e, r := newEngine()
	in := healthy(0)
	in.SwapTotalMB, in.SwapUsedMB = 0, 0
	for i := 0; i < 600; i++ {
		in.Now = t0.Add(sec(2 * i))
		run(e, in)
	}
	if len(r.take()) != 0 {
		t.Fatal("no swap, no alert")
	}
}

func TestMemLow(t *testing.T) {
	e, r := newEngine()
	low := func(d time.Duration, avail uint64) Inputs {
		in := healthy(d)
		in.MemReserveMB, in.MemAvailMB = 2048, avail // threshold 3072
		return in
	}
	run(e, low(0, 3071))
	run(e, low(sec(119), 3071))
	if len(r.take()) != 0 {
		t.Fatal("must wait 120 s")
	}
	run(e, low(sec(120), 3071))
	got := r.take()
	if len(got) != 1 || got[0].Tag != "mem_low" || !strings.Contains(got[0].Body, "3.0 GB") {
		t.Fatalf("got %+v", got)
	}
	// exactly at the threshold (3072) is not "below"; 3500 is inside band (<3686.4)
	run(e, low(sec(130), 3500))
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("band must hold the alert")
	}
	run(e, low(sec(140), 3700))
	if g := r.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("resolve = %+v", g)
	}
}

func TestMemLowZeroReserveNeverFires(t *testing.T) {
	e, r := newEngine()
	in := healthy(0)
	in.MemReserveMB, in.MemAvailMB = 0, 0
	for i := 0; i < 200; i++ {
		in.Now = t0.Add(sec(2 * i))
		run(e, in)
	}
	if len(r.take()) != 0 {
		t.Fatal("zero reserve must not alert")
	}
}

func stuck(d time.Duration, wait int) Inputs {
	in := healthy(d)
	in.QueueDepth, in.QueueOldestWaitSec = 4, wait
	return in
}

func TestQueueStuckCauses(t *testing.T) {
	cases := []struct {
		name   string
		tweak  func(*Inputs)
		expect string
	}{
		{"pressure", func(i *Inputs) { i.GovernorReason = "pressure" }, "presión"},
		{"low-memory", func(i *Inputs) { i.GovernorReason = "low-memory" }, "Falta memoria"},
		{"low-disk", func(i *Inputs) { i.GovernorReason = "low-disk" }, "espacio en disco"},
		{"no-headroom", func(i *Inputs) { i.GovernorReason = "no-headroom" }, "margen"},
		{"no runners", func(i *Inputs) { i.Runners, i.Busy = 0, 0 }, "ningún runner"},
		{"all busy", func(i *Inputs) { i.Runners, i.Busy, i.Limit = 3, 3, 3 }, "Los 3 runners permitidos están ocupados"},
		{"idle runners", func(i *Inputs) { i.Runners, i.Busy, i.Limit = 3, 1, 3 }, "2 de 3 runners libres"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, r := newEngine()
			mk := func(d time.Duration) Inputs { in := stuck(d, 20*60); c.tweak(&in); return in }
			run(e, mk(0))
			run(e, mk(sec(60)))
			got := r.take()
			if len(got) != 1 {
				t.Fatalf("got %d", len(got))
			}
			if !strings.Contains(got[0].Body, c.expect) || !strings.Contains(got[0].Body, "4 jobs en cola") ||
				!strings.Contains(got[0].Body, "20 min") {
				t.Fatalf("body = %q", got[0].Body)
			}
		})
	}
}

func TestQueueStuckThresholdAndResolution(t *testing.T) {
	e, r := newEngine()
	run(e, stuck(0, 15*60)) // exactly 15 min is not "more than"
	run(e, stuck(sec(120), 15*60))
	if len(r.take()) != 0 {
		t.Fatal("not above threshold")
	}
	run(e, stuck(sec(130), 15*60+1))
	run(e, stuck(sec(190), 15*60+1))
	if len(r.take()) != 1 {
		t.Fatal("should fire")
	}
	run(e, stuck(sec(200), 10*60)) // 10 min > 7.5 min: band
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("band")
	}
	run(e, stuck(sec(210), 7*60))
	if g := r.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("resolve = %+v", g)
	}
	// empty queue resolves too
	run(e, stuck(sec(300), 20*60))
	run(e, stuck(sec(360), 20*60))
	r.take()
	in := healthy(sec(362))
	run(e, in)
	if g := r.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("empty queue must resolve: %+v", g)
	}
}

func fails(now time.Time, repo string, n int, ago time.Duration) []FailedJob {
	var out []FailedJob
	for i := 0; i < n; i++ {
		out = append(out, FailedJob{Repo: repo, Job: fmt.Sprintf("job%d", i), At: now.Add(-ago - sec(i))})
	}
	return out
}

func TestJobFailuresPerRepo(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(5))
	in.FailedJobs = append(fails(in.Now, "acme/api", 3, min(1)), fails(in.Now, "acme/web", 2, min(1))...)
	run(e, in)
	got := r.take()
	if len(got) != 1 || got[0].Tag != "job_failures:acme/api" || !strings.Contains(got[0].Body, "3 jobs han fallado en acme/api") {
		t.Fatalf("got %+v", got)
	}
	// web reaches 3 -> its own alert
	in.Now = in.Now.Add(sec(10))
	in.FailedJobs = append(fails(in.Now, "acme/api", 3, min(1)), fails(in.Now, "acme/web", 3, min(2))...)
	run(e, in)
	got = r.take()
	if len(got) != 1 || got[0].Tag != "job_failures:acme/web" {
		t.Fatalf("got %+v", got)
	}
	ids := activeIDs(e)
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	// never repeats
	for i := 0; i < 20; i++ {
		in.Now = in.Now.Add(min(5))
		in.FailedJobs = fails(in.Now, "acme/api", 3, sec(1))
		run(e, in)
	}
	for _, n := range r.take() {
		if !n.Resolved {
			t.Fatalf("job_failures must not repeat: %+v", n)
		}
	}
}

func TestJobFailuresWindowAndDedupe(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(100))
	// 2 recent + 5 outside the 30 min window: below threshold
	in.FailedJobs = append(fails(in.Now, "a/b", 2, min(1)), fails(in.Now, "a/b", 5, min(40))...)
	run(e, in)
	// the same failure repeated is counted once
	f := FailedJob{Repo: "a/b", Job: "x", At: in.Now.Add(-min(2))}
	in.FailedJobs = []FailedJob{f, f, f, f}
	run(e, in)
	// future-dated failures are ignored
	in.FailedJobs = []FailedJob{{Repo: "a/b", Job: "y", At: in.Now.Add(min(5))}, {Repo: "a/b", Job: "z", At: in.Now.Add(min(6))}, {Repo: "a/b", Job: "w", At: in.Now.Add(min(7))}}
	run(e, in)
	if len(r.take()) != 0 {
		t.Fatal("must not alert")
	}
}

func TestJobFailuresResolvesAfter60QuietMinutes(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(5))
	in.FailedJobs = fails(in.Now, "a/b", 3, 0)
	last := in.FailedJobs[0].At
	run(e, in)
	r.take()
	// the collector keeps reporting the old failures
	for _, d := range []time.Duration{min(35), min(59)} {
		in.Now = last.Add(d)
		run(e, in)
		if len(e.Snapshot().Active) != 1 || len(r.take()) != 0 {
			t.Fatalf("must stay active at +%v", d)
		}
	}
	in.Now = last.Add(min(60))
	run(e, in)
	got := r.take()
	if len(got) != 1 || !got[0].Resolved || got[0].Tag != "job_failures:a/b" {
		t.Fatalf("resolve = %+v", got)
	}
	// resolved, and the old failures do not re-fire it
	in.Now = last.Add(min(61))
	run(e, in)
	if len(r.take()) != 0 || len(e.Snapshot().Active) != 0 {
		t.Fatal("old failures must not re-fire")
	}
	// repo vanished from input entirely: quiet period still counts
	e2, r2 := newEngine()
	in2 := healthy(min(5))
	in2.FailedJobs = fails(in2.Now, "x/y", 3, 0)
	l2 := in2.FailedJobs[0].At
	run(e2, in2)
	r2.take()
	in3 := healthy(0)
	in3.Now = l2.Add(min(61))
	run(e2, in3)
	if g := r2.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("vanished repo must resolve: %+v", g)
	}
}

func TestJobFailuresNewFailuresKeepAlertAlive(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(5))
	in.FailedJobs = fails(in.Now, "a/b", 3, 0)
	run(e, in)
	r.take()
	in.Now = in.Now.Add(min(50))
	in.FailedJobs = fails(in.Now, "a/b", 1, 0)
	run(e, in)
	in.Now = in.Now.Add(min(50)) // 50 min since the newest failure
	in.FailedJobs = nil
	run(e, in)
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("newest failure restarts the quiet period")
	}
}

func prodIn(d time.Duration, p float64) Inputs {
	in := healthy(d)
	in.ProdCPULimit, in.ProdCPUPressure = 40, p
	return in
}

func TestProdPressure(t *testing.T) {
	e, r := newEngine()
	for s := 0; s < 180; s += 2 {
		run(e, prodIn(sec(s), 45))
	}
	if len(r.take()) != 0 {
		t.Fatal("needs 3 min sustained")
	}
	run(e, prodIn(sec(180), 40)) // >= limit counts
	got := r.take()
	if len(got) != 1 || got[0].Tag != "prod_pressure" || !strings.Contains(got[0].Body, "40.0 %") || !strings.Contains(got[0].Body, "3 min") {
		t.Fatalf("got %+v", got)
	}
	run(e, prodIn(sec(200), 35)) // 35 > 32: band
	if len(e.Snapshot().Active) != 1 {
		t.Fatal("band")
	}
	run(e, prodIn(sec(210), 20))
	run(e, prodIn(sec(329), 20))
	if len(r.take()) != 0 {
		t.Fatal("clear needs 120 s")
	}
	run(e, prodIn(sec(330), 20))
	if g := r.take(); len(g) != 1 || !g[0].Resolved {
		t.Fatalf("resolve = %+v", g)
	}
}

func TestProdPressureThresholdMinutes(t *testing.T) {
	cfg := DefaultConfig()
	cfg[ProdPressure] = RuleConfig{Enabled: true, Threshold: 1}
	r := &rec{}
	e := New(cfg, r.sink)
	run(e, prodIn(0, 50))
	run(e, prodIn(sec(60), 50))
	if len(r.take()) != 1 {
		t.Fatal("1 min threshold")
	}
}

func TestDaemonRestart(t *testing.T) {
	e, r := newEngine()
	in := healthy(0)
	in.UnexpectedRestart = true
	run(e, in)
	got := r.take()
	if len(got) != 1 || got[0].Severity != Critical || got[0].Tag != "daemon_restart" {
		t.Fatalf("got %+v", got)
	}
	// flag repeated: still once
	run(e, in)
	in.Now = t0.Add(sec(2))
	in.UnexpectedRestart = false
	run(e, in)
	for _, d := range []time.Duration{min(10), min(59)} {
		in.Now = t0.Add(d)
		run(e, in)
	}
	if len(r.take()) != 0 || len(e.Snapshot().Active) != 1 {
		t.Fatal("stays active for 60 min, never repeats")
	}
	in.Now = t0.Add(min(60))
	run(e, in)
	if len(r.take()) != 0 {
		t.Fatal("auto-clear must be silent")
	}
	s := e.Snapshot()
	if len(s.Active) != 0 || len(s.History) != 1 || s.History[0].Rule != DaemonRestart {
		t.Fatalf("snapshot = %+v", s)
	}
	// a later restart is a new alert
	in.Now = t0.Add(min(120))
	in.UnexpectedRestart = true
	run(e, in)
	if len(r.take()) != 1 {
		t.Fatal("new restart must notify again")
	}
}

func jan(d time.Duration, runs int, freed uint64) Inputs {
	in := lowDisk(d)
	in.JanitorRuns, in.JanitorFreedMBLast = runs, freed
	return in
}

func TestJanitorStuck(t *testing.T) {
	e, r := newEngine()
	// disk_low fires at 60 s; janitor_stuck only at 600 s
	for s := 0; s < 600; s += 2 {
		run(e, jan(sec(s), 3, 0))
	}
	got := r.take()
	if len(got) != 1 || got[0].Tag != "disk_low" {
		t.Fatalf("before 600 s only disk_low: %+v", got)
	}
	run(e, jan(sec(600), 3, 0))
	got = r.take()
	if len(got) != 1 || got[0].Tag != "janitor_stuck" || !strings.Contains(got[0].Body, "3 veces") {
		t.Fatalf("got %+v", got)
	}
	// freed something: condition false, resolves after 120 s
	run(e, jan(sec(610), 4, 500))
	run(e, jan(sec(729), 4, 500))
	if len(e.Snapshot().Active) != 2 {
		t.Fatal("janitor_stuck clears only after 120 s")
	}
	run(e, jan(sec(730), 4, 500))
	var resolved []string
	for _, n := range r.take() {
		if n.Resolved {
			resolved = append(resolved, n.Tag)
		}
	}
	if len(resolved) != 1 || resolved[0] != "janitor_stuck" {
		t.Fatalf("resolved = %v", resolved)
	}
}

func TestJanitorStuckNeedsTwoRunsAndNothingFreed(t *testing.T) {
	for _, c := range []struct {
		runs  int
		freed uint64
	}{{1, 0}, {5, 1}, {0, 0}} {
		e, r := newEngine()
		for s := 0; s <= 900; s += 2 {
			run(e, jan(sec(s), c.runs, c.freed))
		}
		for _, n := range r.take() {
			if n.Tag == "janitor_stuck" {
				t.Fatalf("runs=%d freed=%d must not fire", c.runs, c.freed)
			}
		}
	}
}

// ---- noise control ----

func manyRepos(now time.Time, n int) []FailedJob {
	var out []FailedJob
	for i := 0; i < n; i++ {
		out = append(out, fails(now, fmt.Sprintf("org/repo%02d", i), 3, sec(5))...)
	}
	return out
}

func TestGlobalLimitAndSummary(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(1))
	in.FailedJobs = manyRepos(in.Now, 10)
	run(e, in)
	got := r.take()
	if len(got) != 6 {
		t.Fatalf("got %d notifications, want 6 (5 + summary)", len(got))
	}
	sum := got[5]
	if sum.Tag != "alerts-summary" || !strings.Contains(sum.Title, "5 avisos más") ||
		!strings.Contains(sum.Body, "5 avisos más: ") || sum.URL != "/#/alertas" {
		t.Fatalf("summary = %+v", sum)
	}
	for _, n := range got[:5] {
		if !strings.HasPrefix(n.Tag, "job_failures:") {
			t.Fatalf("single = %+v", n)
		}
	}
	// everyone was told (individually or via the summary); nothing more is sent
	for _, a := range e.Snapshot().Active {
		if a.Notifications != 1 {
			t.Fatalf("%s notifications = %d", a.ID, a.Notifications)
		}
	}
	for i := 1; i < 50; i++ {
		in.Now = in.Now.Add(sec(2))
		in.FailedJobs = manyRepos(in.Now, 10)
		run(e, in)
	}
	if len(r.take()) != 0 {
		t.Fatal("no further notifications")
	}
}

func TestSummaryListIsBoundedAndCriticalGoesFirst(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(1))
	in.FailedJobs = manyRepos(in.Now, 20)
	in.UnexpectedRestart = true
	run(e, in)
	got := r.take()
	if len(got) != 6 || got[0].Tag != "daemon_restart" {
		t.Fatalf("critical must lead: %+v", got[0])
	}
	if !strings.Contains(got[5].Body, "y 10 más") { // 16 summarised, list shows 6
		t.Fatalf("summary body = %q", got[5].Body)
	}
}

func TestBudgetWindowSlidesAndBlocksUntilFree(t *testing.T) {
	e, r := newEngine()
	in := healthy(0)
	in.FailedJobs = manyRepos(in.Now, 6)
	run(e, in)
	if len(r.take()) != 6 {
		t.Fatal("first burst: 6")
	}
	// a brand-new disk alert in the same window is held back
	d := lowDisk(min(2))
	run(e, d)
	run(e, lowDisk(min(2)+sec(60)))
	if len(r.take()) != 0 {
		t.Fatal("budget exhausted: nothing sent")
	}
	if a := activeAlert(e, "disk_low"); a == nil || a.Notifications != 0 {
		t.Fatalf("unannounced alert expected: %+v", a)
	}
	// after the 10 min window expires it goes out on its own
	run(e, lowDisk(min(10)))
	got := r.take()
	if len(got) != 1 || got[0].Tag != "disk_low" {
		t.Fatalf("after window: %+v", got)
	}
}

func activeAlert(e *Engine, id string) *Alert {
	for _, a := range e.Snapshot().Active {
		if a.ID == id {
			c := a
			return &c
		}
	}
	return nil
}

func TestResolvedNoticeHeldWhenBudgetExhausted(t *testing.T) {
	e, r := newEngine()
	// announce disk_low first
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	r.take()
	// burn the budget with 5 more alerts (5 singles fit: 1 sent already)
	in := healthy(sec(70))
	in.FailedJobs = manyRepos(in.Now, 5)
	run(e, in)
	if len(r.take()) != 5 {
		t.Fatal("5 more fit in the budget")
	}
	// disk recovers: resolved notice cannot go out yet
	in2 := healthy(sec(80))
	in2.FailedJobs = manyRepos(in2.Now, 5)
	run(e, in2)
	in2.Now = t0.Add(sec(140))
	run(e, in2)
	if len(r.take()) != 0 || activeAlert(e, "disk_low") != nil {
		t.Fatal("resolved notice must wait, alert is already resolved")
	}
	in2.Now = t0.Add(min(11))
	in2.FailedJobs = nil
	run(e, in2)
	var res int
	for _, n := range r.take() {
		if n.Resolved && n.Tag == "disk_low" {
			res++
		}
	}
	if res != 1 {
		t.Fatalf("held resolved notice must be delivered once, got %d", res)
	}
}

func TestResolvedNeverAnnouncedStaysSilent(t *testing.T) {
	e, r := newEngine()
	// exhaust the budget so disk_low is never announced
	in := healthy(0)
	in.FailedJobs = manyRepos(in.Now, 6)
	run(e, in)
	r.take()
	run(e, lowDisk(sec(10)))
	run(e, lowDisk(sec(70)))
	run(e, healthy(sec(80)))
	run(e, healthy(sec(140)))
	if len(r.take()) != 0 {
		t.Fatal("an alert nobody heard about must not announce its resolution")
	}
	if len(e.Snapshot().History) != 1 {
		t.Fatal("it still goes to the history")
	}
}

func TestSummaryCountsAsOneAndRepeatsAreReplacedByTag(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	got := r.take()
	rep := func() Notification {
		run(e, lowDisk(sec(60)+min(30)))
		return r.take()[0]
	}
	if got[0].Tag != rep().Tag {
		t.Fatal("repeat must reuse the tag so it replaces the previous one")
	}
}

// ---- config ----

func TestDisableLiveIsSilent(t *testing.T) {
	e, r := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	r.take()
	cfg := e.Config()
	cfg[DiskLow] = RuleConfig{Enabled: false, Threshold: 12}
	e.SetConfig(cfg)
	run(e, lowDisk(sec(62)))
	if len(r.take()) != 0 {
		t.Fatal("disabling must be silent")
	}
	s := e.Snapshot()
	if len(s.Active) != 0 || len(s.History) != 1 || s.History[0].ResolvedAt == nil {
		t.Fatalf("snapshot = %+v", s)
	}
	// stays quiet while disabled
	for i := 0; i < 100; i++ {
		run(e, lowDisk(sec(64+2*i)))
	}
	if len(r.take()) != 0 || len(e.Snapshot().Active) != 0 {
		t.Fatal("disabled rule must stay silent")
	}
	// re-enabling restarts from scratch (for applies again)
	cfg[DiskLow] = RuleConfig{Enabled: true, Threshold: 12}
	e.SetConfig(cfg)
	run(e, lowDisk(sec(400)))
	if len(r.take()) != 0 {
		t.Fatal("for must apply again")
	}
	run(e, lowDisk(sec(460)))
	if len(r.take()) != 1 {
		t.Fatal("should fire again")
	}
	if !e.Snapshot().Rules[0].Config.Enabled {
		t.Fatal("snapshot rules must reflect live config")
	}
}

func TestDisableJobFailuresRuleClearsAllRepos(t *testing.T) {
	e, r := newEngine()
	in := healthy(min(1))
	in.FailedJobs = manyRepos(in.Now, 3)
	run(e, in)
	r.take()
	cfg := e.Config()
	cfg[JobFailures] = RuleConfig{Enabled: false, Threshold: 3}
	e.SetConfig(cfg)
	run(e, in)
	if len(r.take()) != 0 || len(e.Snapshot().Active) != 0 || len(e.Snapshot().History) != 3 {
		t.Fatalf("snapshot = %+v", e.Snapshot())
	}
}

func TestThresholdChangeLive(t *testing.T) {
	e, r := newEngine()
	in := healthy(0)
	in.DiskFreeMB = 30 * 1024 // 15 % free
	run(e, in)
	in.Now = t0.Add(sec(60))
	run(e, in)
	if len(r.take()) != 0 {
		t.Fatal("15 % is fine at 12 %")
	}
	cfg := e.Config()
	cfg[DiskLow] = RuleConfig{Enabled: true, Threshold: 20}
	e.SetConfig(cfg)
	in.Now = t0.Add(sec(62))
	run(e, in)
	in.Now = t0.Add(sec(122))
	run(e, in)
	if g := r.take(); len(g) != 1 || g[0].Tag != "disk_low" {
		t.Fatalf("raised threshold must fire: %+v", g)
	}
}

func TestConfigSanitize(t *testing.T) {
	nan := func() float64 { z := 0.0; return z / z }()
	e := New(Config{
		DiskLow:      {Enabled: true, Threshold: 999},
		SwapFull:     {Enabled: false, Threshold: 1},
		MemLow:       {Enabled: true, Threshold: nan},
		QueueStuck:   {Enabled: true, Threshold: 0},
		"bogus_rule": {Enabled: true, Threshold: 5},
	}, nil)
	c := e.Config()
	if c[DiskLow].Threshold != 50 || c[SwapFull].Threshold != 50 || c[SwapFull].Enabled ||
		c[MemLow].Threshold != 1.5 || c[QueueStuck].Threshold != 15 {
		t.Fatalf("cfg = %+v", c)
	}
	if _, ok := c["bogus_rule"]; ok {
		t.Fatal("unknown rules must be dropped")
	}
	if len(c) != 9 || !c[JobFailures].Enabled || c[JobFailures].Threshold != 3 {
		t.Fatalf("missing rules must take defaults: %+v", c)
	}
	// Config returns a copy
	c[DiskLow] = RuleConfig{}
	if e.Config()[DiskLow].Threshold != 50 {
		t.Fatal("Config must return a copy")
	}
	// nil config and nil sink are fine
	e2 := New(nil, nil)
	run(e2, lowDisk(0))
	run(e2, lowDisk(sec(60)))
	if len(e2.Snapshot().Active) != 1 {
		t.Fatal("nil sink must still track alerts")
	}
}

func TestRuleCatalog(t *testing.T) {
	cat := RuleCatalog()
	want := []RuleID{DiskLow, SwapFull, TmpRAM, MemLow, QueueStuck, JobFailures, ProdPressure, DaemonRestart, JanitorStuck}
	if len(cat) != len(want) {
		t.Fatalf("len = %d", len(cat))
	}
	for i, ri := range cat {
		if ri.ID != want[i] {
			t.Fatalf("order: %s != %s", ri.ID, want[i])
		}
		if ri.Title == "" || ri.Description == "" || (ri.Severity != Warning && ri.Severity != Critical) {
			t.Fatalf("%s metadata incomplete: %+v", ri.ID, ri)
		}
		if !ri.Config.Enabled {
			t.Fatalf("%s must default enabled", ri.ID)
		}
		if ri.ID == DaemonRestart {
			if ri.Max != 0 || ri.ThresholdLabel != "" {
				t.Fatalf("daemon_restart has no threshold: %+v", ri)
			}
			continue
		}
		if ri.Unit == "" || ri.ThresholdLabel == "" || ri.Step <= 0 ||
			!(ri.Min <= ri.Config.Threshold && ri.Config.Threshold <= ri.Max) {
			t.Fatalf("%s range wrong: %+v", ri.ID, ri)
		}
	}
	byID := map[RuleID]RuleInfo{}
	for _, ri := range cat {
		byID[ri.ID] = ri
	}
	check := func(id RuleID, def, lo, hi float64, sev Severity) {
		ri := byID[id]
		if ri.Config.Threshold != def || ri.Min != lo || ri.Max != hi || ri.Severity != sev {
			t.Fatalf("%s = %+v", id, ri)
		}
	}
	check(DiskLow, 12, 3, 50, Critical)
	check(SwapFull, 95, 50, 100, Warning)
	check(TmpRAM, 4, 1, 64, Warning)
	check(MemLow, 1.5, 1, 4, Warning)
	check(QueueStuck, 15, 2, 240, Warning)
	check(JobFailures, 3, 2, 20, Warning)
	check(ProdPressure, 3, 1, 60, Warning)
	check(JanitorStuck, 12, 3, 50, Warning)
	check(DaemonRestart, 0, 0, 0, Critical)
}

func TestSnapshotJSONShape(t *testing.T) {
	e, _ := newEngine()
	run(e, lowDisk(0))
	run(e, lowDisk(sec(60)))
	b, err := json.Marshal(e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"active", "history", "rules"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in %s", k, b)
		}
	}
	a := m["active"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "rule", "severity", "title", "body", "since", "lastNotified", "notifications", "resolvedAt"} {
		if _, ok := a[k]; !ok {
			t.Fatalf("alert missing %q: %s", k, b)
		}
	}
	rule := m["rules"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "title", "description", "severity", "unit", "thresholdLabel", "min", "max", "step", "config"} {
		if _, ok := rule[k]; !ok {
			t.Fatalf("rule missing %q", k)
		}
	}
	cfg := rule["config"].(map[string]any)
	if _, ok := cfg["enabled"]; !ok {
		t.Fatal("config.enabled")
	}
	if _, ok := cfg["threshold"]; !ok {
		t.Fatal("config.threshold")
	}
	if !strings.Contains(string(b), `"active":[`) || !strings.Contains(string(b), `"history":[]`) {
		t.Fatalf("empty history must be [] not null: %s", b)
	}
	// empty engine too
	e2, _ := newEngine()
	b2, _ := json.Marshal(e2.Snapshot())
	if !strings.Contains(string(b2), `"active":[]`) {
		t.Fatalf("empty active must be []: %s", b2)
	}
}

func TestHistoryCappedAt100(t *testing.T) {
	e, r := newEngine()
	now := t0
	for i := 0; i < 130; i++ {
		// a restart alert that auto-clears after 60 min, 130 times
		in := healthy(0)
		in.Now = now
		in.UnexpectedRestart = true
		run(e, in)
		in.Now = now.Add(min(61))
		in.UnexpectedRestart = false
		run(e, in)
		now = now.Add(min(120))
	}
	r.take()
	h := e.Snapshot().History
	if len(h) != 100 {
		t.Fatalf("history len = %d", len(h))
	}
	if !h[0].ResolvedAt.After(*h[99].ResolvedAt) {
		t.Fatal("newest first")
	}
}

func TestDeterministic(t *testing.T) {
	seq := func() []Notification {
		e, r := newEngine()
		for i := 0; i < 400; i++ {
			in := healthy(sec(2 * i))
			switch {
			case i > 20 && i < 120:
				in.DiskFreeMB = 5000
			case i == 150:
				in.UnexpectedRestart = true
			}
			if i > 30 && i < 60 {
				in.FailedJobs = manyRepos(in.Now, 9)
			}
			if i > 200 && i < 300 {
				in.QueueDepth, in.QueueOldestWaitSec = 3, 2000
			}
			run(e, in)
		}
		return r.take()
	}
	a, b := seq(), seq()
	if len(a) == 0 {
		t.Fatal("scenario produced nothing")
	}
	if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", b) {
		t.Fatal("same inputs must give identical notifications")
	}
}

func TestActiveOrderingCriticalFirst(t *testing.T) {
	e, _ := newEngine()
	in := healthy(0)
	in.FailedJobs = manyRepos(in.Now, 2)
	in.UnexpectedRestart = true
	run(e, in)
	a := e.Snapshot().Active
	if len(a) != 3 || a[0].ID != "daemon_restart" {
		t.Fatalf("order = %v", activeIDs(e))
	}
}

func TestConcurrentSnapshotAndConfig(t *testing.T) {
	e, _ := newEngine()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.Snapshot()
					c := e.Config()
					e.SetConfig(c)
				}
			}
		}()
	}
	for i := 0; i < 300; i++ {
		in := lowDisk(sec(2 * i))
		in.FailedJobs = manyRepos(in.Now, 3)
		run(e, in)
	}
	close(stop)
	wg.Wait()
}

func TestFormatHelpers(t *testing.T) {
	if fmtGB(7270) != "7.1 GB" || fmtGB(197*1024) != "197 GB" || fmtGB(0) != "0.0 GB" {
		t.Fatal("fmtGB")
	}
	cases := map[int]string{0: "0 s", 45: "45 s", 120: "2 min", 1200: "20 min", 7200: "2 h", 7500: "2 h 5 min"}
	for in, want := range cases {
		if got := fmtDur(in); got != want {
			t.Fatalf("fmtDur(%d) = %q, want %q", in, got, want)
		}
	}
	if fmtPct(3.64) != "3.6 %" {
		t.Fatal("fmtPct")
	}
}
