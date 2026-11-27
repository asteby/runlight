package dashboard

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proc is one process in the "top consumers" panels.
type Proc struct {
	PID   int     `json:"pid"`
	User  string  `json:"user"`
	CPU   float64 `json:"cpu"` // instantaneous, % of one core
	Mem   float64 `json:"mem"` // % of RAM
	RSSMB float64 `json:"rssMb"`
	Name  string  `json:"name"`
}

// Procs is the payload of /api/procs.
type Procs struct {
	AtMs  int64  `json:"atMs"`
	ByCPU []Proc `json:"byCpu"`
	ByMem []Proc `json:"byMem"`
}

// parseTopMemKiB converts top's RES column ("392740", "53m", "1.1g") to KiB.
func parseTopMemKiB(s string) float64 {
	mult := 1.0
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'm':
			mult, s = 1024, s[:n-1]
		case 'g':
			mult, s = 1024*1024, s[:n-1]
		case 't':
			mult, s = 1024*1024*1024, s[:n-1]
		case 'k':
			s = s[:n-1]
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v * mult
}

// parseTop reads the LAST frame of `top -b -n 2`: the first frame reports
// averages since process start, the second the live rate over the delay.
func parseTop(out string) []Proc {
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if f := strings.Fields(l); len(f) >= 12 && f[0] == "PID" && f[1] == "USER" {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	var procs []Proc
	for _, l := range lines[start+1:] {
		f := strings.Fields(l)
		if len(f) < 12 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.ReplaceAll(f[8], ",", "."), 64)
		mem, _ := strconv.ParseFloat(strings.ReplaceAll(f[9], ",", "."), 64)
		name := strings.Join(f[11:], " ")
		if name == "top" {
			continue // our own sampler
		}
		procs = append(procs, Proc{PID: pid, User: f[1], CPU: cpu, Mem: mem, RSSMB: parseTopMemKiB(f[5]) / 1024, Name: name})
	}
	return procs
}

func topN(all []Proc, n int, less func(a, b Proc) bool) []Proc {
	c := append([]Proc(nil), all...)
	sort.Slice(c, func(i, j int) bool { return less(c[i], c[j]) })
	if len(c) > n {
		c = c[:n]
	}
	return c
}

// procSampler caches the result: sampling costs about a second of `top`, so
// it runs at most every few seconds and only while someone is watching.
type procSampler struct {
	mu   sync.Mutex
	at   time.Time
	last Procs
	ttl  time.Duration
}

func (p *procSampler) get(ctx context.Context) Procs {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.at) < p.ttl {
		return p.last
	}
	cmd := exec.CommandContext(ctx, "top", "-b", "-n", "2", "-d", "0.7", "-w", "256", "-o", "%CPU")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "COLUMNS=256")
	if out, err := cmd.Output(); err == nil {
		all := parseTop(string(out))
		p.last = Procs{
			AtMs:  time.Now().UnixMilli(),
			ByCPU: topN(all, 8, func(a, b Proc) bool { return a.CPU > b.CPU }),
			ByMem: topN(all, 8, func(a, b Proc) bool { return a.RSSMB > b.RSSMB }),
		}
	}
	p.at = time.Now()
	return p.last
}
