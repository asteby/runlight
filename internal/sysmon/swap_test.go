package sysmon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseVmstatSwap(t *testing.T) {
	in := "nr_free_pages 123\npswpin 1000\npswpout 2500\npswpout_x 9\n"
	i, o, ok := parseVmstatSwap(strings.NewReader(in))
	if !ok || i != 1000 || o != 2500 {
		t.Fatalf("got %d %d %v", i, o, ok)
	}
	for _, bad := range []string{"", "pswpin 5\n", "pswpout 5\n", "pswpin x\npswpout 1\n", "garbage"} {
		if _, _, ok := parseVmstatSwap(strings.NewReader(bad)); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestSwapRate(t *testing.T) {
	var s swapRate
	t0 := time.Unix(1000, 0)
	if i, o := s.update(100, 100, t0, 4); i != 0 || o != 0 {
		t.Fatal("first sample has no rate")
	}
	// 500 pages in, 250 out in 5 s => 400 / 200 KB/s.
	i, o := s.update(600, 350, t0.Add(5*time.Second), 4)
	if i != 400 || o != 200 {
		t.Fatalf("rates = %v %v", i, o)
	}
	// Parked swap: counters do not move => 0.
	if i, o := s.update(600, 350, t0.Add(10*time.Second), 4); i != 0 || o != 0 {
		t.Fatalf("idle rates = %v %v", i, o)
	}
	// Counter went backwards (reboot): no negative or huge spike.
	if i, o := s.update(5, 5, t0.Add(15*time.Second), 4); i != 0 || o != 0 {
		t.Fatalf("reset rates = %v %v", i, o)
	}
	// Clock that did not advance.
	if i, o := s.update(100, 100, t0.Add(15*time.Second), 4); i != 0 || o != 0 {
		t.Fatalf("dt=0 rates = %v %v", i, o)
	}
}

func TestTmpfsMB(t *testing.T) {
	// 12 GB of 4 KB blocks, half used.
	blocks := uint64(12<<30) / 4096
	u, tot := tmpfsMB(blocks, blocks/2, 4096)
	if u != 6144 || tot != 12288 {
		t.Fatalf("got %d/%d", u, tot)
	}
	if u, tot := tmpfsMB(0, 0, 4096); u != 0 || tot != 0 {
		t.Fatal("empty fs")
	}
	if u, _ := tmpfsMB(10, 20, 4096); u != 0 {
		t.Fatal("bfree > blocks must not underflow")
	}
}

func TestParseStatusSwap(t *testing.T) {
	st := "Name:\tmariadbd\nUmask:\t0022\nVmRSS:\t  1000 kB\nVmSwap:\t 2365440 kB\nThreads:\t9\n"
	n, kb, ok := parseStatusSwap(strings.NewReader(st))
	if !ok || n != "mariadbd" || kb != 2365440 {
		t.Fatalf("got %q %d %v", n, kb, ok)
	}
	if _, _, ok := parseStatusSwap(strings.NewReader("Name:\tkthreadd\nState:\tS\n")); ok {
		t.Fatal("kernel thread has no VmSwap")
	}
}

func TestUnitFromCgroup(t *testing.T) {
	cases := map[string]string{
		"0::/system.slice/mariadb.service\n":                                   "mariadb",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.service": "x",
		"0::/user.slice/user-1000.slice/session-3.scope\n":                     "session-3",
		"0::/system.slice/docker-abcdef.scope\n":                               "docker",
		"0::/\n":                                                               "",
		"":                                                                     "",
		"0::/init.scope":                                                       "init",
	}
	for in, want := range cases {
		if got := unitFromCgroup(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func fakeProc(t *testing.T, root string, pid int, name string, swapKB int, cgroup string) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	st := "Name:\t" + name + "\n"
	if swapKB >= 0 {
		st += "VmSwap:\t" + strconv.Itoa(swapKB) + " kB\n"
	}
	if err := os.WriteFile(filepath.Join(d, "status"), []byte(st), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "cgroup"), []byte(cgroup), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanSwapOrdersCapsAndSkips(t *testing.T) {
	root := t.TempDir()
	fakeProc(t, root, 10, "small", 2048, "0::/system.slice/small.service\n")
	fakeProc(t, root, 11, "big", 3<<20, "0::/system.slice/big.service\n")
	fakeProc(t, root, 12, "mid", 500<<10, "")
	fakeProc(t, root, 13, "kthread", -1, "") // no VmSwap
	fakeProc(t, root, 14, "tiny", 900, "")   // under 1 MB
	fakeProc(t, root, 15, "zero", 0, "")     // nothing swapped
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meminfo"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ScanSwap(root, 8, 100, time.Second)
	if len(got) != 3 || got[0].Name != "big" || got[0].MB != 3072 || got[0].Unit != "big" ||
		got[1].Name != "mid" || got[1].Unit != "" || got[2].Name != "small" {
		t.Fatalf("got %+v", got)
	}
	if got := ScanSwap(root, 2, 100, time.Second); len(got) != 2 || got[1].Name != "mid" {
		t.Fatalf("cap n: %+v", got)
	}
	// maxProcs bounds the work: at most one process is looked at.
	if got := ScanSwap(root, 8, 1, time.Second); len(got) > 1 {
		t.Fatalf("maxProcs ignored: %+v", got)
	}
	if got := ScanSwap(filepath.Join(root, "nope"), 8, 100, time.Second); got != nil {
		t.Fatalf("missing root: %+v", got)
	}
	if got := ScanSwap(root, 0, 100, time.Second); got != nil {
		t.Fatal("n=0")
	}
}

func TestSwapRateIgnoresSamplesTooClose(t *testing.T) {
	var r swapRate
	t0 := time.Unix(1000, 0)
	r.update(0, 0, t0, 4)
	in, _ := r.update(250, 0, t0.Add(10*time.Second), 4) // 100 KB/s
	if in != 100 {
		t.Fatalf("in = %v", in)
	}
	// A second caller 5 ms later must not turn 1 page into 800 KB/s.
	if in, _ := r.update(251, 0, t0.Add(10*time.Second+5*time.Millisecond), 4); in != 100 {
		t.Fatalf("close sample = %v, want the previous 100", in)
	}
	// The baseline was kept, so the next real interval covers everything since.
	if in, _ := r.update(260, 0, t0.Add(20*time.Second), 4); in != 4 { // 10 pages*4KB/10s
		t.Fatalf("next = %v, want 4", in)
	}
}
