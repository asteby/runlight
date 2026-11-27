package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNilHubIsSafe(t *testing.T) {
	var h *Hub
	h.Event("info", "x")
	h.Eventf("info", "%d", 1)
	h.AddPoint(Point{})
	h.RunnerStarted("r", "why")
	h.JobQueued(1, "a/b", "j", nil, time.Time{})
	h.JobStarted(1, "r", "a/b", "j")
	h.JobDone(1, "r", "a/b", "j", "succeeded", 0, 0, 1)
	h.SetUsage("r", 1, 1, 1)
	h.SetDecision(1)
	if h.EstimateMemMB(7, 1, 100) != 7 || h.QueueDepth() != 0 || h.History(0) != nil {
		t.Fatal("nil hub must return neutral values")
	}
	_ = h.Snapshot()
}

func TestRingsAreBounded(t *testing.T) {
	h := New(Config{})
	for i := 0; i < maxPoints+500; i++ {
		h.AddPoint(Point{T: int64(i)})
	}
	for i := 0; i < maxEvents+50; i++ {
		h.Eventf("info", "e%d", i)
	}
	if got := len(h.History(-1)); got != maxPoints {
		t.Fatalf("points = %d, want %d", got, maxPoints)
	}
	if got := len(h.events); got != maxEvents {
		t.Fatalf("events = %d, want %d", got, maxEvents)
	}
	// Newest points survive.
	if last := h.History(-1)[maxPoints-1].T; last != int64(maxPoints+499) {
		t.Fatalf("last point T=%d", last)
	}
}

func TestHistorySince(t *testing.T) {
	h := New(Config{})
	for i := 1; i <= 10; i++ {
		h.AddPoint(Point{T: int64(i * 1000)})
	}
	got := h.History(7000)
	if len(got) != 3 || got[0].T != 8000 {
		t.Fatalf("got %v", got)
	}
}

func TestRunnerAndJobLifecycle(t *testing.T) {
	h := New(Config{})
	h.RunnerStarted("r1", "demand")
	h.JobQueued(7, "org/repo", "lint", []string{"self-hosted"}, time.Now().Add(-5*time.Second))
	if h.QueueDepth() != 1 {
		t.Fatal("job should be queued")
	}
	h.JobStarted(7, "r1", "org/repo", "lint")
	st := h.Snapshot()
	if len(st.Queue) != 0 || st.Runners[0].State != "busy" || st.Runners[0].Job != "lint" {
		t.Fatalf("state = %+v", st)
	}
	h.SetUsage("r1", 800, 900, 150)
	h.JobDone(7, "r1", "org/repo", "lint", "succeeded", 5*time.Second, 40*time.Second, 950)
	h.RunnerGone("r1")
	st = h.Snapshot()
	if len(st.Runners) != 0 || len(st.Jobs) != 1 || st.Totals.Succeeded != 1 || st.Totals.Started != 1 {
		t.Fatalf("state = %+v", st)
	}
	if st.Jobs[0].RunSec != 40 || st.Jobs[0].QueueSec != 5 {
		t.Fatalf("job = %+v", st.Jobs[0])
	}
}

func TestProfilesLearnOnlyFromSuccessfulMeasuredRuns(t *testing.T) {
	h := New(Config{})
	h.JobDone(1, "r", "o/r", "build", "failed", 0, time.Second, 5000) // ignored
	h.JobDone(2, "r", "o/r", "build", "succeeded", 0, time.Second, 0) // unmeasured: ignored
	h.JobDone(3, "r", "o/r", "build", "succeeded", 0, 10*time.Second, 1000)
	h.JobDone(4, "r", "o/r", "build", "succeeded", 0, 20*time.Second, 2000)

	st := h.Snapshot()
	if len(st.Profiles) != 1 {
		t.Fatalf("profiles = %+v", st.Profiles)
	}
	p := st.Profiles[0]
	if p.Runs != 2 || p.MaxMemMB != 2000 {
		t.Fatalf("profile = %+v", p)
	}
	// EWMA: 0.3*2000 + 0.7*1000 = 1300
	if p.PeakMemMB < 1299 || p.PeakMemMB > 1301 {
		t.Fatalf("ewma = %v, want ~1300", p.PeakMemMB)
	}
	if st.Totals.Failed != 1 || st.Totals.Succeeded != 3 {
		t.Fatalf("totals = %+v", st.Totals)
	}
}

func TestEstimateMemUsesLargestQueuedProfile(t *testing.T) {
	h := New(Config{})
	h.JobDone(1, "r", "o/r", "light", "succeeded", 0, time.Second, 200)
	h.JobDone(2, "r", "o/r", "heavy", "succeeded", 0, time.Second, 3000)

	if got := h.EstimateMemMB(1024, 256, 8000); got != 1024 {
		t.Fatalf("nothing queued: got %d, want the default", got)
	}
	h.JobQueued(10, "o/r", "light", nil, time.Time{})
	if got := h.EstimateMemMB(1024, 256, 8000); got != 256 { // 200*1.2=240 -> clamped to lo
		t.Fatalf("light queue: got %d", got)
	}
	h.JobQueued(11, "o/r", "heavy", nil, time.Time{})
	if got := h.EstimateMemMB(1024, 256, 8000); got != 3600 { // 3000*1.2
		t.Fatalf("heavy queue: got %d", got)
	}
	h.JobQueued(12, "o/r", "unknown", nil, time.Time{}) // unknown jobs don't lower the estimate
	if got := h.EstimateMemMB(1024, 256, 3000); got != 3000 {
		t.Fatalf("clamp to hi: got %d", got)
	}
}

func TestSnapshotMarshalsAndOrders(t *testing.T) {
	h := New(Config{Version: "x"})
	h.Event("info", "first")
	h.Event("capacity", "second")
	h.SetDecision(map[string]int{"limit": 2})
	h.AddPoint(Point{T: 1, Limit: 2})

	b, err := json.Marshal(h.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Events  []Event `json:"events"`
		Point   *Point  `json:"point"`
		Runners []any   `json:"runners"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Events[0].Msg != "second" {
		t.Fatalf("events must be newest first: %+v", out.Events)
	}
	if out.Point == nil || out.Point.Limit != 2 || out.Runners == nil {
		t.Fatalf("snapshot = %s", b)
	}
}

func TestFailuresPausedAndQueueWait(t *testing.T) {
	h := New(Config{})
	now := time.Now()
	h.JobDone(1, "r", "a/b", "j", "failed", 0, 0, 0)
	h.JobDone(2, "r", "a/b", "j", "succeeded", 0, 0, 0)
	h.JobDone(3, "r", "a/c", "j", "canceled", 0, 0, 0)
	if got := h.RecentFailures(now.Add(-time.Minute)); len(got) != 1 || got[0].Repo != "a/b" {
		t.Fatalf("failures = %+v", got)
	}
	if got := h.RecentFailures(now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("future window = %+v", got)
	}
	for i := 0; i < maxFails+20; i++ {
		h.JobDone(int64(i), "r", "x/y", "j", "failed", 0, 0, 0)
	}
	if got := h.RecentFailures(time.Time{}); len(got) != maxFails {
		t.Fatalf("failures not bounded: %d", len(got))
	}
	h.SetPaused(true)
	if !h.Snapshot().Paused {
		t.Fatal("paused not reported")
	}
	h.JobQueued(9, "a/b", "j", nil, now.Add(-90*time.Second))
	if w := h.QueueOldestWaitSec(now); w < 89 || w > 91 {
		t.Fatalf("wait = %d", w)
	}
	h.JanitorRan(5, false)
	if s := h.JanitorStats(); s.LastFreed != 5 || s.Runs != 1 {
		t.Fatalf("janitor = %+v", s)
	}
	var nilHub *Hub
	nilHub.SetPaused(true)
	_ = nilHub.RecentFailures(now)
	_ = nilHub.QueueOldestWaitSec(now)
	_ = nilHub.JanitorStats()
}
