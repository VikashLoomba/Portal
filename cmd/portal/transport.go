package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/sshnative"
)

// validateNativeHost reports whether host RESOLVES via ssh_config to a native
// target. It is a package-level seam so tests stay hermetic (overriding it)
// without execing real `ssh -G`; in production it execs `ssh -G <host>` once at
// selection time (a cheap local call bounded by the resolver's 5s timeout).
var validateNativeHost = func(host string) error {
	return sshnative.ValidTarget(context.Background(), host, sshnative.DefaultConfigResolver())
}

// newTransportCmd implements `portal transport [name]` (T8). With no argument it
// prints the ACTIVE transport's Describe().Impl UNCONDITIONALLY — the canonical
// way to see which transport is live. With `system`/`native` it persists the
// selection via SetTransport and notes that a daemon restart is required for the
// change to take effect (mirroring host-switch semantics). An invalid name is a
// usage error.
func newTransportCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "transport [system|native]",
		Short: "Show or set the active transport (system ssh or native x/crypto ssh)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTransport(cmd.OutOrStdout(), a, args)
		},
	}
}

// runTransport is newTransportCmd.RunE's body, extracted so tests drive it with
// a buffer. The no-arg form reads the LIVE transport (a.Transport.Describe), not
// the config file, so it reflects what the running App is actually using.
func runTransport(w io.Writer, a *app.App, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(w, a.Transport.Describe().Impl)
		return nil
	}
	name := args[0]
	// Reject `transport native` unless the configured host RESOLVES via ssh_config
	// to a non-empty HostName. Native resolves aliases through `ssh -G`, so an
	// alias that resolves is a valid target; but an unresolvable/empty host would
	// build a client that fails at App construction and brick EVERY subsequent
	// command (including this revert). Fail here, before persisting, pointing at
	// the fix.
	if name == "native" {
		host, _ := a.Cfg.ReadHost()
		if err := validateNativeHost(host); err != nil {
			return usageErr{msg: fmt.Sprintf(
				"cannot select native transport: configured host %q is not a native target (%v); "+
					"native requires a host that resolves via ssh_config — set one with `%s host <user@host>` first",
				host, err, app.Tool)}
		}
	}
	if err := a.Cfg.SetTransport(name); err != nil {
		return usageErr{msg: err.Error()}
	}
	fmt.Fprintf(w, "transport set to %s\n", name)
	fmt.Fprintf(w, "restart the daemon for this to take effect: %s restart\n", app.Tool)
	return nil
}
