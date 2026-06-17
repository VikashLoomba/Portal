package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vikashl/portal/internal/app"
	"github.com/vikashl/portal/internal/logfile"
)

func newStatusCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show box, service state, ssh master, active forwards (default)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.Context(), a)
		},
	}
}

func runStatus(ctx context.Context, a *app.App) error {
	host, _ := a.Cfg.ReadHost()
	if host == "" {
		fmt.Printf("dev box: <not configured>\n")
	} else {
		fmt.Printf("dev box: %s\n", host)
	}
	fmt.Printf("service (%s):\n", a.Paths.Label)

	st, _ := a.Service.Status(ctx)
	if !st.Loaded {
		fmt.Printf("  not loaded (run: %s install)\n", app.Tool)
	} else {
		for _, line := range st.StateLines {
			fmt.Println(line)
		}
	}
	fmt.Println()

	if host == "" {
		return nil
	}
	pid, _ := a.Transport.MasterPID(ctx)
	if pid == 0 {
		fmt.Printf("ssh master: DOWN (host=%s sock=%s)\n", host, a.Paths.Sock)
		return nil
	}
	fmt.Printf("ssh master: UP (pid=%d) host=%s\n", pid, host)
	fmt.Println("active forwards (local listeners owned by master):")
	// Bash: lsof | awk 'NR>1 {print "  " $9}' | sort -u — emits the verbatim
	// NAME column, so an IPv4+IPv6 dual-stack listener produces TWO lines.
	lines, _ := a.Ports.MasterForwardLines(ctx, pid)
	for _, ln := range lines {
		fmt.Printf("  %s\n", ln)
	}
	return nil
}

func newPortsCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:     "ports",
		Aliases: []string{"list-remote"},
		Short:   "List loopback dev ports currently listening on the dev box",
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _ := a.Cfg.ReadHost()
			if host == "" {
				return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
			}
			if pid, _, err := a.Transport.EnsureMaster(cmd.Context()); err != nil || pid == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "could not reach %s\n", host)
				return errSilent
			}
			allow, _ := a.Cfg.AllowedPorts()
			ports, err := a.Discover.DesiredPorts(cmd.Context(), app.DenyPorts, allow)
			if err != nil {
				return err
			}
			fmt.Printf("loopback dev ports listening on %s (will be forwarded):\n", host)
			for _, p := range ports {
				fmt.Printf("  %d\n", p)
			}
			return nil
		},
	}
}

func newLogsCmd(a *app.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs [-f|N]",
		Short: "Show recent log lines; -f to follow, N for last N lines",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := a.Paths.Log
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("no log yet at %s", path)
			}
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			switch arg {
			case "":
				out, err := logfile.Tail(path, 40)
				if err != nil {
					return err
				}
				fmt.Print(out)
				return nil
			case "-f", "--follow", "follow":
				return logfile.Follow(cmd.Context(), path, os.Stdout)
			default:
				n, err := strconv.Atoi(strings.TrimSpace(arg))
				if err != nil || n < 0 {
					// Bash falls back to `tail -n 40` only on Atoi failure
					// (`tail -n <bad>` returns non-zero). `tail -n 0` is
					// VALID and prints zero lines, so we honor that here.
					n = 40
				}
				if n == 0 {
					return nil
				}
				out, err := logfile.Tail(path, n)
				if err != nil {
					return err
				}
				fmt.Print(out)
				return nil
			}
		},
	}
	return cmd
}
