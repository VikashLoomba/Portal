package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localclient"
)

func newAllowCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "allow <port>...",
		Short: "Force-forward a port even if it's in the ephemeral range or denylist",
		// No Args validator: bash exits 2 with a "usage:" line on no args, so
		// we surface that ourselves rather than letting Cobra print its
		// generic "Error: requires at least 1 arg(s)" + exit 1.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAllow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), a, args)
		},
	}
}

// runAllow writes the requested ports to the local allow file (always, so the
// daemon-down path still converges via the safety reconcile) and then pushes
// each newly-added port to the running daemon over the API. When every push is
// acked the "~100ms" claim is truthful; when the daemon is down we say so
// honestly. Output is byte-identical to today apart from that latency clause.
func runAllow(ctx context.Context, w, errw io.Writer, a *app.App, args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(errw, "usage: %s allow <port>...\n", app.Tool)
		return usageErr{msg: ""}
	}
	ports, bad := parsePorts(args)
	for _, b := range bad {
		fmt.Fprintf(errw, "skipping non-numeric port: %s\n", b)
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
			fmt.Fprintf(w, "already allowed: %d\n", p)
		}
	}
	if len(added) > 0 {
		fmt.Fprintf(w, "allowed:")
		for _, p := range added {
			fmt.Fprintf(w, " %d", p)
		}
		// Push each newly-added port to the daemon. A single failed PUT means
		// the daemon is down, so the ~100ms claim would be a lie — fall back to
		// the honest reconcile message.
		lc := localclient.New(a.Paths.APISock)
		up := true
		for _, p := range added {
			if _, e := lc.Allow(ctx, p); e != nil {
				up = false
				break
			}
		}
		if up {
			fmt.Fprintf(w, " (takes effect within ~100ms)\n")
		} else {
			fmt.Fprintf(w, " (takes effect when the daemon reconciles)\n")
		}
	}
	return nil
}

func newUnallowCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "unallow <port>...",
		Short: "Remove ports from the allowlist",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnallow(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), a, args)
		},
	}
}

// runUnallow removes the requested ports from the local allow file and pushes
// the deletion to the running daemon. A single successful DELETE reconverges the
// full list even for a multi-port unallow (the daemon re-pushes the whole
// allowlist), so `up` need only reflect that the daemon acked. When all args are
// non-numeric there is nothing to DELETE, so we probe Available for `up`.
func runUnallow(ctx context.Context, w, errw io.Writer, a *app.App, args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(errw, "usage: %s unallow <port>...\n", app.Tool)
		return usageErr{msg: ""}
	}
	// Bash short-circuit: if no allow file exists, print "allowlist is empty"
	// and return without creating it. Matches portal:611.
	if _, err := os.Stat(a.Cfg.AllowFilePath()); os.IsNotExist(err) {
		fmt.Fprintln(w, "allowlist is empty")
		return nil
	}
	// Bash does no numeric validation here — it just runs grep -vxF. Pass
	// strings through; numeric ones drop from the file, others no-op. We still
	// print the per-arg "unallowed:" line for parity.
	ports, _ := parsePorts(args)
	if err := a.Cfg.Unallow(ports); err != nil {
		return err
	}
	lc := localclient.New(a.Paths.APISock)
	up := true
	if len(ports) > 0 {
		for _, p := range ports {
			if _, e := lc.Unallow(ctx, p); e != nil {
				up = false
				break
			}
		}
	} else {
		up = lc.Available(ctx)
	}
	suffix := " (forward drops within ~100ms if in ephemeral range)"
	if !up {
		suffix = " (forward drops when the daemon reconciles)"
	}
	for _, raw := range args {
		fmt.Fprintf(w, "unallowed: %s%s\n", raw, suffix)
	}
	return nil
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

// parsePorts splits args into valid TCP ports and rejects. The valid range is
// 1..65535 — identical to localapi.parsePort (§4.5), so a port the daemon would
// reject with 400 invalid_port never gets written to the allow file nor PUT to
// the daemon (which would misreport an up daemon as "reconciles" / down).
func parsePorts(args []string) (ports []int, bad []string) {
	for _, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 || n > 65535 {
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
