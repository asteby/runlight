package backend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Systemd runs each ephemeral runner natively on the host (so deploy jobs can
// reach host services), isolated in its own transient systemd unit with
// hard CPU/memory/task limits and placed under a low-priority slice.
//
// Per job it hard-links a pre-extracted runner template (near-zero disk and
// time), starts it via `systemd-run --wait`, and deletes the directory when
// the runner exits.
type Systemd struct {
	Template string   // extracted actions/runner directory
	JobsDir  string   // parent of per-job copies
	RunAs    string   // unix user jobs run as
	Slice    string   // e.g. runlight.slice
	Env      []string // KEY=VALUE injected into every job (PATH, GOCACHE...)
	Nice     int
	// Elevate is the command prefix used to call systemd-run/systemctl, e.g.
	// ["sudo","-n"]. Empty when the daemon already runs as root.
	Elevate    []string
	RuntimeMax time.Duration
	Log        *slog.Logger
	exited     chan string
}

func NewSystemd(template, jobsDir, runAs string, log *slog.Logger) *Systemd {
	return &Systemd{
		Template: template, JobsDir: jobsDir, RunAs: runAs,
		Elevate: []string{"sudo", "-n"}, Log: log, exited: make(chan string, 256),
	}
}

func (s *Systemd) Exited() <-chan string { return s.exited }

func unitName(name string) string { return name + ".service" }

// runArgs builds the systemd-run argument list (pure, unit-tested).
func (s *Systemd) runArgs(sp Spec, jobDir string) []string {
	a := []string{
		"systemd-run", "--wait", "--collect", "--quiet",
		"--unit=" + name2unit(sp.Name),
		"--uid=" + s.RunAs, "--gid=" + s.RunAs,
		"--working-directory=" + jobDir,
		"-p", "TasksMax=4096",
		"-p", "Nice=" + strconv.Itoa(s.Nice),
	}
	if s.Slice != "" {
		a = append(a, "--slice="+s.Slice)
	}
	if sp.CPUs > 0 {
		a = append(a, "-p", fmt.Sprintf("CPUQuota=%d%%", int(sp.CPUs*100)))
	}
	if sp.MemoryMB > 0 {
		a = append(a, "-p", fmt.Sprintf("MemoryHigh=%dM", sp.MemoryMB*85/100),
			"-p", fmt.Sprintf("MemoryMax=%dM", sp.MemoryMB))
	}
	if s.RuntimeMax > 0 {
		a = append(a, "-p", fmt.Sprintf("RuntimeMaxSec=%d", int(s.RuntimeMax.Seconds())))
	}
	for _, e := range s.Env {
		a = append(a, "--setenv="+e)
	}
	// The JIT credential travels via a 0600 file consumed (and removed) by the
	// wrapper, so it never shows up in argv, `ps` or `systemctl show`.
	a = append(a, "/bin/sh", "-c",
		`J=$(cat .jit); rm -f .jit; ACTIONS_RUNNER_INPUT_JITCONFIG="$J" exec ./run.sh`)
	return a
}

func name2unit(n string) string { return n }

func (s *Systemd) Start(ctx context.Context, sp Spec) error {
	jobDir := filepath.Join(s.JobsDir, sp.Name)
	if err := os.MkdirAll(s.JobsDir, 0o755); err != nil {
		return err
	}
	// Hard links: the 600+ MB runner is "copied" in milliseconds.
	if out, err := exec.CommandContext(ctx, "cp", "-al", s.Template, jobDir).CombinedOutput(); err != nil {
		return fmt.Errorf("clone template: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.WriteFile(filepath.Join(jobDir, ".jit"), []byte(sp.JITConfig), 0o600); err != nil {
		os.RemoveAll(jobDir)
		return err
	}
	args := append(append([]string{}, s.Elevate...), s.runArgs(sp, jobDir)...)
	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(jobDir)
		return fmt.Errorf("systemd-run %s: %w", sp.Name, err)
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			s.Log.Debug("runner unit exited with error", "name", sp.Name, "err", err)
		}
		os.RemoveAll(jobDir)
		s.exited <- sp.Name
	}()
	return nil
}

// validName guards every destructive operation derived from a runner name:
// it must be a plain "runlight-…" identifier, never empty and never a path.
// An empty name once made Stop delete the whole jobs directory.
func validName(name string) bool {
	return strings.HasPrefix(name, "runlight-") && !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

func (s *Systemd) Stop(ctx context.Context, name string) error {
	if !validName(name) {
		s.Log.Warn("refusing to stop runner with unsafe name", "name", name)
		return nil
	}
	args := append(append([]string{}, s.Elevate...), "systemctl", "stop", unitName(name))
	if out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
		s.Log.Debug("systemctl stop", "name", name, "out", strings.TrimSpace(string(out)))
	}
	os.RemoveAll(filepath.Join(s.JobsDir, name))
	return nil
}

// Sweep stops leftover runlight units and removes stale job directories from
// a previous crashed run.
func (s *Systemd) Sweep(ctx context.Context) error {
	args := append(append([]string{}, s.Elevate...), "systemctl", "list-units", "runlight-*.service",
		"--plain", "--no-legend", "--all")
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if err != nil {
		return fmt.Errorf("list units: %w", err)
	}
	live := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "runlight-") {
			continue
		}
		name := strings.TrimSuffix(f[0], ".service")
		// A restart of the daemon must never kill a job that is still
		// running: leave units whose Runner.Worker is alive untouched.
		worker := filepath.Join(s.JobsDir, name, "bin", "Runner.Worker")
		if exec.CommandContext(ctx, "pgrep", "-f", worker).Run() == nil {
			s.Log.Info("leaving running job from a previous daemon", "runner", name)
			live[name] = true
			continue
		}
		stop := append(append([]string{}, s.Elevate...), "systemctl", "stop", f[0])
		_ = exec.CommandContext(ctx, stop[0], stop[1:]...).Run()
	}
	entries, _ := os.ReadDir(s.JobsDir)
	for _, e := range entries {
		if !live[e.Name()] {
			os.RemoveAll(filepath.Join(s.JobsDir, e.Name()))
		}
	}
	return nil
}
