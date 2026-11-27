// Package sysmon samples host health for the governor.
package sysmon

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"github.com/dannyjp49/runlight/internal/governor"
)

// Sampler returns the current host signals.
type Sampler interface {
	Sample() (governor.Signals, error)
}

// parsePSI extracts the "some avg10" value from a /proc/pressure/* file.
//
//	some avg10=1.23 avg60=0.50 avg300=0.10 total=12345
//	full avg10=0.00 ...
func parsePSI(r io.Reader) (float64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				n, err := strconv.ParseFloat(v, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}

// parseMemAvailMB reads MemAvailable from /proc/meminfo content.
func parseMemAvailMB(r io.Reader) (uint64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(f[0], 10, 64)
		return kb / 1024, err == nil
	}
	return 0, false
}

// parseMeminfoKB returns every "Key:   123 kB" line of /proc/meminfo as kB.
func parseMeminfoKB(r io.Reader) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// parseLoad1 reads the 1-minute load average from /proc/loadavg content.
func parseLoad1(r io.Reader) (float64, bool) {
	b, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(f[0], 64)
	return n, err == nil
}
