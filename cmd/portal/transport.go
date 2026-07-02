package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
)

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
	if err := a.Cfg.SetTransport(name); err != nil {
		return usageErr{msg: err.Error()}
	}
	fmt.Fprintf(w, "transport set to %s\n", name)
	fmt.Fprintf(w, "restart the daemon for this to take effect: %s restart\n", app.Tool)
	return nil
}
