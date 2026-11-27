package janitor

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

func TestSetRulesTakesEffectOnNextPassAndCopies(t *testing.T) {
	d := cacheDir(t)
	old := put(t, d, "a/old", 4*mb, 72*time.Hour)
	j := newJ()
	rules := []Rule{{Name: "go", Dir: d, MaxBytes: 2 * mb, MinAge: time.Hour, Mode: ModeLRU}}
	j.SetRules(rules)
	rules[0].MaxBytes = 1 << 40 // caller mutating its slice must not leak in
	rep := j.Once(t.Context())
	if len(rep.Rules) != 1 || rep.Rules[0].Cap != 2*mb || exists(old) {
		t.Fatalf("report = %+v, old exists=%v", rep.Rules, exists(old))
	}
	j.SetRules(nil)
	if rep := j.Once(t.Context()); len(rep.Rules) != 0 {
		t.Fatalf("rules not replaced: %+v", rep.Rules)
	}
}

func TestSetIntervalIgnoresNonPositive(t *testing.T) {
	j := newJ()
	j.SetInterval(5 * time.Minute)
	j.SetInterval(0)
	j.SetInterval(-time.Second)
	j.mu.Lock()
	got := j.Interval
	j.mu.Unlock()
	if got != 5*time.Minute {
		t.Fatalf("interval = %v", got)
	}
}

func TestRunNowRunsInBackgroundAndIgnoresOverlap(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	j := &Janitor{now: func() time.Time { return now }, Tel: telemetry.New(telemetry.Config{})}
	j.Disk = func() (uint64, uint64) {
		calls.Add(1)
		<-release
		return 50, 100
	}
	j.RunNow()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	j.RunNow() // overlaps the first one: must be ignored
	j.RunNow()
	close(release)
	for {
		j.mu.Lock()
		r := j.running
		j.mu.Unlock()
		if !r {
			break
		}
		if time.Now().After(deadline.Add(2 * time.Second)) {
			t.Fatal("pass never finished")
		}
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("passes = %d, want 1", got)
	}
	j.RunNow() // after finishing, a new one is allowed
	deadline = time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("second RunNow did not run, calls=%d", calls.Load())
	}
}
