package backend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const jitEnv = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// Docker runs each runner as a throw-away container via the docker CLI.
// Using the CLI instead of the Docker SDK keeps the binary small and the
// dependency tree empty.
type Docker struct {
	Image string
	Bin   string // docker binary, default "docker"
	// CgroupParent places every runner under a systemd slice (e.g.
	// "runlight.slice") so the kernel prefers production over CI under contention.
	CgroupParent string
	Log          *slog.Logger
	exited       chan string
}

func NewDocker(image string, log *slog.Logger) *Docker {
	return &Docker{Image: image, Bin: "docker", Log: log, exited: make(chan string, 256)}
}

func (d *Docker) Exited() <-chan string { return d.exited }

func (d *Docker) Start(ctx context.Context, s Spec) error {
	args := []string{
		"run", "--rm", "--name", s.Name,
		// Value-less -e: docker reads it from its own environment, so the
		// credential never appears in `ps` output.
		"-e", jitEnv,
		"--label", "runlight=1",
		"--init",
		"--pids-limit", "2048",
	}
	if s.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(s.MemoryMB)+"m")
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', 2, 64))
	}
	if d.CgroupParent != "" {
		args = append(args, "--cgroup-parent", d.CgroupParent)
	}
	args = append(args, d.Image, "/home/runner/run.sh")

	// Deliberately NOT bound to ctx: runners must survive a Scale() call
	// returning. They are stopped explicitly via Stop or at shutdown.
	cmd := exec.Command(d.Bin, args...)
	cmd.Env = append(os.Environ(), jitEnv+"="+s.JITConfig)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker run %s: %w", s.Name, err)
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			d.Log.Debug("runner container exited with error", "name", s.Name, "err", err)
		}
		d.exited <- s.Name
	}()
	return nil
}

func (d *Docker) Stop(ctx context.Context, name string) error {
	if !validName(name) {
		return nil
	}
	out, err := exec.CommandContext(ctx, d.Bin, "rm", "-f", name).CombinedOutput()
	if err != nil {
		// "No such container" is fine: Stop is idempotent.
		d.Log.Debug("docker rm", "name", name, "out", string(out), "err", err)
	}
	return nil
}

// Sweep removes leftover runlight containers from a previous crashed run.
func (d *Docker) Sweep(ctx context.Context) error {
	ids, err := exec.CommandContext(ctx, d.Bin, "ps", "-aq", "--filter", "label=runlight=1").Output()
	if err != nil {
		return fmt.Errorf("docker ps: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, strings.Fields(string(ids))...)
	return exec.CommandContext(ctx, d.Bin, args...).Run()
}
