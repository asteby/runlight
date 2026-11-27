// Package scaler turns scale-set messages into runner lifecycle actions,
// bounded by what the governor says the host can afford.
package scaler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/dannyjp49/runlight/internal/backend"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

// API is the subset of *scaleset.Client the scaler needs.
type API interface {
	GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// Session is the subset of the message session client the scaler needs.
type Session interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

// Limiter reports the current concurrency ceiling (the governor).
type Limiter interface{ Limit() int }

type Options struct {
	ScaleSetID int
	MinRunners int
	IdleTTL    time.Duration // idle runners above MinRunners are reaped after this
	MemoryMB   int           // per-runner memory cap passed to the backend
	CPUs       float64
	Logger     *slog.Logger

	// Tel records decisions for the dashboard; nil disables it.
	Tel *telemetry.Hub
	// UnitStats returns a runner's current and peak memory in bytes, read
	// just before the runner is destroyed so its footprint can be learned.
	UnitStats func(name string) (current, peak uint64)

	// Slots is how many stable runner names exist: runlight-1 … runlight-N.
	// Reusing a slot's name between jobs lets caches that workflows key by
	// runner name (GOCACHE=…/<runner.name>/build) stay warm and bounded
	// instead of cold and multiplying with every random name. 0 = random names.
	Slots int
	// SlotCooldown is how long a freed slot rests before reuse, so GitHub and
	// systemd finish tearing the previous runner down. <0 disables, 0 = 3s.
	SlotCooldown time.Duration
}

// IsSlotName reports whether name is a stable slot name (runlight-<number>).
func IsSlotName(name string) bool {
	n, ok := strings.CutPrefix(name, "runlight-")
	if !ok || n == "" {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type runner struct {
	busy      bool
	idleSince time.Time
}

type Scaler struct {
	api     API
	session Session
	be      backend.Backend
	limit   Limiter
	o       Options

	mu        sync.Mutex
	runners   map[string]*runner
	assigned  int                  // last known TotalAssignedJobs
	throttled bool                 // demand currently exceeds the governor's limit
	cool      map[string]time.Time // slots resting until the given time
	paused    bool                 // no new runners are started while true
}

// SetPaused stops (true) or resumes (false) starting new runners. Live runners
// are never touched: they finish their jobs and are reaped as usual.
func (s *Scaler) SetPaused(p bool) {
	s.mu.Lock()
	s.paused = p
	s.mu.Unlock()
}

// Paused reports whether starting new runners is paused.
func (s *Scaler) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// SetIdleTTL changes how long an idle runner above MinRunners lives.
// Non-positive values are ignored.
func (s *Scaler) SetIdleTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.o.IdleTTL = d
	s.mu.Unlock()
}

// SetMinRunners changes how many warm runners are kept. Negative becomes 0.
func (s *Scaler) SetMinRunners(n int) {
	s.mu.Lock()
	s.o.MinRunners = max(n, 0)
	s.mu.Unlock()
}

// SetSlots changes how many stable runner names exist. Negative becomes 0
// (random names). Runners already running keep their names.
func (s *Scaler) SetSlots(n int) {
	s.mu.Lock()
	s.o.Slots = max(n, 0)
	s.mu.Unlock()
}

func (s *Scaler) idleTTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.o.IdleTTL
}

func (s *Scaler) slots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.o.Slots
}

// Names lists the live runners.
func (s *Scaler) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.runners))
	for n := range s.runners {
		out = append(out, n)
	}
	return out
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func fmtMB(mb uint64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

var _ listener.Scaler = (*Scaler)(nil)

func New(api API, session Session, be backend.Backend, limit Limiter, o Options) *Scaler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = 30 * time.Second
	}
	if o.SlotCooldown == 0 {
		o.SlotCooldown = 3 * time.Second
	}
	return &Scaler{api: api, session: session, be: be, limit: limit, o: o, runners: map[string]*runner{}, cool: map[string]time.Time{}}
}

// pickName chooses the lowest free slot, or a random name when every slot is
// taken or resting (e.g. runners adopted from a previous daemon).
func (s *Scaler) pickName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for i := 1; i <= s.o.Slots; i++ {
		name := fmt.Sprintf("runlight-%d", i)
		if _, used := s.runners[name]; used {
			continue
		}
		if until, ok := s.cool[name]; ok && now.Before(until) {
			continue
		}
		return name
	}
	return "runlight-" + uuid.NewString()[:8]
}

// restSlot keeps a name out of rotation for d.
func (s *Scaler) restSlot(name string, d time.Duration) {
	if d <= 0 || !IsSlotName(name) {
		return
	}
	s.mu.Lock()
	s.cool[name] = time.Now().Add(d)
	s.mu.Unlock()
}

// Count returns the number of live runners (idle + busy).
func (s *Scaler) Count() (total, busy int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runners {
		if r.busy {
			busy++
		}
	}
	return len(s.runners), busy
}

// Scale implements listener.Scaler. It is never called concurrently, but the
// reaper and exit watcher run alongside it, hence the mutex.
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg != nil {
		// Acquire first so jobs are assigned as early as possible. Acquiring
		// an already-acquired job is a no-op, so redelivery is safe.
		if len(msg.JobAvailableMessages) > 0 {
			ids := make([]int64, 0, len(msg.JobAvailableMessages))
			for _, j := range msg.JobAvailableMessages {
				ids = append(ids, j.RunnerRequestID)
				s.o.Tel.JobQueued(j.RunnerRequestID, j.OwnerName+"/"+j.RepositoryName, j.JobDisplayName, j.RequestLabels, j.QueueTime)
			}
			if _, err := s.session.AcquireJobs(ctx, ids); err != nil {
				return fmt.Errorf("acquire jobs: %w", err)
			}
		}
		for _, j := range msg.JobStartedMessages {
			s.markBusy(j.RunnerName)
			s.o.Tel.JobStarted(j.RunnerRequestID, j.RunnerName, j.OwnerName+"/"+j.RepositoryName, j.JobDisplayName)
		}
		for _, j := range msg.JobCompletedMessages {
			repo := j.OwnerName + "/" + j.RepositoryName
			s.o.Logger.Info("job completed", "runner", j.RunnerName, "result", j.Result, "repo", repo, "job", j.JobDisplayName)
			if j.RunnerName == "" {
				// Cancelled while queued: it never had a runner, so it only
				// leaves the queue (and must never trigger a Stop).
				s.o.Tel.JobDropped(j.RunnerRequestID)
				continue
			}
			var queue, run time.Duration
			if !j.QueueTime.IsZero() && !j.RunnerAssignTime.IsZero() {
				queue = j.RunnerAssignTime.Sub(j.QueueTime)
			}
			if !j.RunnerAssignTime.IsZero() && !j.FinishTime.IsZero() {
				run = j.FinishTime.Sub(j.RunnerAssignTime)
			}
			// Read the footprint before retire() destroys the cgroup.
			var peakMB uint64
			if s.o.UnitStats != nil && j.RunnerName != "" {
				_, peak := s.o.UnitStats(j.RunnerName)
				peakMB = peak >> 20
			}
			s.o.Tel.JobDone(j.RunnerRequestID, j.RunnerName, repo, j.JobDisplayName, j.Result, queue, run, peakMB)
			mark := "✓"
			if j.Result != "succeeded" {
				mark = "✗"
			}
			line := fmt.Sprintf("%s %s · %s terminó (%s) en %s", mark, repo, j.JobDisplayName, j.Result, fmtDur(run))
			if peakMB > 0 {
				line += ", pico de memoria " + fmtMB(peakMB)
			}
			s.o.Tel.Event("job", line)
			s.retire(ctx, j.RunnerName)
		}
		if msg.Statistics != nil {
			s.mu.Lock()
			s.assigned = msg.Statistics.TotalAssignedJobs
			s.mu.Unlock()
		}
	}
	// Reconcile even on a nil (idle long-poll) message: the governor limit may
	// have risen since the last poll and queued work could now fit.
	s.reconcile(ctx)
	return nil
}

func (s *Scaler) reconcile(ctx context.Context) {
	s.mu.Lock()
	limit := s.limit.Limit()
	want := s.o.MinRunners + s.assigned
	target := min(limit, want)
	need := target - len(s.runners)
	if s.paused {
		need = 0
	}
	why := fmt.Sprintf("%d job(s) asignado(s), %d runner(s) vivos, límite %d", s.assigned, len(s.runners), limit)
	// Say so once when demand is being held back, and once when it clears.
	if want > limit && !s.throttled {
		s.throttled = true
		s.o.Tel.Eventf("scale", "Hay %d job(s) esperando pero el límite es %d: el resto queda en cola de GitHub hasta que haya margen.", want, limit)
	} else if want <= limit {
		s.throttled = false
	}
	s.mu.Unlock()

	for range max(need, 0) {
		if err := s.start(ctx, why); err != nil {
			// Transient (network, docker hiccup): the next poll retries.
			s.o.Logger.Warn("start runner failed", "err", err)
			return
		}
	}
}

func (s *Scaler) start(ctx context.Context, why string) error {
	var (
		name string
		jit  *scaleset.RunnerScaleSetJitRunnerConfig
		err  error
	)
	// A slot's previous runner may still be registered (or its job still
	// running, if adopted after a restart): GitHub then answers "runner
	// exists". Rest that slot and try the next one.
	for range s.slots() + 1 {
		name = s.pickName()
		jit, err = s.api.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.o.ScaleSetID)
		if err == nil || !errors.Is(err, scaleset.RunnerExistsError) {
			break
		}
		s.restSlot(name, 30*time.Second)
	}
	if err != nil {
		return fmt.Errorf("jit config: %w", err)
	}
	// Register before starting so an exit event can never race the insert.
	s.mu.Lock()
	s.runners[name] = &runner{idleSince: time.Now()}
	s.mu.Unlock()

	err = s.be.Start(ctx, backend.Spec{Name: name, JITConfig: jit.EncodedJITConfig, MemoryMB: s.o.MemoryMB, CPUs: s.o.CPUs})
	if err != nil {
		s.mu.Lock()
		delete(s.runners, name)
		s.mu.Unlock()
		// The unit or directory of that name may still exist: rest the slot.
		s.restSlot(name, 30*time.Second)
		// The JIT runner is registered on GitHub's side; drop it.
		if jit.Runner != nil {
			_ = s.api.RemoveRunner(context.WithoutCancel(ctx), int64(jit.Runner.ID))
		}
		return err
	}
	s.o.Logger.Info("runner started", "runner", name)
	s.o.Tel.RunnerStarted(name, why)
	s.o.Tel.Eventf("scale", "Arranca %s: %s.", name, why)
	return nil
}

func (s *Scaler) markBusy(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Tolerant by design: an unknown name (e.g. after a restart) is ignored
	// rather than crashing the daemon.
	if r, ok := s.runners[name]; ok {
		r.busy = true
	}
}

// retire destroys a runner and unregisters it from GitHub. Idempotent.
func (s *Scaler) retire(ctx context.Context, name string) {
	// A JobCompleted for a job that never got a runner (e.g. cancelled while
	// queued) carries an empty name. Acting on it once wiped every job dir.
	if name == "" {
		return
	}
	s.mu.Lock()
	_, known := s.runners[name]
	delete(s.runners, name)
	s.mu.Unlock()
	s.restSlot(name, s.o.SlotCooldown)
	s.o.Tel.RunnerGone(name)

	ctx = context.WithoutCancel(ctx)
	_ = s.be.Stop(ctx, name)
	if !known {
		return
	}
	// Ephemeral runners normally deregister themselves after their job; this
	// cleans up the ones that never got one (idle reaping, crashes).
	if ref, err := s.api.GetRunnerByName(ctx, name); err == nil && ref != nil {
		if err := s.api.RemoveRunner(ctx, int64(ref.ID)); err != nil && !errors.Is(err, scaleset.RunnerNotFoundError) {
			s.o.Logger.Debug("remove runner", "runner", name, "err", err)
		}
	}
}

// Reap scales down: idle runners above MinRunners that have been idle for
// longer than IdleTTL are destroyed. Call it periodically.
func (s *Scaler) Reap(ctx context.Context, now time.Time) {
	s.mu.Lock()
	spare := len(s.runners) - s.o.MinRunners
	var victims []string
	for name, r := range s.runners {
		if spare <= 0 {
			break
		}
		if !r.busy && now.Sub(r.idleSince) >= s.o.IdleTTL {
			victims = append(victims, name)
			spare--
		}
	}
	s.mu.Unlock()
	for _, n := range victims {
		s.reapOne(ctx, n)
	}
}

// reapOne destroys an idle runner, but only after GitHub confirms it has no
// job: RemoveRunner fails with JobStillRunningError for a runner that is
// executing (or has just been handed) a job. This is authoritative, unlike
// local bookkeeping that can miss a JobStarted message — trusting the local
// view once killed a real job mid-run.
func (s *Scaler) reapOne(ctx context.Context, name string) {
	ctx = context.WithoutCancel(ctx)
	ref, err := s.api.GetRunnerByName(ctx, name)
	switch {
	case err == nil && ref != nil:
		if err := s.api.RemoveRunner(ctx, int64(ref.ID)); err != nil {
			if errors.Is(err, scaleset.JobStillRunningError) {
				s.o.Logger.Info("runner has a job, keeping it", "runner", name)
				s.o.Tel.Eventf("keep", "Conserva %s: GitHub confirma que tiene un job en curso.", name)
				s.markBusy(name)
				return
			}
			s.o.Logger.Warn("cannot confirm runner is idle, keeping it", "runner", name, "err", err)
			return
		}
	case err != nil && !errors.Is(err, scaleset.RunnerNotFoundError):
		s.o.Logger.Warn("cannot look up runner, keeping it", "runner", name, "err", err)
		return
	}
	// Removed from GitHub (or never registered): nothing can be running.
	s.o.Logger.Info("reaping idle runner", "runner", name)
	s.o.Tel.Eventf("reap", "Retira %s: lleva %s sin job y GitHub confirma que está libre.", name, fmtDur(s.idleTTL()))
	s.retire(ctx, name)
}

// WatchExits drops runners whose process ended by itself. Run in a goroutine.
func (s *Scaler) WatchExits(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-s.be.Exited():
			s.retire(ctx, name)
		}
	}
}

// Shutdown destroys every runner. Call with a non-cancelled context.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	names := make([]string, 0, len(s.runners))
	for n := range s.runners {
		names = append(names, n)
	}
	s.mu.Unlock()
	// Only runners GitHub confirms idle are destroyed. Runners with a job keep
	// running as independent units (and are cleaned up on completion), so
	// restarting or upgrading the daemon never kills work in flight.
	for _, n := range names {
		s.reapOne(ctx, n)
	}
}
