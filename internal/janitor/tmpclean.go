package janitor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTmpMinAge is used when a TmpRule has no (or a non-positive) MinAge.
const DefaultTmpMinAge = 180 * time.Minute

// TmpRule cleans CI leftovers from a shared temp directory (typically /tmp,
// which is tmpfs, i.e. RAM: old files there end up in swap).
//
// Safety rules, all mandatory:
//   - Root is exactly /tmp or /var/tmp, or a real directory with >= 4 segments.
//   - Only first-level entries whose name starts with one of Prefixes.
//   - Symlinks are never followed nor removed.
//   - The newest mtime of the entry (and, for directories, of its contents down
//     to depth 2) must be older than MinAge.
//   - Entries that any process has open or as its cwd are skipped.
//   - Owner (name or numeric uid), when set, must own the entry.
type TmpRule struct {
	Root     string
	Prefixes []string
	MinAge   time.Duration
	Owner    string
}

// checkTmpRootPath is the syntactic part of the root check (no filesystem).
func checkTmpRootPath(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("empty root")
	}
	slash := filepath.ToSlash(filepath.Clean(root))
	if slash == "/tmp" || slash == "/var/tmp" {
		return nil
	}
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) && !strings.HasPrefix(slash, "/") {
		return fmt.Errorf("%q is not absolute", root)
	}
	segs := 0
	for _, s := range strings.Split(filepath.ToSlash(clean), "/") {
		if s != "" && !strings.HasSuffix(s, ":") { // ignore a Windows drive letter
			segs++
		}
	}
	if segs < 4 {
		return fmt.Errorf("%q is too shallow: only /tmp, /var/tmp or a directory with at least 4 path segments are allowed", root)
	}
	return nil
}

// safeTmpPrefix is enforced for rules built in code as well as parsed ones.
func safeTmpPrefix(p string) bool {
	return len(p) >= 4 && !strings.ContainsAny(p, "/\\\x00") && !strings.Contains(p, "..") && strings.Trim(p, ".") != ""
}

// validTmpRoot adds the filesystem checks: exists, is a directory, not a symlink.
func validTmpRoot(root string) error {
	if err := checkTmpRootPath(root); err != nil {
		return err
	}
	clean := filepath.Clean(root)
	fi, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink; refusing to follow it", root)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%q is not a directory", root)
	}
	return nil
}

// ParseTmp parses "root:prefix1,prefix2,...[:minAgeMinutes[:owner]]".
func ParseTmp(s string) (TmpRule, error) {
	const usage = `want root:prefix1,prefix2,...[:minAgeMinutes[:owner]]`
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 4 || parts[0] == "" || parts[1] == "" {
		return TmpRule{}, errors.New(usage)
	}
	r := TmpRule{Root: parts[0], MinAge: DefaultTmpMinAge}
	if err := checkTmpRootPath(r.Root); err != nil {
		return TmpRule{}, err
	}
	for _, p := range strings.Split(parts[1], ",") {
		if !safeTmpPrefix(p) {
			return TmpRule{}, fmt.Errorf("prefix %q is not safe (min 4 characters, no path separators)", p)
		}
		r.Prefixes = append(r.Prefixes, p)
	}
	if len(parts) > 2 && parts[2] != "" {
		var m float64
		if _, err := fmt.Sscanf(parts[2], "%g", &m); err != nil || m < 0 {
			return TmpRule{}, fmt.Errorf("bad min age %q", parts[2])
		}
		r.MinAge = time.Duration(m * float64(time.Minute))
	}
	if len(parts) > 3 {
		r.Owner = parts[3]
	}
	return r, nil
}

// newestMtime returns the most recent mtime of path and, when it is a
// directory, of its contents down to depth 2. Entries are read in chunks so
// memory stays constant however many files a directory holds.
func newestMtime(path string, fi fs.FileInfo) time.Time {
	newest := fi.ModTime()
	if !fi.IsDir() {
		return newest
	}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		f, err := os.Open(dir)
		if err != nil {
			return
		}
		defer f.Close()
		for {
			ents, err := f.ReadDir(256)
			for _, e := range ents {
				info, e2 := e.Info() // lstat semantics: never follows symlinks
				if e2 != nil {
					continue
				}
				if info.ModTime().After(newest) {
					newest = info.ModTime()
				}
				if depth < 2 && info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
					walk(filepath.Join(dir, e.Name()), depth+1)
				}
			}
			if err != nil { // io.EOF or a read error: stop either way
				return
			}
		}
	}
	walk(path, 1)
	return newest
}

// sweepTmp applies one rule. open holds the first-level entries (as full
// paths) that some process has open; it must come from a successful scan.
func (j *Janitor) sweepTmp(r TmpRule, open map[string]bool, now time.Time, minAge time.Duration, ownerUID uint32, hasOwner bool, rescan func() (map[string]bool, error)) (removed []string, freed int64) {
	rescanned := false
	f, err := os.Open(filepath.Clean(r.Root))
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	for {
		ents, rerr := f.ReadDir(256)
		for _, c := range ents {
			name := c.Name()
			if !hasPrefix(name, r.Prefixes) {
				continue
			}
			full := filepath.Join(filepath.Clean(r.Root), name)
			fi, err := os.Lstat(full)
			if err != nil || fi.Mode()&fs.ModeSymlink != 0 || !(fi.IsDir() || fi.Mode().IsRegular()) {
				continue
			}
			if hasOwner {
				if uid, ok := fileOwner(fi); !ok || uid != ownerUID {
					continue
				}
			}
			if now.Sub(newestMtime(full, fi)) < minAge {
				continue
			}
			if open[full] {
				continue
			}
			// Re-scan once, right before the first deletion, to shrink the
			// window between the pass-start scan and the delete.
			if !j.DryRun && !rescanned && rescan != nil {
				m, err := rescan()
				if err != nil {
					return removed, freed
				}
				for k := range m {
					open[k] = true
				}
				rescanned = true
				if open[full] {
					continue
				}
			}
			var size int64
			if fi.IsDir() {
				size, _, _ = treeStats(full)
			} else {
				size = fi.Size()
			}
			if !j.DryRun {
				if fi.IsDir() {
					makeWritable(full)
				}
				if err := os.RemoveAll(full); err != nil {
					continue
				}
			}
			removed = append(removed, name)
			freed += size
		}
		if rerr != nil {
			break
		}
	}
	return removed, freed
}

// runTmp applies every TmpRule for one pass.
func (j *Janitor) runTmp(rules []TmpRule, now time.Time, low bool) (removed []string, freed int64) {
	roots := make([]string, 0, len(rules))
	valid := make([]TmpRule, 0, len(rules))
	for _, r := range rules {
		if err := validTmpRoot(r.Root); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				j.log().Warn("tmp-clean root refused", "root", r.Root, "err", err)
			}
			continue
		}
		badPrefix := false
		for _, p := range r.Prefixes {
			if !safeTmpPrefix(p) {
				badPrefix = true
			}
		}
		if badPrefix {
			j.log().Warn("tmp-clean rule refused: unsafe prefix", "root", r.Root, "prefixes", r.Prefixes)
			continue
		}
		// /proc reports real paths: resolve parent symlinks so the open-file
		// matching below sees the same names the sweep deletes.
		if real, err := filepath.EvalSymlinks(filepath.Clean(r.Root)); err == nil {
			r.Root = real
		}
		valid = append(valid, r)
		roots = append(roots, filepath.Clean(r.Root))
	}
	if len(valid) == 0 {
		return nil, 0
	}
	scan := j.openScan
	if scan == nil {
		scan = scanOpenEntries
	}
	// One /proc scan per pass. If it fails we cannot prove anything is unused,
	// so nothing is deleted.
	open, err := scan(roots)
	if err != nil {
		j.log().Warn("tmp-clean skipped: cannot tell which files are open", "err", err)
		return nil, 0
	}
	for _, r := range valid {
		minAge := r.MinAge
		if minAge <= 0 {
			minAge = DefaultTmpMinAge
		}
		if low {
			minAge = min(minAge, 30*time.Minute)
		}
		var uid uint32
		hasOwner := r.Owner != ""
		if hasOwner {
			u, err := resolveOwner(r.Owner)
			if err != nil {
				j.log().Warn("tmp-clean owner refused", "root", r.Root, "owner", r.Owner, "err", err)
				continue
			}
			uid = u
		}
		rm, fr := j.sweepTmp(r, open, now, minAge, uid, hasOwner, func() (map[string]bool, error) { return scan(roots) })
		removed = append(removed, rm...)
		freed += fr
		if len(rm) > 0 {
			verb, freedVerb := "eliminó", "liberó"
			if j.DryRun {
				verb, freedVerb = "eliminaría", "liberaría"
			}
			j.Tel.Eventf("prune", "Limpieza de temporales en %s: %s %d elemento(s) de CI y %s %s.", r.Root, verb, len(rm), freedVerb, human(fr))
		}
	}
	return removed, freed
}
