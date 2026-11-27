//go:build !linux

package main

import "github.com/dannyjp49/runlight/internal/sysmon"

func newSampler(string) sysmon.Sampler { return sysmon.Other{} }
