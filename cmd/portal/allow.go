package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/vikashl/portal/internal/app"
)

func newAllowCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "allow <port>...",
		Short: "Force-forward a port even if it's in the ephemeral range or denylist",
		// No Args validator: bash exits 2 with a "usage:" line on no args, so
		// we surface that ourselves rather than letting Cobra print its
		// generic "Error: requires at least 1 arg(s)" + exit 1.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "usage: %s allow <port>...\n", app.Tool)
				return usageErr{msg: ""}
			}
			ports, bad := parsePorts(args)
			for _, b := range bad {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipping non-numeric port: %s\n", b)
			}
			if len(ports) == 0 {
				return nil
			}
			added, err := a.Cfg.Allow(ports)
			if err != nil {
				return err
			}
			for _, p := range ports {
				if !contains(added, p) {
					fmt.Printf("already allowed: %d\n", p)
				}
			}
			if len(added) > 0 {
				fmt.Printf("allowed:")
				for _, p := range added {
					fmt.Printf(" %d", p)
				}
				// Allowlist file updated; nudge the agent so the change takes
				// effect within tens of ms instead of after the safety
				// reconcile. Best-effort — Subscribe is buffered if the
				// running daemon hasn't connected yet.
				pushAllowlist(a)
				fmt.Printf(" (takes effect within ~100ms)\n")
			}
			return nil
		},
	}
}

func newUnallowCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "unallow <port>...",
		Short: "Remove ports from the allowlist",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "usage: %s unallow <port>...\n", app.Tool)
				return usageErr{msg: ""}
			}
			// Bash short-circuit: if no allow file exists, print
			// "allowlist is empty" and return without creating it. Matches
			// portal:611.
			if _, err := os.Stat(a.Cfg.AllowFilePath()); os.IsNotExist(err) {
				fmt.Println("allowlist is empty")
				return nil
			}
			// Bash does no numeric validation here — it just runs grep -vxF.
			// Pass strings through; numeric ones drop from the file, others
			// no-op. We still print the per-arg "unallowed:" line for parity.
			ports, _ := parsePorts(args)
			if err := a.Cfg.Unallow(ports); err != nil {
				return err
			}
			pushAllowlist(a)
			for _, raw := range args {
				fmt.Printf("unallowed: %s (forward drops within ~100ms if in ephemeral range)\n", raw)
			}
			return nil
		},
	}
}

// pushAllowlist sends a fresh Subscribe to the agent (best-effort). When
// invoked from a transient `portal allow N` command the AgentClient is not
// connected — the call is a no-op. The running launchd daemon's instance
// will pick up the file change on its next Reconcile (safety ticker, 60s)
// or sooner if its allow file is re-read on a tighter cadence.
//
// In a future iteration we'd add an fsnotify watcher in the daemon so it
// re-Subscribes on every mutation; for now the safety reconcile is good
// enough.
func pushAllowlist(a *app.App) {
	if a.AgentClient == nil {
		return
	}
	allow, _ := a.Cfg.AllowedPorts()
	_ = a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)
}

func newAllowedCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "allowed",
		Short: "Show the current allowlist",
		RunE: func(cmd *cobra.Command, args []string) error {
			ports, err := a.Cfg.AllowedPorts()
			if err != nil {
				return err
			}
			if len(ports) == 0 {
				fmt.Printf("allowlist empty (file: %s)\n", a.Cfg.AllowFilePath())
				return nil
			}
			fmt.Println("allowlisted ports (forwarded even if ephemeral/denied):")
			for _, p := range ports {
				fmt.Printf("  %d\n", p)
			}
			return nil
		},
	}
}

func parsePorts(args []string) (ports []int, bad []string) {
	for _, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil || n <= 0 {
			bad = append(bad, a)
			continue
		}
		ports = append(ports, n)
	}
	return ports, bad
}

func contains(ints []int, n int) bool {
	for _, x := range ints {
		if x == n {
			return true
		}
	}
	return false
}
