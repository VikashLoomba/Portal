// Package main is the portal CLI entry point. It builds the Cobra command
// tree, wires the App composition root, and dispatches to the per-command
// files in this directory. Each command's Run* function is a thin shell
// around internal packages — there is no business logic here.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vikashl/portal/internal/app"
)

// version is overridable via -ldflags "-X main.version=…" at build time.
var version = "dev"

// usageErr signals "exit 2" to main (bad arg counts, unknown command, etc.).
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

// errSilent signals "exit 1, but DON'T print the error message" (caller has
// already printed something custom to stderr, e.g. `ports`'s "could not
// reach <host>" line which should not be wrapped).
var errSilent = errors.New("")

func newRootCmd(a *app.App) *cobra.Command {
	root := &cobra.Command{
		Use:           app.Tool,
		Short:         "Dynamic SSH port forwarding from a remote dev box to this Mac",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Default behavior with no args = status (matches bash).
		RunE: func(cmd *cobra.Command, args []string) error { return runStatus(cmd.Context(), a) },
	}
	root.Version = version
	root.SetHelpTemplate(helpText(a))

	root.AddCommand(newRunCmd(a))
	root.AddCommand(newOnceCmd(a))
	root.AddCommand(newInstallCmd(a))
	root.AddCommand(newUninstallCmd(a))
	root.AddCommand(newReloadCmd(a))
	root.AddCommand(newHostCmd(a))
	root.AddCommand(newStartCmd(a))
	root.AddCommand(newStopCmd(a))
	root.AddCommand(newRestartCmd(a))
	root.AddCommand(newStatusCmd(a))
	root.AddCommand(newPortsCmd(a))
	root.AddCommand(newLogsCmd(a))
	root.AddCommand(newAllowCmd(a))
	root.AddCommand(newUnallowCmd(a))
	root.AddCommand(newAllowedCmd(a))
	root.AddCommand(newAgentVersionCmd(a))
	return root
}

// helpText reproduces the bash cmd_help block: configured-host header,
// Setup/Control/Inspect/Allowlist/Advanced sections, Files: footer. We use
// a literal template (no Cobra flag/section auto-rendering) so the output
// is byte-identical to the bash version regardless of Cobra's defaults.
func helpText(a *app.App) string {
	host, _ := a.Cfg.ReadHost()
	if host == "" {
		host = "<not configured>"
	}
	return fmt.Sprintf(`%[1]s — dynamic SSH port forwarding from a remote dev box to this Mac
        (configured box: %[2]s)

Usage: %[1]s <command>

  Setup
    install [host]  Configure the dev box (asks if not given) and install as a
                    login agent (auto-start + self-heal), then start it.
    uninstall       Stop, remove the agent, and tear down the ssh master.
    reload          Re-apply config/plist changes (after editing this script).
    host [newhost]  Show the configured dev box, or switch to a new one.

  Control
    start           Start (load) the forwarding service.
    stop            Stop forwarding now (unload service + drop all forwards).
    restart         Force-restart the running service.

  Inspect
    status          Show box, service state, ssh master, active forwards. (default)
    ports           List the loopback dev ports currently listening on the box.
    logs [-f|N]     Show recent log lines; -f to follow, N for last N lines.

  Allowlist (forward ports the auto-filter would otherwise skip)
    allow <port>... Force-forward a port even if it's in the ephemeral range
                    (32768-60999) or denylist — e.g. a real service on 40085.
    unallow <port>. Remove ports from the allowlist.
    allowed         Show the current allowlist.

  Advanced
    run             Run the forwarding loop in the foreground (used by launchd).
    once            Do a single reconcile pass, then print status.
    help            Show this help.

Requirements: a Linux dev box reachable over passwordless (key-based) SSH.
Tuning (poll interval, denylist, skipped local ports) lives as plain
constants in internal/app/paths.go — edit there, rebuild, then `+"`%[1]s reload`"+`.
Files: host=%[3]s  allowlist=%[4]s  log=%[5]s
`, app.Tool, host, a.Paths.HostFile, a.Paths.AllowFile, a.Paths.Log)
}

func main() {
	a, err := app.NewProd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root := newRootCmd(a)
	if err := root.Execute(); err != nil {
		// Detect Cobra's auto-generated "unknown command" error and treat
		// it as a usage error (exit 2), matching bash's `*) ... exit 2`.
		if strings.HasPrefix(err.Error(), "unknown command") {
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprint(os.Stderr, helpText(a))
			os.Exit(2)
		}
		// usageErr → exit 2; print the message (if any).
		var ue usageErr
		if errors.As(err, &ue) {
			if ue.msg != "" {
				fmt.Fprintln(os.Stderr, ue.msg)
			}
			os.Exit(2)
		}
		// errSilent → exit 1 without re-printing (the command already did).
		if errors.Is(err, errSilent) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
