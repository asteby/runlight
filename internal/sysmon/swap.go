package sysmon

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file holds the portable, pure parts of the swap/tmpfs metrics so they
// can be tested anywhere; the Linux sampler feeds them real /proc data.

// parseVmstatSwap reads the cumulative pswpin/pswpout page counters from
// /proc/vmstat content. ok is false unless both are present.
func parseVmstatSwap(r io.Reader) (in, out uint64, ok bool) {
	var gotIn, gotOut bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "pswpin":
			n, err := strconv.ParseUint(f[1], 10, 64)
			in, gotIn = n, err == nil
		case "pswpout":
			n, err := strconv.ParseUint(f[1], 10, 64)
			out, gotOut = n, err == nil
		}
	}
	return in, out, gotIn && gotOut
}

// swapRate turns the cumulative page counters into KB/s between samples.
type swapRate struct {
	mu       sync.Mutex
	in, out  uint64
	at       time.Time
	hasPrior bool
	lastIn   float64 // last computed rates, reused for too-close samples
	lastOut  float64
}

// minSwapInterval is the shortest span a rate is computed over. Sample() has
// several callers (control loop, janitor), so two calls milliseconds apart
// would turn a couple of pages into a bogus KB/s spike.
const minSwapInterval = time.Second

// update records a new reading and returns the mean rates since the previous
// one. The first reading, a non-advancing clock or a counter that went
// backwards (reboot, wrap) yield zero rather than a bogus spike.
func (s *swapRate) update(in, out uint64, now time.Time, pageKB uint64) (inKBs, outKBs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pIn, pOut, pAt, had := s.in, s.out, s.at, s.hasPrior
	if had && now.Sub(pAt) > 0 && now.Sub(pAt) < minSwapInterval && in >= pIn && out >= pOut {
		return s.lastIn, s.lastOut // too close: keep the baseline, reuse the last rate
	}
	s.in, s.out, s.at, s.hasPrior = in, out, now, true
	s.lastIn, s.lastOut = 0, 0
	dt := now.Sub(pAt).Seconds()
	if !had || in < pIn || out < pOut || dt <= 0 {
		return 0, 0
	}
	s.lastIn = float64(in-pIn) * float64(pageKB) / dt
	s.lastOut = float64(out-pOut) * float64(pageKB) / dt
	return s.lastIn, s.lastOut
}

// tmpfsMB converts statfs block counts into used/total MB.
func tmpfsMB(blocks, bfree, bsize uint64) (usedMB, totalMB uint64) {
	if blocks == 0 || bfree > blocks {
		return 0, blocks * bsize >> 20
	}
	return (blocks - bfree) * bsize >> 20, blocks * bsize >> 20
}

// SwapProc is one process holding swapped-out pages.
type SwapProc struct {
	PID  int
	Name string
	MB   uint64
	Unit string // readable service/scope, "" if unknown
}

// parseStatusSwap pulls Name and VmSwap (kB) out of /proc/<pid>/status.
// Kernel threads have no VmSwap line: has is false.
func parseStatusSwap(r io.Reader) (name string, kb uint64, has bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "Name:"); ok {
			name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "VmSwap:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
					return name, n, true
				}
			}
		}
	}
	return name, 0, false
}

// unitFromCgroup extracts a readable unit from /proc/<pid>/cgroup content,
// e.g. "0::/system.slice/mariadb.service" -> "mariadb". "" if none is found.
func unitFromCgroup(content string) string {
	for _, line := range strings.Split(content, "\n") {
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		segs := strings.Split(strings.TrimSpace(line[i+1:]), "/")
		for j := len(segs) - 1; j >= 0; j-- {
			s := segs[j]
			for _, suf := range []string{".service", ".scope"} {
				if base, ok := strings.CutSuffix(s, suf); ok && base != "" {
					if suf == ".scope" && strings.HasPrefix(base, "docker-") {
						return "docker"
					}
					return base
				}
			}
		}
	}
	return ""
}

// ScanSwap walks procRoot (normally /proc) and returns the n processes with
// the most swapped-out memory, biggest first. The cost is capped: it looks at
// no more than maxProcs processes and stops once budget has elapsed, so it is
// safe to call from a control loop every few tens of seconds. Entries under
// 1 MB are ignored.
func ScanSwap(procRoot string, n, maxProcs int, budget time.Duration) []SwapProc {
	ents, err := os.ReadDir(procRoot)
	if err != nil || n <= 0 {
		return nil
	}
	deadline := time.Now().Add(budget)
	var found []SwapProc
	seen := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		if seen++; seen > maxProcs || (seen%64 == 0 && time.Now().After(deadline)) {
			break
		}
		dir := filepath.Join(procRoot, e.Name())
		f, err := os.Open(filepath.Join(dir, "status"))
		if err != nil {
			continue
		}
		name, kb, has := parseStatusSwap(io.LimitReader(f, 16<<10))
		f.Close()
		if !has || kb < 1024 {
			continue
		}
		p := SwapProc{PID: pid, Name: name, MB: kb / 1024}
		if b, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			p.Unit = unitFromCgroup(string(b))
		}
		found = append(found, p)
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].MB != found[j].MB {
			return found[i].MB > found[j].MB
		}
		return found[i].PID < found[j].PID
	})
	if len(found) > n {
		found = found[:n]
	}
	return found
}
