package scaler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/dannyjp49/runlight/internal/backend"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

type fakeAPI struct {
	mu        sync.Mutex
	removed   []int64
	next      int
	removeErr error           // returned by RemoveRunner when set
	exists    map[string]bool // names GitHub says are already registered
}

func (f *fakeAPI) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists[s.Name] {
		return nil, scaleset.RunnerExistsError
	}
	f.next++
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: f.next, Name: s.Name},
		EncodedJITConfig: "jit",
	}, nil
}
func (f *fakeAPI) GetRunnerByName(_ context.Context, n string) (*scaleset.RunnerReference, error) {
	return &scaleset.RunnerReference{ID: 99, Name: n}, nil
}
func (f *fakeAPI) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	return nil
}

type fakeSession struct{ acquired []int64 }

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.acquired = append(f.acquired, ids...)
	return ids, nil
}

type fakeBackend struct {
	mu      sync.Mutex
	started []string
	stopped []string
	failN   int
	exited  chan string
}

func newFakeBackend() *fakeBackend { return &fakeBackend{exited: make(chan string, 16)} }

func (f *fakeBackend) Start(_ context.Context, s backend.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		return errors.New("boom")
	}
	f.started = append(f.started, s.Name)
	return nil
}
func (f *fakeBackend) Stop(_ context.Context, n string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, n)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Exited() <-chan string { return f.exited }

type fixedLimit int

func (l fixedLimit) Limit() int { return int(l) }

func stats(assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}}
}

func setup(limit, min int) (*Scaler, *fakeAPI, *fakeBackend, *fakeSession) {
	api, be, ses := &fakeAPI{}, newFakeBackend(), &fakeSession{}
	s := New(api, ses, be, fixedLimit(limit), Options{MinRunners: min, IdleTTL: time.Second})
	return s, api, be, ses
}

func TestScalesToDemand(t *testing.T) {
	s, _, be, _ := setup(10, 0)
	if err := s.Scale(context.Background(), stats(3)); err != nil {
		t.Fatal(err)
	}
	if len(be.started) != 3 {
		t.Fatalf("started %d, want 3", len(be.started))
	}
}

func TestGovernorLimitCapsRunners(t *testing.T) {
	s, _, be, _ := setup(2, 0)
	_ = s.Scale(context.Background(), stats(8))
	if len(be.started) != 2 {
		t.Fatalf("started %d, want capped at 2", len(be.started))
	}
	// Repeated polls must not exceed the cap.
	_ = s.Scale(context.Background(), stats(8))
	if len(be.started) != 2 {
		t.Fatalf("started %d after repoll", len(be.started))
	}
}

func TestMinRunnersKeptWarm(t *testing.T) {
	s, _, be, _ := setup(5, 1)
	_ = s.Scale(context.Background(), stats(0))
	if len(be.started) != 1 {
		t.Fatalf("started %d, want 1 warm", len(be.started))
	}
}

func TestNilMessageStillReconciles(t *testing.T) {
	s, _, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(2))
	s.limit = fixedLimit(5)
	// Pretend 2 more jobs arrived earlier but were capped; nil poll must not panic.
	if err := s.Scale(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(be.started) != 2 {
		t.Fatalf("started %d", len(be.started))
	}
}

func TestAcquiresJobs(t *testing.T) {
	s, _, _, ses := setup(5, 0)
	msg := &scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}},
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 8}},
		},
	}
	if err := s.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(ses.acquired) != 2 {
		t.Fatalf("acquired %v", ses.acquired)
	}
}

func TestJobLifecycleRetiresRunner(t *testing.T) {
	s, api, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(1))
	name := be.started[0]

	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name}},
	})
	if _, busy := s.Count(); busy != 1 {
		t.Fatalf("busy = %d", busy)
	}
	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: name, Result: "succeeded"}},
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
	})
	if total, _ := s.Count(); total != 0 {
		t.Fatalf("total = %d, want 0", total)
	}
	if len(be.stopped) == 0 || len(api.removed) == 0 {
		t.Fatalf("not cleaned: stopped=%v removed=%v", be.stopped, api.removed)
	}
}

func TestUnknownRunnerDoesNotPanic(t *testing.T) {
	s, _, _, _ := setup(5, 0)
	err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "ghost"}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "ghost"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Regression: a JobCompleted with no runner (job cancelled while queued) must
// be a no-op, not a Stop("") that wipes the host's job directories.
func TestJobCompletedWithoutRunnerIsIgnored(t *testing.T) {
	s, _, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(1))
	err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "", Result: "canceled"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(be.stopped) != 0 {
		t.Fatalf("acted on a nameless completion: %v", be.stopped)
	}
	if total, _ := s.Count(); total != 1 {
		t.Fatalf("total = %d, want the live runner untouched", total)
	}
}

func TestStartFailureCleansUpAndRetries(t *testing.T) {
	s, api, be, _ := setup(5, 0)
	be.failN = 1
	if err := s.Scale(context.Background(), stats(1)); err != nil {
		t.Fatalf("start failure must not stop the listener: %v", err)
	}
	if total, _ := s.Count(); total != 0 {
		t.Fatalf("leaked runner: %d", total)
	}
	if len(api.removed) != 1 {
		t.Fatalf("orphan JIT runner not removed: %v", api.removed)
	}
	// Next poll succeeds.
	_ = s.Scale(context.Background(), stats(1))
	if len(be.started) != 1 {
		t.Fatalf("did not retry: %d", len(be.started))
	}
}

func TestReapOnlyIdleAboveMin(t *testing.T) {
	s, _, be, _ := setup(5, 1)
	_ = s.Scale(context.Background(), stats(2)) // 3 runners: 1 min + 2
	if len(be.started) != 3 {
		t.Fatalf("started %d", len(be.started))
	}
	s.markBusy(be.started[0])

	s.Reap(context.Background(), time.Now().Add(time.Hour))
	total, busy := s.Count()
	if total != 1 || busy != 1 {
		t.Fatalf("total=%d busy=%d, want 1 busy survivor", total, busy)
	}
}

// Regression: the reaper once killed a runner that was mid-job because its
// local state still said "idle". GitHub is the source of truth.
func TestReapNeverKillsRunnerThatHasAJob(t *testing.T) {
	s, api, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(1))
	api.removeErr = scaleset.JobStillRunningError

	s.Reap(context.Background(), time.Now().Add(time.Hour))

	total, busy := s.Count()
	if total != 1 || busy != 1 {
		t.Fatalf("total=%d busy=%d, want the runner kept and marked busy", total, busy)
	}
	if len(be.stopped) != 0 {
		t.Fatalf("stopped a runner that has a job: %v", be.stopped)
	}
}

func TestReapKeepsRunnerWhenGitHubIsUnreachable(t *testing.T) {
	s, api, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(1))
	api.removeErr = errors.New("api down")

	s.Reap(context.Background(), time.Now().Add(time.Hour))

	if total, _ := s.Count(); total != 1 || len(be.stopped) != 0 {
		t.Fatalf("must be conservative when idleness cannot be confirmed: total=%d stopped=%v", total, be.stopped)
	}
}

func TestWatchExitsRemovesRunner(t *testing.T) {
	s, _, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(1))
	name := be.started[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.WatchExits(ctx)
	be.exited <- name

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if total, _ := s.Count(); total == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("exited runner not removed")
}

func TestShutdownLeavesRunningJobsAlone(t *testing.T) {
	s, api, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(2))
	api.removeErr = scaleset.JobStillRunningError

	s.Shutdown(context.Background())

	if len(be.stopped) != 0 {
		t.Fatalf("shutdown killed runners that have jobs: %v", be.stopped)
	}
}

func TestShutdownDestroysAll(t *testing.T) {
	s, _, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(3))
	s.Shutdown(context.Background())
	if total, _ := s.Count(); total != 0 {
		t.Fatalf("total = %d", total)
	}
	if len(be.stopped) < 3 {
		t.Fatalf("stopped %d", len(be.stopped))
	}
}

func TestTelemetryRecordsTheWholeLifecycle(t *testing.T) {
	hub := telemetry.New(telemetry.Config{})
	api, be, ses := &fakeAPI{}, newFakeBackend(), &fakeSession{}
	s := New(api, ses, be, fixedLimit(5), Options{
		IdleTTL: time.Second, Tel: hub,
		UnitStats: func(string) (uint64, uint64) { return 0, 1500 << 20 },
	})
	now := time.Now()
	base := scaleset.JobMessageBase{
		RunnerRequestID: 9, OwnerName: "org", RepositoryName: "repo", JobDisplayName: "build",
		QueueTime: now.Add(-50 * time.Second), RunnerAssignTime: now.Add(-40 * time.Second), FinishTime: now,
	}

	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: base}},
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
	})
	if hub.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d, want 1", hub.QueueDepth())
	}
	name := be.started[0]

	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name, JobMessageBase: base}},
	})
	if st := hub.Snapshot(); len(st.Queue) != 0 || st.Runners[0].State != "busy" {
		t.Fatalf("after start: %+v", st)
	}

	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: name, JobMessageBase: base}},
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
	})
	st := hub.Snapshot()
	if len(st.Jobs) != 1 || st.Jobs[0].RunSec != 40 || st.Jobs[0].QueueSec != 10 || st.Jobs[0].PeakMemMB != 1500 {
		t.Fatalf("job record = %+v", st.Jobs)
	}
	if len(st.Profiles) != 1 || st.Profiles[0].PeakMemMB != 1500 {
		t.Fatalf("profile not learned: %+v", st.Profiles)
	}
	if len(st.Runners) != 0 {
		t.Fatalf("runner should be gone: %+v", st.Runners)
	}
	if len(st.Events) < 2 {
		t.Fatalf("decision log should explain what happened: %+v", st.Events)
	}
}

func TestQueuedCancellationOnlyLeavesTheQueue(t *testing.T) {
	hub := telemetry.New(telemetry.Config{})
	s, _, be, _ := setup(5, 0)
	s.o.Tel = hub
	hub.JobQueued(5, "o/r", "j", nil, time.Time{})

	_ = s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "canceled", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 5}}},
	})
	st := hub.Snapshot()
	if hub.QueueDepth() != 0 || len(st.Jobs) != 0 || st.Totals.Canceled != 0 {
		t.Fatalf("a job cancelled in the queue must only leave it: %+v", st)
	}
	if len(be.stopped) != 0 {
		t.Fatalf("must not stop anything: %v", be.stopped)
	}
}

func slotScaler(slots int, existing ...string) (*Scaler, *fakeAPI, *fakeBackend) {
	api, be, ses := &fakeAPI{exists: map[string]bool{}}, newFakeBackend(), &fakeSession{}
	for _, n := range existing {
		api.exists[n] = true
	}
	return New(api, ses, be, fixedLimit(slots), Options{Slots: slots, SlotCooldown: -1, IdleTTL: time.Second}), api, be
}

func TestRunnersTakeStableSlotNames(t *testing.T) {
	s, _, be := slotScaler(3)
	_ = s.Scale(context.Background(), stats(3))
	got := map[string]bool{}
	for _, n := range be.started {
		got[n] = true
	}
	for _, want := range []string{"runlight-1", "runlight-2", "runlight-3"} {
		if !got[want] {
			t.Fatalf("started %v, want the three stable slots", be.started)
		}
	}
}

func TestAFreedSlotIsReusedSoCachesStayWarm(t *testing.T) {
	s, _, be := slotScaler(3)
	_ = s.Scale(context.Background(), stats(2)) // slots 1 and 2
	s.retire(context.Background(), "runlight-1")
	_ = s.Scale(context.Background(), stats(2)) // one new runner: must be slot 1 again, not slot 3
	if last := be.started[len(be.started)-1]; last != "runlight-1" {
		t.Fatalf("new runner took %q instead of reusing runlight-1: %v", last, be.started)
	}
}

func TestSlotThatStillExistsOnGitHubIsSkipped(t *testing.T) {
	s, api, be := slotScaler(3, "runlight-1") // runlight-1 is still registered (busy elsewhere)
	_ = s.Scale(context.Background(), stats(1))
	if len(be.started) != 1 || be.started[0] != "runlight-2" {
		t.Fatalf("started %v, want runlight-2 after skipping the existing runlight-1", be.started)
	}
	if len(api.removed) != 0 {
		t.Fatal("nothing should be removed from GitHub for a name that was never created")
	}
}

func TestRestingSlotIsNotReusedImmediately(t *testing.T) {
	api, be, ses := &fakeAPI{exists: map[string]bool{}}, newFakeBackend(), &fakeSession{}
	s := New(api, ses, be, fixedLimit(3), Options{Slots: 3, SlotCooldown: time.Hour, IdleTTL: time.Second})
	_ = s.Scale(context.Background(), stats(1))
	s.retire(context.Background(), "runlight-1")
	_ = s.Scale(context.Background(), stats(1))
	if last := be.started[len(be.started)-1]; last == "runlight-1" {
		t.Fatalf("slot 1 was reused while resting: %v", be.started)
	}
}

func TestOverflowFallsBackToUniqueNames(t *testing.T) {
	s, _, be := slotScaler(1)
	s.o.Slots = 1
	_ = s.Scale(context.Background(), stats(1))
	s.limit = fixedLimit(3)
	_ = s.Scale(context.Background(), stats(3))
	seen := map[string]bool{}
	for _, n := range be.started {
		if seen[n] {
			t.Fatalf("duplicate runner name %q: %v", n, be.started)
		}
		seen[n] = true
	}
	if len(be.started) != 3 {
		t.Fatalf("started %v", be.started)
	}
}

func TestIsSlotName(t *testing.T) {
	for n, want := range map[string]bool{"runlight-1": true, "runlight-12": true, "runlight-ab12cd34": false, "runlight-": false, "vps-1": false, "runlight-1x": false} {
		if IsSlotName(n) != want {
			t.Errorf("IsSlotName(%q) != %v", n, want)
		}
	}
}

func TestPausedStartsNothingButResumeCatchesUp(t *testing.T) {
	s, _, be, _ := setup(5, 1)
	s.SetPaused(true)
	if !s.Paused() {
		t.Fatal("Paused() should report true")
	}
	_ = s.Scale(context.Background(), stats(3))
	if len(be.started) != 0 {
		t.Fatalf("started %d while paused", len(be.started))
	}
	s.SetPaused(false)
	_ = s.Scale(context.Background(), nil)
	if len(be.started) != 4 {
		t.Fatalf("started %d after resume, want 4", len(be.started))
	}
}

func TestPauseKeepsLiveRunnersAndReapStillWorks(t *testing.T) {
	s, _, be, _ := setup(5, 0)
	_ = s.Scale(context.Background(), stats(2))
	s.SetPaused(true)
	if total, _ := s.Count(); total != 2 {
		t.Fatalf("pausing must not remove runners, have %d", total)
	}
	s.Reap(context.Background(), time.Now().Add(time.Hour))
	if total, _ := s.Count(); total != 0 || len(be.stopped) != 2 {
		t.Fatalf("idle runners should still be reaped while paused: total=%d stopped=%d", total, len(be.stopped))
	}
}

func TestLiveSettersChangeBehaviour(t *testing.T) {
	s, _, be, _ := setup(5, 0)
	s.SetMinRunners(2)
	_ = s.Scale(context.Background(), stats(0))
	if len(be.started) != 2 {
		t.Fatalf("started %d, want 2 warm", len(be.started))
	}
	s.SetMinRunners(-3) // clamped to 0
	s.SetIdleTTL(0)     // ignored
	s.SetIdleTTL(time.Hour)
	s.Reap(context.Background(), time.Now().Add(time.Minute))
	if total, _ := s.Count(); total != 2 {
		t.Fatalf("runners idle for less than the new TTL must stay, have %d", total)
	}
	s.SetIdleTTL(time.Second)
	s.Reap(context.Background(), time.Now().Add(time.Minute))
	if total, _ := s.Count(); total != 0 {
		t.Fatalf("expected reaping with the short TTL, have %d", total)
	}
	s.SetSlots(2)
	_ = s.Scale(context.Background(), stats(1))
	if n := be.started[len(be.started)-1]; n != "runlight-1" {
		t.Fatalf("slot name = %s", n)
	}
}

func TestLiveSettersAreRaceFree(t *testing.T) {
	s, _, _, _ := setup(5, 0)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.SetPaused(i%2 == 0)
			s.SetMinRunners(i % 3)
			s.SetIdleTTL(time.Duration(i+1) * time.Second)
			s.SetSlots(i % 4)
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		_ = s.Scale(context.Background(), stats(2))
		s.Reap(context.Background(), time.Now())
	}
	<-done
}
