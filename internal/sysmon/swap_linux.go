//go:build linux

package sysmon

import (
	"os"
	"syscall"
	"time"
)

const tmpfsMagic = 0x01021994

var (
	vmRate swapRate
	// pageKB is the kernel page size in KB (pswpin/pswpout count pages).
	pageKB = uint64(max(os.Getpagesize()/1024, 1))
)

// sampleSwapRates reads /proc/vmstat and returns KB/s since the last call.
func sampleSwapRates(now time.Time) (inKBs, outKBs float64) {
	f, err := os.Open("/proc/vmstat")
	if err != nil {
		return 0, 0
	}
	in, out, ok := parseVmstatSwap(f)
	f.Close()
	if !ok {
		return 0, 0
	}
	return vmRate.update(in, out, now, pageKB)
}

// Tmpfs reports usage of path if it lives on a tmpfs (RAM-backed); ok is
// false for any other filesystem or on error.
func Tmpfs(path string) (usedMB, totalMB uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || int64(st.Type) != tmpfsMagic {
		return 0, 0, false
	}
	u, t := tmpfsMB(uint64(st.Blocks), uint64(st.Bfree), uint64(st.Bsize))
	return u, t, true
}

// TopSwap lists the processes holding the most swap (at most n).
func TopSwap(n int) []SwapProc { return ScanSwap("/proc", n, 20000, 300*time.Millisecond) }
