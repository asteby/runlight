package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/actions/scaleset"
)

// openSession creates the message session. GitHub allows one session per
// scale set, so after a crash or SIGKILL the previous session lingers for a
// while and the API answers 409 Conflict. Wait for it to expire instead of
// dying and relying on a restart loop.
func openSession(ctx context.Context, c *scaleset.Client, scaleSetID int, owner string, log *slog.Logger) (*scaleset.MessageSessionClient, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		s, err := c.MessageSessionClient(ctx, scaleSetID, owner)
		conflict := err != nil && (errors.Is(err, scaleset.ConflictError) || strings.Contains(err.Error(), "conflict"))
		if err == nil || !conflict || time.Now().After(deadline) {
			return s, err
		}
		log.Warn("previous session still active, waiting for it to expire", "retry_in", "5s")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
