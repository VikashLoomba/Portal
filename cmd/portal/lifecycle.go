package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
)

func newUninstallCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop, remove the agent, and tear down the ssh master",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = a.Service.Uninstall(cmd.Context())
			host, _ := a.Cfg.ReadHost()
			if host != "" {
				// Remove the xdg-open wrapper, clipboard shims, portald
				// symlink, and PATH markers before tearing down the master
				// (while the connection is still live).
				removePortalWrappers(cmd.Context(), a)
				// Clean up the remote ~/.cache/portal/ agent binaries.
				if a.Bootstrap != nil {
					_ = a.Bootstrap.PruneAll(cmd.Context())
				}
				_, _ = a.Transport.Exit(cmd.Context())
			}
			_ = os.Remove(a.Paths.Sock)
			// Best-effort: a stale api.sock must never block a fresh daemon.
			_ = os.Remove(a.Paths.APISock)
			_ = os.Remove(a.Paths.BinPath)
			_ = os.RemoveAll(a.Paths.ConfigDir)
			fmt.Printf("uninstalled (%s)\n", a.Paths.Label)
			return nil
		},
	}
}

func newReloadCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Re-apply config/plist changes (after editing this script)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Service.Reload(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("reloaded (%s)\n", a.Paths.Label)
			return nil
		},
	}
}

func newStartCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start (load) the forwarding service",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Service.Start(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("started (%s)\n", a.Paths.Label)
			return nil
		},
	}
}

func newStopCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop forwarding now (unload service + drop all forwards)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Service.Stop(cmd.Context()); err != nil {
				return err
			}
			host, _ := a.Cfg.ReadHost()
			if host != "" {
				stopped, _ := a.Transport.Exit(cmd.Context())
				if stopped {
					// Mirror bash: only print this line when the master responded.
					fmt.Println("master stopped")
				}
			}
			_ = os.Remove(a.Paths.Sock)
			// Best-effort: a stale api.sock must never block a fresh daemon.
			_ = os.Remove(a.Paths.APISock)
			fmt.Printf("stopped (%s) — run '%s start' to resume\n", a.Paths.Label, app.Tool)
			return nil
		},
	}
}

func newRestartCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Force-restart the running service",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.Service.Restart(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("restarted (%s)\n", a.Paths.Label)
			return nil
		},
	}
}

func newHostCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "host [newhost]",
		Short: "Show the configured dev box, or switch to a new one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cur, _ := a.Cfg.ReadHost()
			if len(args) == 0 {
				if cur == "" {
					fmt.Printf("(no dev box configured — run: %s install <ssh-host>)\n", app.Tool)
				} else {
					fmt.Println(cur)
				}
				return nil
			}
			newHost := stripWhitespace(args[0])
			if newHost == "" {
				return usageErr{msg: "no host given"}
			}
			// Tear down the old master before switching, ignoring errors.
			if cur != "" {
				_, _ = a.Transport.Exit(cmd.Context())
			}
			_ = os.Remove(a.Paths.Sock)
			if err := a.Cfg.WriteHost(newHost); err != nil {
				return err
			}
			fmt.Printf("dev box changed to: %s\n", newHost)
			loaded, _ := a.Service.IsLoaded(cmd.Context())
			if loaded {
				if err := a.Service.Restart(cmd.Context()); err != nil {
					return err
				}
				fmt.Printf("restarted (%s)\n", a.Paths.Label)
			}
			return nil
		},
	}
}
