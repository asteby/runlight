//go:build linux

package main

import "github.com/dannyjp49/runlight/internal/sysmon"

func newSampler(psiCgroup string) sysmon.Sampler {
	return sysmon.Linux{DiskPath: "/var/lib/docker", PressureCgroup: psiCgroup}
}
