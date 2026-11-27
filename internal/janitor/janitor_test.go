package janitor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

var now = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

const mb = 1 << 20

// put creates a file of size bytes whose mtime is `age` before now.
func put(t *testing.T, root, rel string, size int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := now.Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func cacheDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func newJ(rules ...Rule) *Janitor {
	return &Janitor{Rules: rules, now: func() time.Time { return now }, Tel: telemetry.New(telemetry.Config{})}
}

func TestLRUEvictsOldestFirstDownToEightyFivePercent(t *testing.T) {
	d := cacheDir(t)
	old := put(t, d, "a/old", 4*mb, 72*time.Hour)
	mid := put(t, d, "b/mid", 4*mb, 48*time.Hour)
	new1 := put(t, d, "c/new1", 4*mb, 30*time.Hour)
	new2 := put(t, d, "d/new2", 4*mb, 26*time.Hour)

	// 16 MB cached, cap 12 MB: target is 85% = 10.2 MB, so two files must go.
	rep := newJ(Rule{Name: "go", Dir: d, MaxBytes: 12 * mb, MinAge: 24 * time.Hour, Mode: ModeLRU}).Once(context.Background())

	if exists(old) || exists(mid) {
		t.Fatal("the two least recently used files should have been evicted")
	}
	if !exists(new1) || !exists(new2) {
		t.Fatal("recently used files must survive")
	}
	r := rep.Rules[0]
	if r.Freed != 8*mb || r.After != 8*mb || r.Files != 2 {
		t.Fatalf("report = %+v", r)
	}
	if exists(filepath.Join(d, "a")) {
		t.Fatal("directories left empty should be removed")
	}
}

func TestUnderTheCapNothingIsTouched(t *testing.T) {
	d := cacheDir(t)
	f := put(t, d, "x", 4*mb, 500*time.Hour)
	rep := newJ(Rule{Name: "go", Dir: d, MaxBytes: 100 * mb, Mode: ModeLRU}).Once(context.Background())
	if !exists(f) || rep.Freed != 0 {
		t.Fatalf("freed %d from a cache under its cap", rep.Freed)
	}
}

func TestFilesYoungerThanMinAgeAreNeverRemoved(t *testing.T) {
	d := cacheDir(t)
	a := put(t, d, "a", 8*mb, 2*time.Hour)
	b := put(t, d, "b", 8*mb, 1*time.Hour)
	rep := newJ(Rule{Name: "go", Dir: d, MaxBytes: 4 * mb, MinAge: 24 * time.Hour, Mode: ModeLRU}).Once(context.Background())
	if !exists(a) || !exists(b) || rep.Freed != 0 {
		t.Fatal("a cache full of recent files must be left alone, even over its cap")
	}
}

func TestIdleModeWaitsForRunnersThenWipesEvenReadOnlyTrees(t *testing.T) {
	d := cacheDir(t)
	f := put(t, d, "mod/pkg@v1/file.go", 6*mb, 100*time.Hour)
	if runtime.GOOS != "windows" {
		_ = os.Chmod(filepath.Dir(f), 0o555) // like Go's read-only module cache
		defer os.Chmod(filepath.Dir(f), 0o755)
	}
	j := newJ(Rule{Name: "mod", Dir: d, MaxBytes: 2 * mb, Mode: ModeIdle})

	j.Live = func() ([]string, int) { return []string{"runlight-1"}, 1 }
	if rep := j.Once(context.Background()); exists(f) == false || rep.Rules[0].Skipped == "" {
		t.Fatalf("must not wipe while a runner is busy: %+v", rep.Rules[0])
	}

	j.Live = func() ([]string, int) { return nil, 0 }
	rep := j.Once(context.Background())
	if exists(f) {
		t.Fatal("idle and over the cap: the cache should have been wiped")
	}
	if !exists(d) {
		t.Fatal("the cache root itself must survive")
	}
	if rep.Rules[0].Freed != 6*mb {
		t.Fatalf("freed = %d", rep.Rules[0].Freed)
	}
}

func TestEphemeralRemovesOnlyDeadStaleRunnerDirs(t *testing.T) {
	root := cacheDir(t)
	dead := put(t, root, "runlight-ab12cd34/build/x", 2*mb, 5*time.Hour)
	legacy := put(t, root, "vps-deploy/build/x", 2*mb, 5*time.Hour)
	live := put(t, root, "runlight-9999aaaa/build/x", 2*mb, 5*time.Hour)
	slot := put(t, root, "runlight-1/build/x", 2*mb, 5*time.Hour)
	young := put(t, root, "runlight-ffff0000/build/x", 2*mb, 1*time.Minute)
	other := put(t, root, "shared/build/x", 2*mb, 500*time.Hour)

	j := newJ()
	j.Ephemerals = []Ephemeral{{Root: root, Prefixes: []string{"runlight-", "vps-"}, MinAge: 30 * time.Minute}}
	j.Live = func() ([]string, int) { return []string{"runlight-9999aaaa"}, 0 }
	j.Keep = func(n string) bool { return n == "runlight-1" }
	rep := j.Once(context.Background())

	if exists(dead) || exists(legacy) {
		t.Fatal("dead runners' directories should be removed")
	}
	for name, p := range map[string]string{"live runner": live, "stable slot": slot, "recently used": young, "not matching a prefix": other} {
		if !exists(p) {
			t.Fatalf("%s must be kept", name)
		}
	}
	if len(rep.Removed) != 2 || rep.Freed != 4*mb {
		t.Fatalf("report = %+v", rep)
	}
}

func TestGuardRailsRefuseDangerousRoots(t *testing.T) {
	for _, bad := range []string{"/home/ubuntu", "/", "/home", "relative/cache", ""} {
		if err := validRoot(bad); err == nil {
			t.Errorf("validRoot(%q) should be refused", bad)
		}
	}
	j := newJ(Rule{Name: "x", Dir: "/home/ubuntu", MaxBytes: 1, Mode: ModeIdle})
	rep := j.Once(context.Background())
	if rep.Rules[0].Skipped == "" || rep.Freed != 0 {
		t.Fatalf("a shallow root must be skipped, got %+v", rep.Rules[0])
	}
}

func TestSymlinkedRootIsNeverFollowed(t *testing.T) {
	target := cacheDir(t)
	victim := put(t, target, "precious", 4*mb, 900*time.Hour)
	link := filepath.Join(filepath.Dir(target), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("cannot create symlinks here:", err)
	}
	j := newJ(Rule{Name: "x", Dir: link, MaxBytes: 1, MinAge: 0, Mode: ModeLRU})
	rep := j.Once(context.Background())
	if !exists(victim) || rep.Rules[0].Skipped == "" {
		t.Fatalf("a symlinked root must be refused: %+v", rep.Rules[0])
	}
}

func TestSymlinksInsideAreNotFollowedOrCounted(t *testing.T) {
	d := cacheDir(t)
	outside := put(t, filepath.Dir(d), "outside/data", 10*mb, 900*time.Hour)
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(d, "escape")); err != nil {
		t.Skip("cannot create symlinks here:", err)
	}
	put(t, d, "real", 4*mb, 900*time.Hour)
	newJ(Rule{Name: "x", Dir: d, MaxBytes: 1 * mb, MinAge: 0, Mode: ModeLRU}).Once(context.Background())
	if !exists(outside) {
		t.Fatal("pruning followed a symlink out of the cache and deleted foreign data")
	}
}

func TestDryRunReportsButDeletesNothing(t *testing.T) {
	d := cacheDir(t)
	f := put(t, d, "a", 8*mb, 100*time.Hour)
	j := newJ(Rule{Name: "go", Dir: d, MaxBytes: 2 * mb, MinAge: time.Hour, Mode: ModeLRU})
	j.DryRun = true
	rep := j.Once(context.Background())
	if !exists(f) {
		t.Fatal("dry run deleted a file")
	}
	if rep.Freed != 8*mb || !rep.DryRun {
		t.Fatalf("dry run should report what it would free: %+v", rep)
	}
}

func TestLowDiskTightensTheTarget(t *testing.T) {
	build := func(low bool) int64 {
		d := cacheDir(t)
		for i := 0; i < 10; i++ {
			put(t, d, "f"+string(rune('a'+i)), mb, time.Duration(100+i)*time.Hour)
		}
		j := newJ(Rule{Name: "go", Dir: d, MaxBytes: 10 * mb, MinAge: time.Hour, Mode: ModeLRU})
		j.Disk = func() (uint64, uint64) {
			if low {
				return 5, 100 // 5% free
			}
			return 60, 100
		}
		return j.Once(context.Background()).Freed
	}
	// Exactly at the cap: calm mode keeps everything, emergency mode halves it.
	if calm := build(false); calm != 0 {
		t.Fatalf("calm freed %d MB", calm/mb)
	}
	if emergency := build(true); emergency == 0 {
		t.Fatal("emergency mode should prune even a cache that is exactly at its cap")
	}
}

func TestOncePublishesToTelemetry(t *testing.T) {
	d := cacheDir(t)
	put(t, d, "a", 6*mb, 100*time.Hour)
	j := newJ(Rule{Name: "go-build", Dir: d, MaxBytes: 2 * mb, MinAge: time.Hour, Mode: ModeLRU})
	j.Once(context.Background())
	st := j.Tel.Snapshot()
	if len(st.Caches) != 1 || st.Caches[0].Name != "go-build" || st.Caches[0].CapMB != 2 {
		t.Fatalf("caches = %+v", st.Caches)
	}
	if !st.Janitor.Enabled || st.Janitor.Runs != 1 || st.Janitor.FreedMB != 6 {
		t.Fatalf("janitor = %+v", st.Janitor)
	}
	found := false
	for _, e := range st.Events {
		if e.Kind == "prune" && strings.Contains(e.Msg, "go-build") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a prune event in the decision log: %+v", st.Events)
	}
}

func TestParseRule(t *testing.T) {
	r, err := ParseRule("go-build=/home/ubuntu/.cache/ci-go/shared/build:8:lru:24")
	if err != nil || r.Name != "go-build" || r.MaxBytes != 8<<30 || r.Mode != ModeLRU || r.MinAge != 24*time.Hour {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ParseRule("mod=/a/b/c/d:6:idle")
	if err != nil || r.Mode != ModeIdle || r.MinAge != 24*time.Hour {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	for _, bad := range []string{"", "x", "x=/a/b/c/d", "x=/a/b/c/d:zero", "x=/a/b/c/d:-1", "x=/a/b/c/d:1:weird", "=/a:1"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("ParseRule(%q) should fail", bad)
		}
	}
}

func TestParseEphemeral(t *testing.T) {
	e, err := ParseEphemeral("/home/ubuntu/.cache/ci-go:runlight-,vps-:45")
	if err != nil || e.Root != "/home/ubuntu/.cache/ci-go" || len(e.Prefixes) != 2 || e.MinAge != 45*time.Minute {
		t.Fatalf("%+v %v", e, err)
	}
	for _, bad := range []string{"", "/a/b/c/d", "/a/b/c/d:", "/a/b/c/d:ab", "/a/b/c/d:runlight-:x"} {
		if _, err := ParseEphemeral(bad); err == nil {
			t.Errorf("ParseEphemeral(%q) should fail (short prefixes are too dangerous)", bad)
		}
	}
}

func TestMissingCacheDirIsSilentlySkipped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "cache", "runlight-1")
	j := newJ(Rule{Name: "slot-1", Dir: missing, MaxBytes: mb, Mode: ModeLRU})
	rep := j.Once(context.Background())
	if rep.Rules[0].Skipped == "" || rep.Freed != 0 {
		t.Fatalf("report = %+v", rep.Rules[0])
	}
	for _, e := range j.Tel.Snapshot().Events {
		if e.Kind == "warn" {
			t.Fatalf("a cache that does not exist yet must not raise warnings: %+v", e)
		}
	}
}

// The histogram-based pass must reproduce exact least-recently-used order
// over many files, not just the handful of the first test.
func TestLRUOrderIsExactOverManyFiles(t *testing.T) {
	d := cacheDir(t)
	paths := map[int]string{}
	for age := 1; age <= 300; age++ { // 1 MB each, one per hour of age
		paths[age] = put(t, d, filepath.Join("s", string(rune('a'+age%26)), "f"+string(rune('A'+age%26))+itoa(age)), mb, time.Duration(age)*time.Hour)
	}
	// 300 MB cached, cap 200: target 170 MB, so the 130 oldest files go.
	rep := newJ(Rule{Name: "go", Dir: d, MaxBytes: 200 * mb, MinAge: 0, Mode: ModeLRU}).Once(context.Background())

	removed := 0
	for age, p := range paths {
		gone := !exists(p)
		if gone {
			removed++
		}
		if age > 170 && !gone {
			t.Fatalf("file aged %dh should have been evicted", age)
		}
		if age <= 170 && gone {
			t.Fatalf("file aged %dh must have been kept", age)
		}
	}
	if removed != 130 || rep.Rules[0].Freed != 130*mb {
		t.Fatalf("removed %d, freed %d MB", removed, rep.Rules[0].Freed/mb)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// When nothing is old enough, even a cache far over its cap is left alone.
func TestLRUNeverCrossesTheAgeFloorEvenWhenFarOverCap(t *testing.T) {
	d := cacheDir(t)
	keep := put(t, d, "recent", 50*mb, 10*time.Minute)
	old := put(t, d, "old", 1*mb, 48*time.Hour)
	rep := newJ(Rule{Name: "go", Dir: d, MaxBytes: 5 * mb, MinAge: 24 * time.Hour, Mode: ModeLRU}).Once(context.Background())
	if !exists(keep) {
		t.Fatal("a file inside the age floor was removed")
	}
	if exists(old) || rep.Freed != 1*mb {
		t.Fatalf("the one eligible file should go and nothing more: freed %d MB", rep.Freed/mb)
	}
}
