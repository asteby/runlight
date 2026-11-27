//go:build linux

package sysmon

import (
	"os"
	"path/filepath"
)

const cgroupRoot = "/sys/fs/cgroup"

// ReadCG reads memory and CPU accounting for a cgroup, given its path
// relative to the cgroup v2 root (e.g. "system.slice" or
// "runlight.slice/runlight-ab12cd34.service"). OK is false if it is absent.
func ReadCG(rel string) CG {
	dir := filepath.Join(cgroupRoot, rel)
	var c CG
	if f, err := os.Open(filepath.Join(dir, "memory.current")); err == nil {
		c.MemCurrent, c.OK = parseUint(f)
		f.Close()
	}
	if !c.OK {
		return CG{}
	}
	if f, err := os.Open(filepath.Join(dir, "memory.peak")); err == nil {
		c.MemPeak, _ = parseUint(f)
		f.Close()
	}
	if f, err := os.Open(filepath.Join(dir, "cpu.stat")); err == nil {
		c.CPUUsec, _ = parseCPUUsec(f)
		f.Close()
	}
	return c
}
