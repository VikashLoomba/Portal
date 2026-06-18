package main

import (
	"context"
	"errors"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
)

// waitForSnapshot polls AgentClient.Snapshot() until it returns ok or the
// timeout (in ms) elapses. Used by `once` so the first Reconcile has real
// data to work with.
func waitForSnapshot(ctx context.Context, a *app.App, timeoutMS int) error {
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	for {
		if _, _, ok := a.AgentClient.Snapshot(); ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for agent snapshot")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
