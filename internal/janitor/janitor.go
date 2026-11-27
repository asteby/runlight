// Package janitor keeps CI caches and per-runner leftovers from eating the
// disk that production shares with them.
//
// It does three jobs:
//
//   - LRU rules: a cache with a size cap; when it overflows, the files that
//     were used longest ago go first, until it is back under 85% of the cap.
//   - Idle rules: caches that cannot be trimmed file by file (Go's module
//     cache is read-only); wiped whole, but only while no runner is busy.
//   - Ephemeral roots: directories keyed by runner name. A runner that no
//     longer exists leaves dead weight behind; those directories are removed.
//
// Because it deletes files on a production host, every operation is bounded:
// roots must be deep enough not to be a home directory, symlinks are never
// followed, files younger than MinAge are never touched, nothing is removed
// outside the configured directories, and a dry-run mode reports what would go.
package janitor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dannyjp49/runlight/internal/telemetry"
)

type Mode string

const (
	// ModeLRU evicts least-recently-used files until under the cap.
	ModeLRU Mode = "lru"
	// ModeIdle removes the whole cache when over the cap and nothing is running.
	ModeIdle Mode = "idle"
)

// Rule is a capped cache.
type Rule struct {
	Name     string
	Dir      string
	MaxBytes int64
	MinAge   time.Duration // files used more recently than this are never removed
	Mode     Mode
}

// Ephemeral is a directory whose children are named after runners (or any
// other short-lived owner). Children matching a prefix, that are not alive and
// have been untouched for MinAge, are removed.
type Ephemeral struct {
	Root     string
	Prefixes []string
	MinAge   time.Duration
}

// RuleReport is what happened to one rule in one pass.
type RuleReport struct {
	Name    string `json:"name"`
	Dir     string `json:"dir"`
	Before  int64  `json:"before"`
	After   int64  `json:"after"`
	Cap     int64  `json:"cap"`
	Freed   int64  `json:"freed"`
	Files   int    `json:"files"`
	Skipped string `json:"skipped,omitempty"`
}

// Report summarises one pass.
type Report struct {
	At        time.Time    `json:"at"`
	Rules     []RuleReport `json:"rules"`
	Removed   []string     `json:"removed"` // ephemeral directories
	Freed     int64        `json:"freed"`
	LowDisk   bool         `json:"lowDisk"`
	DryRun    bool         `json:"dryRun"`
	Truncated bool         `json:"truncated"`
}

// Janitor runs the rules. Fill the exported fields, then call Run.
type Janitor struct {
	Rules      []Rule
	Ephemerals []Ephemeral
	Interval   time.Duration

	// Live returns the runner names alive right now and how many are busy.
	Live func() (names []string, busy int)
	// Keep protects names that must survive even when they match a prefix
	// (e.g. stable slot names that are reused between jobs).
	Keep func(name string) bool
	// Disk returns free and total MB of the volume holding the caches.
	Disk func() (freeMB, totalMB uint64)
	// LowDiskPct is the free-space percentage below which the janitor turns
	// emergency-aggressive. Default 12.
	LowDiskPct float64

	DryRun bool
	Tel    *telemetry.Hub
	Log    *slog.Logger

	now     func() time.Time
	mu      sync.Mutex
	last    time.Time
	running bool            // a RunNow pass is in flight
	baseCtx context.Context // context of Run, used by RunNow; nil before Run
}

// SetRules replaces the cache rules; the next pass uses them.
func (j *Janitor) SetRules(rules []Rule) {
	cp := append([]Rule(nil), rules...)
	j.mu.Lock()
	j.Rules = cp
	j.mu.Unlock()
}

// SetInterval changes how often Run does a pass. Non-positive values are ignored.
func (j *Janitor) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	j.mu.Lock()
	j.Interval = d
	j.mu.Unlock()
}

// RunNow starts a pass in the background. It does nothing when a pass started
// by RunNow is already in flight.
func (j *Janitor) RunNow() {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return
	}
	j.running = true
	ctx := j.baseCtx
	j.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer func() {
			j.mu.Lock()
			j.running = false
			j.mu.Unlock()
		}()
		j.Once(ctx)
	}()
}

// validRoot refuses roots that could be a home directory or a system path.
func validRoot(dir string) error {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("%q is not absolute", dir)
	}
	segs := 0
	for _, s := range strings.Split(clean, string(filepath.Separator)) {
		if s != "" {
			segs++
		}
	}
	if segs < 4 {
		return fmt.Errorf("%q is too shallow to be a cache directory (need at least 4 path segments)", dir)
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink; refusing to follow it", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	return nil
}

// walkFiles streams every regular file under root to fn, without following
// symlinks and without holding the listing in memory: a cache can hold a
// million files and the daemon must stay tiny.
func walkFiles(root string, fn func(path string, size int64, mtime time.Time)) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() { // unreadable, directory, symlink, socket...
			return nil
		}
		if info, e := d.Info(); e == nil {
			fn(p, info.Size(), info.ModTime())
		}
		return nil
	})
}

// treeStats returns the total size, file count and newest mtime of a tree.
func treeStats(root string) (total int64, files int, newest time.Time) {
	if fi, err := os.Lstat(root); err == nil {
		newest = fi.ModTime()
	}
	walkFiles(root, func(_ string, size int64, mt time.Time) {
		total += size
		files++
		if mt.After(newest) {
			newest = mt
		}
	})
	return total, files, newest
}

// makeWritable lets RemoveAll delete trees with read-only directories, such
// as Go's module cache.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

// removeEmptyDirs prunes directories left empty, never the root itself.
// Depth-first, so memory is bounded by the tree depth, not its size.
func removeEmptyDirs(root string) { pruneEmpty(root, true) }

func pruneEmpty(dir string, isRoot bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			pruneEmpty(filepath.Join(dir, e.Name()), false)
		}
	}
	if !isRoot {
		_ = os.Remove(dir) // fails (harmlessly) if it still has content
	}
}

// bucketSecs is the age resolution of the LRU histogram.
const bucketSecs = 600

// pruneLRU trims a cache back to 85% of its cap, least recently used first.
// factor < 1 tightens the target (emergency mode).
//
// Memory is constant: pass 1 only builds a histogram of bytes per 10-minute
// slice of mtime; from it we find the cut-off slice that frees enough; pass 2
// deletes the files older than that cut-off.
func pruneLRU(r Rule, now time.Time, factor float64, minAge time.Duration, dry bool) RuleReport {
	rep := RuleReport{Name: r.Name, Dir: r.Dir, Cap: r.MaxBytes}
	hist := map[int64]int64{}
	var total int64
	walkFiles(r.Dir, func(_ string, size int64, mt time.Time) {
		total += size
		hist[mt.Unix()/bucketSecs] += size
	})
	rep.Before, rep.After = total, total
	if total <= int64(float64(r.MaxBytes)*factor) {
		return rep
	}
	need := total - int64(float64(r.MaxBytes)*0.85*factor)

	keys := make([]int64, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	// Slices strictly older than the cut-off are deleted whole; the slice at
	// limit is deleted only until "remaining" bytes have been freed.
	cutoff := now.Add(-minAge).Unix()
	var acc, remaining int64
	limit := int64(-1 << 62)
	reached := false
	for _, k := range keys {
		if (k+1)*bucketSecs > cutoff {
			break // this slice is not entirely older than minAge: never touch it
		}
		if acc+hist[k] >= need {
			limit, remaining, reached = k, need-acc, true
			break
		}
		acc += hist[k]
		limit = k + 1 // everything so far goes; we cannot reach the target
	}
	if !reached && limit == int64(-1<<62) {
		return rep // nothing old enough to remove
	}

	walkFiles(r.Dir, func(p string, size int64, mt time.Time) {
		b := mt.Unix() / bucketSecs
		switch {
		case b < limit:
		case b == limit && remaining > 0 && reached:
			remaining -= size
		default:
			return
		}
		if !dry {
			if err := os.Remove(p); err != nil {
				return
			}
		}
		rep.After -= size
		rep.Freed += size
		rep.Files++
	})
	if !dry {
		removeEmptyDirs(r.Dir)
	}
	return rep
}

// pruneIdle wipes a cache that is over its cap, only while nothing runs.
func pruneIdle(r Rule, busy int, dry bool) RuleReport {
	rep := RuleReport{Name: r.Name, Dir: r.Dir, Cap: r.MaxBytes}
	total, files, _ := treeStats(r.Dir)
	rep.Before, rep.After = total, total
	if total <= r.MaxBytes {
		return rep
	}
	if busy > 0 {
		rep.Skipped = "hay runners ocupados"
		return rep
	}
	if !dry {
		makeWritable(r.Dir)
		children, _ := os.ReadDir(r.Dir)
		for _, c := range children {
			_ = os.RemoveAll(filepath.Join(r.Dir, c.Name()))
		}
	}
	rep.After, rep.Freed, rep.Files = 0, total, files
	return rep
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// sweepEphemeral removes dead owners' directories.
func (j *Janitor) sweepEphemeral(e Ephemeral, live map[string]bool, now time.Time, minAge time.Duration) (removed []string, freed int64) {
	if err := validRoot(e.Root); err != nil {
		j.log().Warn("ephemeral root refused", "root", e.Root, "err", err)
		return nil, 0
	}
	children, err := os.ReadDir(e.Root)
	if err != nil {
		return nil, 0
	}
	for _, c := range children {
		name := c.Name()
		if !c.IsDir() || c.Type()&fs.ModeSymlink != 0 || !hasPrefix(name, e.Prefixes) {
			continue
		}
		if live[name] || (j.Keep != nil && j.Keep(name)) {
			continue
		}
		dir := filepath.Join(e.Root, name)
		size, _, newest := treeStats(dir)
		if now.Sub(newest) < minAge {
			continue
		}
		if !j.DryRun {
			makeWritable(dir)
			if err := os.RemoveAll(dir); err != nil {
				continue
			}
		}
		removed = append(removed, name)
		freed += size
	}
	return removed, freed
}

func (j *Janitor) log() *slog.Logger {
	if j.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return j.Log
}

func (j *Janitor) clock() time.Time {
	if j.now != nil {
		return j.now()
	}
	return time.Now()
}

func human(b int64) string {
	const gb = 1 << 30
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	}
	return fmt.Sprintf("%d MB", b>>20)
}

// Once runs a single pass.
func (j *Janitor) Once(ctx context.Context) Report {
	now := j.clock()
	rep := Report{At: now, DryRun: j.DryRun}

	low := false
	if j.Disk != nil {
		free, total := j.Disk()
		pct := j.LowDiskPct
		if pct <= 0 {
			pct = 12
		}
		low = total > 0 && float64(free)/float64(total)*100 < pct
	}
	rep.LowDisk = low
	factor := 1.0
	if low {
		factor = 0.5 // emergency: aim for half of every cap
	}

	var names []string
	busy := 0
	if j.Live != nil {
		names, busy = j.Live()
	}
	live := map[string]bool{}
	for _, n := range names {
		live[n] = true
	}

	j.mu.Lock()
	rules := append([]Rule(nil), j.Rules...)
	j.mu.Unlock()
	stats := make([]telemetry.CacheStat, 0, len(rules))
	for _, r := range rules {
		if ctx.Err() != nil {
			rep.Truncated = true
			break
		}
		var rr RuleReport
		quiet := false
		if err := validRoot(r.Dir); err != nil {
			rr = RuleReport{Name: r.Name, Dir: r.Dir, Cap: r.MaxBytes, Skipped: err.Error()}
			if errors.Is(err, fs.ErrNotExist) {
				// Not created yet (e.g. a slot that has not run a job): not an error.
				rr.Skipped, quiet = "todavía no existe", true
			} else {
				j.log().Warn("cache rule refused", "name", r.Name, "err", err)
			}
		} else if r.Mode == ModeIdle {
			rr = pruneIdle(r, busy, j.DryRun)
		} else {
			minAge := r.MinAge
			if low && minAge > time.Hour {
				minAge = time.Hour
			}
			rr = pruneLRU(r, now, factor, minAge, j.DryRun)
		}
		rep.Rules = append(rep.Rules, rr)
		rep.Freed += rr.Freed
		stats = append(stats, telemetry.CacheStat{
			Name: r.Name, Dir: r.Dir, Mode: string(r.Mode),
			SizeMB: uint64(rr.After >> 20), CapMB: uint64(r.MaxBytes >> 20), FreedMB: uint64(rr.Freed >> 20),
			LastPruneMs: now.UnixMilli(),
		})
		if rr.Freed > 0 {
			verb := "liberó"
			if j.DryRun {
				verb = "liberaría"
			}
			j.Tel.Eventf("prune", "Poda %s: %s %s (de %s a %s, tope %s).", r.Name, verb, human(rr.Freed), human(rr.Before), human(rr.After), human(r.MaxBytes))
		} else if rr.Skipped != "" && !quiet && !strings.Contains(rr.Skipped, "ocupados") {
			j.Tel.Eventf("warn", "Caché %s omitida: %s.", r.Name, rr.Skipped)
		}
	}

	minEph := func(e Ephemeral) time.Duration {
		if low {
			return min(e.MinAge, 10*time.Minute)
		}
		return e.MinAge
	}
	for _, e := range j.Ephemerals {
		if ctx.Err() != nil {
			rep.Truncated = true
			break
		}
		removed, freed := j.sweepEphemeral(e, live, now, minEph(e))
		rep.Freed += freed
		rep.Removed = append(rep.Removed, removed...)
		if len(removed) > 0 {
			verb := "eliminó"
			if j.DryRun {
				verb = "eliminaría"
			}
			j.Tel.Eventf("prune", "Limpieza de %s: %s %d carpeta(s) de runners que ya no existen y liberó %s.", e.Root, verb, len(removed), human(freed))
		}
	}

	if low && rep.Freed == 0 {
		j.Tel.Eventf("warn", "Disco casi lleno y no hay nada más que podar en las cachés configuradas.")
	}
	j.Tel.SetCaches(stats)
	j.Tel.JanitorRan(uint64(rep.Freed>>20), j.DryRun)
	j.mu.Lock()
	j.last = now
	j.mu.Unlock()
	return rep
}

// Run executes a pass shortly after start and then every Interval. When the
// disk is low it does not wait the full interval.
func (j *Janitor) Run(ctx context.Context) {
	j.mu.Lock()
	if j.Interval <= 0 {
		j.Interval = 10 * time.Minute
	}
	j.baseCtx = ctx
	j.mu.Unlock()
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	j.Once(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.mu.Lock()
			since := j.clock().Sub(j.last)
			interval := j.Interval
			j.mu.Unlock()
			lowNow := false
			if j.Disk != nil {
				free, total := j.Disk()
				pct := j.LowDiskPct
				if pct <= 0 {
					pct = 12
				}
				lowNow = total > 0 && float64(free)/float64(total)*100 < pct
			}
			if since >= interval || (lowNow && since >= 2*time.Minute) {
				j.Once(ctx)
			}
		}
	}
}

// ParseRule parses "name=dir:capGB[:mode[:minAgeHours]]", e.g.
// "go-build=/home/ubuntu/.cache/ci-go/shared/build:8:lru:24".
func ParseRule(s string) (Rule, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return Rule{}, errors.New(`want name=dir:capGB[:mode[:minAgeHours]]`)
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 {
		return Rule{}, errors.New(`want name=dir:capGB[:mode[:minAgeHours]]`)
	}
	var capGB float64
	if _, err := fmt.Sscanf(parts[1], "%g", &capGB); err != nil || capGB <= 0 {
		return Rule{}, fmt.Errorf("bad cap %q", parts[1])
	}
	r := Rule{Name: name, Dir: parts[0], MaxBytes: int64(capGB * (1 << 30)), Mode: ModeLRU, MinAge: 24 * time.Hour}
	if len(parts) > 2 && parts[2] != "" {
		switch Mode(parts[2]) {
		case ModeLRU, ModeIdle:
			r.Mode = Mode(parts[2])
		default:
			return Rule{}, fmt.Errorf("unknown mode %q (lru|idle)", parts[2])
		}
	}
	if len(parts) > 3 && parts[3] != "" {
		var h float64
		if _, err := fmt.Sscanf(parts[3], "%g", &h); err != nil || h < 0 {
			return Rule{}, fmt.Errorf("bad min age %q", parts[3])
		}
		r.MinAge = time.Duration(h * float64(time.Hour))
	}
	return r, nil
}

// ParseEphemeral parses "root:prefix1,prefix2:minAgeMinutes".
func ParseEphemeral(s string) (Ephemeral, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Ephemeral{}, errors.New(`want root:prefix1,prefix2[:minAgeMinutes]`)
	}
	e := Ephemeral{Root: parts[0], MinAge: 30 * time.Minute}
	for _, p := range strings.Split(parts[1], ",") {
		if len(p) < 4 {
			return Ephemeral{}, fmt.Errorf("prefix %q is too short to be safe (min 4 characters)", p)
		}
		e.Prefixes = append(e.Prefixes, p)
	}
	if len(parts) > 2 && parts[2] != "" {
		var m float64
		if _, err := fmt.Sscanf(parts[2], "%g", &m); err != nil || m < 0 {
			return Ephemeral{}, fmt.Errorf("bad min age %q", parts[2])
		}
		e.MinAge = time.Duration(m * float64(time.Minute))
	}
	return e, nil
}
