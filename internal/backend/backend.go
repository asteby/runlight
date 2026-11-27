// Package backend abstracts how a runner process is started and destroyed.
package backend

import "context"

// Spec describes one ephemeral runner to launch.
type Spec struct {
	Name      string // unique runner name, also the container name
	JITConfig string // encoded JIT config from GitHub
	MemoryMB  int    // hard memory cap, 0 = unlimited
	CPUs      float64
}

// Backend launches and tears down ephemeral runners.
type Backend interface {
	// Start launches the runner and returns immediately.
	Start(ctx context.Context, s Spec) error
	// Stop forcibly destroys the runner. It must be idempotent.
	Stop(ctx context.Context, name string) error
	// Exited returns a channel of runner names whose process ended on its own
	// (finished its single job, or crashed).
	Exited() <-chan string
}
