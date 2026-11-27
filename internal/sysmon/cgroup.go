package sysmon

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// CG is a cgroup v2 reading. Peak is 0 when the kernel lacks memory.peak.
type CG struct {
	MemCurrent uint64 // bytes
	MemPeak    uint64 // bytes
	CPUUsec    uint64 // cumulative CPU time in microseconds
	OK         bool
}

// parseUint reads a single unsigned integer file such as memory.current.
func parseUint(r io.Reader) (uint64, bool) {
	b, err := io.ReadAll(io.LimitReader(r, 64))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

// parseCPUUsec extracts usage_usec from a cgroup cpu.stat file.
func parseCPUUsec(r io.Reader) (uint64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "usage_usec "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}
