//go:build linux

package janitor

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func fileOwner(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func resolveOwner(name string) (uint32, error) {
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(n), nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	return uint32(n), err
}

// scanOpenEntries scans /proc/*/fd and /proc/*/cwd once and returns the
// first-level entries under roots (as full paths) that some process holds
// open or has as its working directory. Only matches under roots are kept, so
// memory does not grow with the number of open files on the machine.
// Processes that vanish or are unreadable (other users) are skipped.
func scanOpenEntries(roots []string) (map[string]bool, error) {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	note := func(target string) {
		target = strings.TrimSuffix(target, " (deleted)")
		for _, r := range roots {
			if target == r || !strings.HasPrefix(target, r+"/") {
				continue
			}
			rest := target[len(r)+1:]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				rest = rest[:i]
			}
			if rest != "" {
				open[r+"/"+rest] = true
			}
		}
	}
	seen := 0
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		seen++
		base := filepath.Join("/proc", p.Name())
		if t, err := os.Readlink(base + "/cwd"); err == nil {
			note(t)
		}
		// A running binary (e.g. a freshly linked go test binary) holds no fd.
		if t, err := os.Readlink(base + "/exe"); err == nil {
			note(t)
		}
		fds, err := os.Open(base + "/fd")
		if err != nil {
			continue
		}
		names, _ := fds.Readdirnames(-1)
		fds.Close()
		for _, n := range names {
			if t, err := os.Readlink(base + "/fd/" + n); err == nil {
				note(t)
			}
		}
	}
	if seen == 0 {
		return nil, errors.New("/proc lists no processes")
	}
	return open, nil
}
