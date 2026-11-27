package janitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

// tmpRoot returns a directory deep enough to be a valid root.
func tmpRoot(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "a", "b", "tmp")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func noneOpen([]string) (map[string]bool, error) { return map[string]bool{}, nil }

func newTmpJ(open func([]string) (map[string]bool, error), rules ...TmpRule) *Janitor {
	return &Janitor{Tmps: rules, now: func() time.Time { return now }, openScan: open,
		Tel: telemetry.New(telemetry.Config{})}
}

// putDir creates dir/<rel> and sets every mtime in the tree (and the dir) to age ago.
func putTree(t *testing.T, root, name string, age time.Duration, files ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		put(t, dir, f, 1*mb, age)
	}
	mt := now.Add(-age)
	if err := os.Chtimes(dir, mt, mt); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTmpCleanRemovesOldPrefixedKeepsTheRest(t *testing.T) {
	root := tmpRoot(t)
	old := putTree(t, root, "go-link-123", 5*time.Hour, "a.o", "sub/b.o")
	oldFile := put(t, root, "ops-seed-bin42", 2*mb, 5*time.Hour)
	young := putTree(t, root, "go-link-new", 10*time.Minute, "a.o")
	other := putTree(t, root, "mariadb-keep", 50*time.Hour, "data")
	// Old dir, but fresh file at depth 2: must survive.
	deep := putTree(t, root, "wasm-imports-1", 5*time.Hour, "x/y.txt")
	fresh := now.Add(-5 * time.Minute)
	if err := os.Chtimes(filepath.Join(deep, "x", "y.txt"), fresh, fresh); err != nil {
		t.Fatal(err)
	}

	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-", "ops-seed-bin", "wasm-imports-"}, MinAge: 3 * time.Hour})
	rep := j.Once(context.Background())

	if exists(old) || exists(oldFile) {
		t.Fatal("old prefixed entries must be removed")
	}
	for _, p := range []string{young, other, deep} {
		if !exists(p) {
			t.Fatalf("%s must survive", p)
		}
	}
	if rep.Freed != 4*mb {
		t.Fatalf("freed = %d, want %d", rep.Freed, 4*mb)
	}
	if len(rep.Removed) != 2 {
		t.Fatalf("removed = %v", rep.Removed)
	}
}

func TestTmpCleanOnlyFirstLevel(t *testing.T) {
	root := tmpRoot(t)
	// A prefixed name nested inside an unprefixed dir is never touched.
	nested := putTree(t, root, "keepdir", 50*time.Hour)
	inner := putTree(t, nested, "go-link-inner", 50*time.Hour, "f")
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.Once(context.Background())
	if !exists(inner) {
		t.Fatal("nested entry must not be removed")
	}
}

func TestTmpCleanDryRunDeletesNothing(t *testing.T) {
	root := tmpRoot(t)
	old := putTree(t, root, "go-link-1", 9*time.Hour, "a")
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.DryRun = true
	rep := j.Once(context.Background())
	if !exists(old) {
		t.Fatal("dry run deleted something")
	}
	if rep.Freed != mb || len(rep.Removed) != 1 || !rep.DryRun {
		t.Fatalf("dry run report wrong: %+v", rep)
	}
}

func TestTmpCleanSkipsOpenEntries(t *testing.T) {
	root := tmpRoot(t)
	busy := putTree(t, root, "go-link-busy", 9*time.Hour, "a")
	idle := putTree(t, root, "go-link-idle", 9*time.Hour, "a")
	open := func([]string) (map[string]bool, error) {
		return map[string]bool{filepath.Join(root, "go-link-busy"): true}, nil
	}
	j := newTmpJ(open, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.Once(context.Background())
	if !exists(busy) {
		t.Fatal("entry held open by a process was removed")
	}
	if exists(idle) {
		t.Fatal("idle entry should be removed")
	}
}

func TestTmpCleanScanFailureDeletesNothing(t *testing.T) {
	root := tmpRoot(t)
	old := putTree(t, root, "go-link-1", 9*time.Hour, "a")
	fail := func([]string) (map[string]bool, error) { return nil, errors.New("no /proc") }
	j := newTmpJ(fail, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.Once(context.Background())
	if !exists(old) {
		t.Fatal("must not delete when open files cannot be determined")
	}
}

func TestTmpCleanNeverFollowsSymlinks(t *testing.T) {
	root := tmpRoot(t)
	target := putTree(t, t.TempDir(), "precious", 99*time.Hour, "data.txt")
	link := filepath.Join(root, "go-link-evil")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Symlink inside an old prefixed dir pointing outside.
	real := putTree(t, root, "go-link-real", 9*time.Hour, "a")
	if err := os.Symlink(target, filepath.Join(real, "esc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.Once(context.Background())
	if !exists(filepath.Join(target, "data.txt")) {
		t.Fatal("symlink target contents were deleted")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("the symlink entry itself must be left alone")
	}
	if exists(real) {
		t.Fatal("old real dir should be removed")
	}
	if !exists(filepath.Join(target, "data.txt")) {
		t.Fatal("symlink inside removed dir was followed")
	}
}

func TestTmpCleanSymlinkRootRefused(t *testing.T) {
	base := filepath.Join(t.TempDir(), "a", "b", "c")
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	old := putTree(t, realRoot, "go-link-1", 9*time.Hour, "a")
	link := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	j := newTmpJ(noneOpen, TmpRule{Root: link, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.Once(context.Background())
	if !exists(old) {
		t.Fatal("a symlinked root must be refused")
	}
}

func TestTmpCleanRefusesDangerousRoots(t *testing.T) {
	for _, root := range []string{
		"/", "/home", "/etc", "/usr", "/var", "/root", "/home/ubuntu", "/var/lib", "/var/lib/docker", "/tmp/x", "/var/tmp/x", "/usr/local/bin/",
		"relative/dir/here/now", "", ".", "..", "/tmp/..", "/var/tmp/../..",
	} {
		if err := checkTmpRootPath(root); err == nil {
			t.Errorf("checkTmpRootPath(%q) accepted a dangerous root", root)
		}
		if _, err := ParseTmp(root + ":go-link-"); err == nil {
			t.Errorf("ParseTmp accepted root %q", root)
		}
	}
	for _, ok := range []string{"/tmp", "/var/tmp", "/tmp/", "/var/tmp/", "/home/ubuntu/ci/tmp", "/srv/ci/work/tmp"} {
		if err := checkTmpRootPath(ok); err != nil {
			t.Errorf("checkTmpRootPath(%q) = %v, want ok", ok, err)
		}
	}
}

func TestTmpCleanOnceNeverTouchesRefusedRoot(t *testing.T) {
	called := false
	scan := func([]string) (map[string]bool, error) { called = true; return map[string]bool{}, nil }
	for _, root := range []string{"/", "/home", "/etc"} {
		j := newTmpJ(scan, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
		j.Once(context.Background())
	}
	if called {
		t.Fatal("open scan must not run when every root is refused")
	}
}

func TestTmpCleanOwnerFilter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("owner filter needs Linux")
	}
	root := tmpRoot(t)
	mine := putTree(t, root, "go-link-mine", 9*time.Hour, "a")
	// An owner that cannot match: nothing may be deleted.
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour, Owner: "4000000"})
	j.Once(context.Background())
	if !exists(mine) {
		t.Fatal("entry owned by someone else was removed")
	}
	// Unknown user name: rule refused.
	j = newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour, Owner: "no-such-user-xyz"})
	j.Once(context.Background())
	if !exists(mine) {
		t.Fatal("unknown owner must refuse the rule")
	}
	// Our own uid matches.
	j = newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour, Owner: itoa(os.Getuid())})
	j.Once(context.Background())
	if exists(mine) {
		t.Fatal("entry owned by the configured owner should be removed")
	}
}

func TestTmpCleanZeroMinAgeUsesDefault(t *testing.T) {
	root := tmpRoot(t)
	young := putTree(t, root, "go-link-young", time.Hour, "a")
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}})
	j.Once(context.Background())
	if !exists(young) {
		t.Fatal("a zero MinAge must fall back to the default, not delete everything")
	}
}

func TestTmpCleanEmptyPrefixesDeleteNothing(t *testing.T) {
	root := tmpRoot(t)
	old := putTree(t, root, "anything", 99*time.Hour, "a")
	j := newTmpJ(noneOpen, TmpRule{Root: root, MinAge: time.Hour})
	j.Once(context.Background())
	j2 := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{""}, MinAge: time.Hour})
	j2.Once(context.Background())
	if !exists(old) {
		t.Fatal("no prefix must mean no deletion")
	}
}

func TestParseTmp(t *testing.T) {
	r, err := ParseTmp("/tmp:go-link-,ops-seed-bin,.pnpm-store:180:ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != "/tmp" || len(r.Prefixes) != 3 || r.MinAge != 180*time.Minute || r.Owner != "ubuntu" {
		t.Fatalf("%+v", r)
	}
	r, err = ParseTmp("/var/tmp:gt-vps-")
	if err != nil || r.MinAge != 180*time.Minute || r.Owner != "" {
		t.Fatalf("defaults wrong: %+v %v", r, err)
	}
	r, _ = ParseTmp("/tmp:go-link-::bob")
	if r.MinAge != 180*time.Minute || r.Owner != "bob" {
		t.Fatalf("empty minAge should keep default: %+v", r)
	}
	for _, bad := range []string{"", "/tmp", "/tmp:", "/tmp:abc", "/tmp:a/b/c/d", "/tmp:../etc1", "/home:go-link-", "/:go-link-",
		"/tmp:go-link-:x", "/tmp:go-link-:-5", "/tmp:go-link-:1:o:extra"} {
		if _, err := ParseTmp(bad); err == nil {
			t.Errorf("ParseTmp(%q) should fail", bad)
		}
	}
}

func TestTmpCleanLowDiskShortensAge(t *testing.T) {
	root := tmpRoot(t)
	p := putTree(t, root, "go-link-1", 2*time.Hour, "a")
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: 5 * time.Hour})
	j.Once(context.Background())
	if !exists(p) {
		t.Fatal("2h-old entry must survive a 5h MinAge")
	}
	j.Disk = func() (uint64, uint64) { return 1, 100 } // 1% free
	j.Once(context.Background())
	if exists(p) {
		t.Fatal("with a low disk the age floor drops to 30 min")
	}
}

// Real /proc scan: a file held open by this very process must be reported.
func TestScanOpenEntriesSeesOurOwnOpenFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	root := tmpRoot(t)
	dir := putTree(t, root, "go-link-open", 9*time.Hour, "a")
	f, err := os.Open(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cwdDir := putTree(t, root, "go-link-cwd", 9*time.Hour, "b")
	old, _ := os.Getwd()
	if err := os.Chdir(cwdDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	free := putTree(t, root, "go-link-free", 9*time.Hour, "c")

	open, err := scanOpenEntries([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if !open[dir] || !open[cwdDir] {
		t.Fatalf("open/cwd entries not detected: %v", open)
	}
	if open[free] {
		t.Fatal("free entry reported as open")
	}
	// End to end with the real scanner.
	j := &Janitor{Tmps: []TmpRule{{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour}},
		now: func() time.Time { return now }, Tel: telemetry.New(telemetry.Config{})}
	j.Once(context.Background())
	if !exists(dir) || !exists(cwdDir) {
		t.Fatal("entries in use were removed")
	}
	if exists(free) {
		t.Fatal("free entry should be removed")
	}
	if strings.Contains(strings.Join(j.Once(context.Background()).Removed, ","), "go-link-open") {
		t.Fatal("open entry listed as removed")
	}
}

func TestTmpCleanRefusesUnsafePrefixInCodeRule(t *testing.T) {
	root := tmpRoot(t)
	keep := putTree(t, root, ".hidden-prod", 50*time.Hour, "data")
	j := newTmpJ(noneOpen, TmpRule{Root: root, Prefixes: []string{"."}, MinAge: time.Hour})
	j.runTmp(j.Tmps, now, false)
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("entry removed through unsafe prefix: %v", err)
	}
}

func TestTmpCleanRescanProtectsEntryOpenedAfterFirstScan(t *testing.T) {
	root := tmpRoot(t)
	dir := putTree(t, root, "go-link-race", 5*time.Hour, "a.o")
	calls := 0
	scan := func(roots []string) (map[string]bool, error) {
		calls++
		if calls == 1 {
			return map[string]bool{}, nil
		}
		return map[string]bool{dir: true}, nil
	}
	j := newTmpJ(scan, TmpRule{Root: root, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.runTmp(j.Tmps, now, false)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("entry opened after the first scan was deleted: %v", err)
	}
}

func TestTmpCleanParentSymlinkRootStillHonoursOpenFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	root := tmpRoot(t)
	dir := putTree(t, root, "go-link-open", 5*time.Hour, "a.o")
	link := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(filepath.Dir(root), link); err != nil {
		t.Skip(err)
	}
	viaLink := filepath.Join(link, "tmp")
	real, _ := filepath.EvalSymlinks(dir)
	scan := func(roots []string) (map[string]bool, error) { return map[string]bool{real: true}, nil }
	j := newTmpJ(scan, TmpRule{Root: viaLink, Prefixes: []string{"go-link-"}, MinAge: time.Hour})
	j.runTmp(j.Tmps, now, false)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("open entry deleted via symlinked parent root: %v", err)
	}
}
