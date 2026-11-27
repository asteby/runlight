//go:build !linux

package sysmon

// ReadCG has no cgroups to read off Linux.
func ReadCG(string) CG { return CG{} }
