//go:build !linux

package sysmon

// Off Linux there is no swap accounting: everything is zero/empty.

// Tmpfs is never a RAM-backed filesystem here.
func Tmpfs(string) (usedMB, totalMB uint64, ok bool) { return 0, 0, false }

// TopSwap has nothing to scan.
func TopSwap(int) []SwapProc { return nil }
