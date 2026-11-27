//go:build !linux

package sysmon

import "github.com/dannyjp49/runlight/internal/governor"

// Other is a development stub for non-Linux hosts: it reports a calm,
// roomy machine so the daemon can be exercised locally.
type Other struct{ DiskPath string }

func (Other) Sample() (governor.Signals, error) {
	return governor.Signals{MemAvailMB: 8192, DiskFreeMB: 100000, DiskTotalMB: 200000}, nil
}
