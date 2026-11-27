package backend

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
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
