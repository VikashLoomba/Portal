package main

import (
	"context"
	"fmt"
	"os/signal"
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
			return a.Engine().Run(ctx)
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
			if err := a.Engine().Reconcile(cmd.Context()); err != nil {
				// Don't fail hard — reconcile errors are normal during transient
				// network blips; status output below shows the live state.
				_ = err
			}
			return runStatus(cmd.Context(), a)
		},
	}
}

// runStatusCtx is a tiny helper so once.go and the root status default share.
func runStatusCtx(ctx context.Context, a *app.App) error { return runStatus(ctx, a) }
