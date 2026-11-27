package backend

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Regression: Stop("") used to RemoveAll(JobsDir), wiping every running job.
func TestStopRefusesUnsafeNamesAndNeverWipesJobsDir(t *testing.T) {
	jobs := t.TempDir()
	keep := filepath.Join(jobs, "runlight-live")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewSystemd("/tpl", jobs, "ubuntu", slog.New(slog.DiscardHandler))
	s.Elevate = []string{"true"} // neutralise systemctl

	for _, bad := range []string{"", ".", "..", "../x", "runlight-/../..", "other", "runlight-a/b"} {
		_ = s.Stop(context.Background(), bad)
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("Stop(%q) damaged the jobs dir: %v", bad, err)
		}
	}
	_ = s.Stop(context.Background(), "runlight-live")
	if _, err := os.Stat(keep); !os.IsNotExist(err) {
		t.Fatal("a valid Stop must remove its own job directory")
	}
}

func TestRunArgs(t *testing.T) {
	s := NewSystemd("/tpl", "/jobs", "ubuntu", slog.New(slog.DiscardHandler))
	s.Slice = "runlight.slice"
	s.Nice = 10
	s.RuntimeMax = 90 * time.Minute
	s.Env = []string{"GOMAXPROCS=2", "PATH=/a:/b"}

	args := s.runArgs(Spec{Name: "runlight-abc", JITConfig: "SECRET-JIT", MemoryMB: 3000, CPUs: 2}, "/jobs/runlight-abc")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--unit=runlight-abc", "--uid=ubuntu", "--slice=runlight.slice",
		"--working-directory=/jobs/runlight-abc",
		"CPUQuota=200%", "MemoryMax=3000M", "MemoryHigh=2550M",
		"RuntimeMaxSec=5400", "Nice=10",
		"--setenv=GOMAXPROCS=2", "--setenv=PATH=/a:/b", "--wait",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	// The credential must never be part of argv.
	if strings.Contains(joined, "SECRET-JIT") {
		t.Fatal("JIT config leaked into argv")
	}
}

func TestRunArgsOptionalLimitsOmitted(t *testing.T) {
	s := NewSystemd("/tpl", "/jobs", "ubuntu", slog.New(slog.DiscardHandler))
	joined := strings.Join(s.runArgs(Spec{Name: "r"}, "/jobs/r"), " ")
	for _, bad := range []string{"CPUQuota", "MemoryMax", "--slice", "RuntimeMaxSec"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unexpected %q: %s", bad, joined)
		}
	}
}

func TestRunArgsTmpdirPerJob(t *testing.T) {
	s := NewSystemd("/tpl", "/jobs", "ubuntu", slog.New(slog.DiscardHandler))
	args := s.runArgs(Spec{Name: "runlight-x"}, "/jobs/runlight-x")
	want := "--setenv=TMPDIR=" + filepath.Join("/jobs/runlight-x", "tmp")
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one %q in %v", want, args)
	}
}

func TestRunArgsEnvTmpdirWins(t *testing.T) {
	s := NewSystemd("/tpl", "/jobs", "ubuntu", slog.New(slog.DiscardHandler))
	s.Env = []string{"PATH=/bin", "TMPDIR=/mnt/big"}
	joined := strings.Join(s.runArgs(Spec{Name: "r"}, "/jobs/r"), " ")
	if !strings.Contains(joined, "--setenv=TMPDIR=/mnt/big") {
		t.Fatalf("Env TMPDIR missing: %s", joined)
	}
	if strings.Count(joined, "TMPDIR=") != 1 {
		t.Fatalf("TMPDIR must appear once: %s", joined)
	}
}

func TestRunArgsNoSwap(t *testing.T) {
	s := NewSystemd("/tpl", "/jobs", "ubuntu", slog.New(slog.DiscardHandler))
	if strings.Contains(strings.Join(s.runArgs(Spec{Name: "r"}, "/jobs/r"), " "), "MemorySwapMax") {
		t.Fatal("MemorySwapMax must be absent by default")
	}
	s.NoSwap = true
	args := s.runArgs(Spec{Name: "r"}, "/jobs/r")
	ok := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-p" && args[i+1] == "MemorySwapMax=0" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("missing -p MemorySwapMax=0: %v", args)
	}
}

func TestMakeJobTmpCreates0700(t *testing.T) {
	jobDir := t.TempDir()
	s := NewSystemd("/tpl", filepath.Dir(jobDir), "nonexistent-user-xyz", slog.New(slog.DiscardHandler))
	s.Elevate = []string{"true"}
	if err := s.makeJobTmp(context.Background(), jobDir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(jobDir, "tmp"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("tmp not created: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("perm = %v, want 0700", fi.Mode().Perm())
	}
}
