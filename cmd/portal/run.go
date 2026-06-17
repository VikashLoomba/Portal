package main

import (
	"context"
	"fmt"
	"os/signal"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/vikashl/portal/internal/app"
)

func newRunCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the forwarding loop in the foreground (used by launchd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _ := a.Cfg.ReadHost()
			if host == "" {
				return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// Push the initial Subscribe so the agent has the latest filter
			// even before its first connect — Subscribe is buffered until
			// the encoder lands, then replayed.
			allow, _ := a.Cfg.AllowedPorts()
			_ = a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)

			engine := a.Engine()

			// Run agent supervisor + reconcile engine in parallel; either
			// returning ends the daemon (launchd will relaunch).
			var wg sync.WaitGroup
			wg.Add(2)
			errCh := make(chan error, 2)

			go func() {
				defer wg.Done()
				errCh <- a.AgentClient.Run(ctx)
			}()
			go func() {
				defer wg.Done()
				errCh <- engine.Run(ctx)
			}()

			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newOnceCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "Do a single reconcile pass, then print status",
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _ := a.Cfg.ReadHost()
			if host == "" {
				return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
			}
			// Spin the agent up briefly to populate Snapshot, then
			// reconcile once. Tests use this to validate end-to-end.
			ctx := cmd.Context()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allowOrEmpty(a)), true)
				_ = a.AgentClient.Run(ctx)
			}()
			defer func() {
				_ = a.AgentClient.Shutdown(ctx, "once")
				<-done
			}()

			// Wait briefly for the Subscribe→Snapshot round-trip.
			if err := waitForSnapshot(ctx, a, snapshotWaitMS); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			}
			if err := a.Engine().Reconcile(ctx); err != nil {
				_ = err
			}
			return runStatus(ctx, a)
		},
	}
}

const snapshotWaitMS = 5000

// runStatusCtx is a tiny helper so once.go and the root status default share.
func runStatusCtx(ctx context.Context, a *app.App) error { return runStatus(ctx, a) }

// toU16 narrows []int → []uint16, dropping out-of-range values.
func toU16(in []int) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		if v <= 0 || v > 65535 {
			continue
		}
		out = append(out, uint16(v))
	}
	return out
}

func allowOrEmpty(a *app.App) []int {
	ps, err := a.Cfg.AllowedPorts()
	if err != nil {
		return nil
	}
	return ps
}
