package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/termx"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/client"
)

var (
	execIsTerminal = termx.IsTerminal
	execMakeRaw    = termx.MakeRaw
	execGetSize    = termx.GetSize
	execWatchWinch = termx.WatchWinch
)

func newExecCmd(a *app.App) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "exec [flags] -- <cmd...>",
		Short:         "Run a command through the portal daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			var argv []string
			if dash >= 0 {
				argv = args[dash:]
			}

			forceTTY, _ := cmd.Flags().GetBool("tty")
			noTTY, _ := cmd.Flags().GetBool("no-tty")
			stdinFD := int(os.Stdin.Fd())
			stdoutFD := int(os.Stdout.Fd())
			isTerminal := execIsTerminal
			makeRaw := execMakeRaw
			getSize := execGetSize
			watchWinch := execWatchWinch

			pty := false
			switch {
			case noTTY:
			case forceTTY:
				if !isTerminal(stdinFD) || !isTerminal(stdoutFD) {
					return fmt.Errorf("cannot allocate tty: stdin/stdout is not a terminal")
				}
				pty = true
			default:
				pty = len(argv) == 0 && isTerminal(stdinFD) && isTerminal(stdoutFD)
			}
			if !pty && len(argv) == 0 {
				return usageErr{msg: "usage: portal exec -- <cmd...>"}
			}

			lc := client.New(a.Paths.APISock)
			if pty {
				restore, err := makeRaw(stdinFD)
				if err != nil {
					return err
				}
				defer func() { _ = restore() }()

				rows, cols, err := getSize(stdinFD)
				if err != nil {
					rows, cols = 0, 0
				}
				winch := make(chan [2]uint16, 1)
				winchCtx, stopWinch := context.WithCancel(cmd.Context())
				defer stopWinch()
				go func() {
					for range watchWinch(winchCtx) {
						r, c, err := getSize(stdinFD)
						if err != nil {
							continue
						}
						select {
						case winch <- [2]uint16{r, c}:
						default:
						}
					}
				}()

				term := os.Getenv("TERM")
				if term == "" {
					term = "xterm-256color"
				}
				code, err := lc.ExecWithOptions(cmd.Context(), argv, os.Stdin, os.Stdout, os.Stderr, client.ExecOptions{
					PTY:   true,
					Term:  term,
					Rows:  rows,
					Cols:  cols,
					Winch: winch,
				})
				if err != nil {
					printExecError(cmd, err)
					return errSilent
				}
				if code != 0 {
					return exitCodeErr{code: code}
				}
				return nil
			}

			code, err := lc.Exec(cmd.Context(), argv, os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				printExecError(cmd, err)
				return errSilent
			}
			if code != 0 {
				return exitCodeErr{code: code}
			}
			return nil
		},
	}
	cmd.Flags().BoolP("tty", "t", false, "allocate a pseudo-terminal")
	cmd.Flags().BoolP("no-tty", "T", false, "disable pseudo-terminal allocation")
	return cmd
}

func printExecError(cmd *cobra.Command, err error) {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "portal exec: %s: %s\n", apiErr.Code, apiErr.Message)
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "portal exec: %v\n", err)
}
