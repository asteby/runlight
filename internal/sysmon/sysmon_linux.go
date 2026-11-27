//go:build linux

package sysmon

import (
	"os"
	"runtime"
	"syscall"

	"github.com/dannyjp49/runlight/internal/governor"
)

// Linux reads /proc and statfs. Zero allocations worth caring about: it runs
// every couple of seconds.
type Linux struct {
	// DiskPath is the filesystem runners write to (docker root or work dir).
	DiskPath string
	// PressureCgroup, when set (e.g. /sys/fs/cgroup/system.slice), makes the
	// governor react to the stalls suffered by *that* cgroup (production)
	// instead of the whole host. Host-wide PSI also counts CI waiting for CPU,
	// which is intended for a low-priority slice and would throttle CI for
	// pain that production never feels.
	PressureCgroup string
}

// pressure returns PSI "some avg10" for cpu/memory/io, preferring the
// configured cgroup and falling back to the host-wide files.
func (l Linux) pressure() (cpu, mem, io float64, ok bool) {
	if l.PressureCgroup != "" {
		c, okC := readPSI(l.PressureCgroup + "/cpu.pressure")
		m, okM := readPSI(l.PressureCgroup + "/memory.pressure")
		i, okI := readPSI(l.PressureCgroup + "/io.pressure")
		if okC && okM && okI {
			return c, m, i, true
		}
	}
	c, okC := readPSI("/proc/pressure/cpu")
	m, okM := readPSI("/proc/pressure/memory")
	i, okI := readPSI("/proc/pressure/io")
	return c, m, i, okC && okM && okI
}

func (l Linux) Sample() (governor.Signals, error) {
	var s governor.Signals

	s.CPUSome, s.MemSome, s.IOSome, s.HasPSI = l.pressure()

	if f, err := os.Open("/proc/meminfo"); err == nil {
		m := parseMeminfoKB(f)
		f.Close()
		s.MemAvailMB = m["MemAvailable"] / 1024
		s.MemTotalMB = m["MemTotal"] / 1024
		s.SwapTotalMB = m["SwapTotal"] / 1024
		if st, sf := m["SwapTotal"], m["SwapFree"]; st >= sf {
			s.SwapUsedMB = (st - sf) / 1024
		}
	}
	if l.PressureCgroup != "" {
		s.HostCPUSome, _ = readPSI("/proc/pressure/cpu")
	}
	if f, err := os.Open("/proc/loadavg"); err == nil {
		if v, ok := parseLoad1(f); ok {
			s.LoadPerCore = v / float64(runtime.NumCPU())
		}
		f.Close()
	}

	path := l.DiskPath
	if path == "" {
		path = "/"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err == nil {
		bs := uint64(st.Bsize)
		s.DiskFreeMB = st.Bavail * bs / (1 << 20)
		s.DiskTotalMB = st.Blocks * bs / (1 << 20)
	}
	return s, nil
}

func readPSI(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return parsePSI(f)
}
