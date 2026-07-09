package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

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
			if dash < 0 && len(args) > 0 {
				return usageErr{msg: "usage: portal exec -- <cmd...>"}
			}

			forceTTY, _ := cmd.Flags().GetBool("tty")
			noTTY, _ := cmd.Flags().GetBool("no-tty")
			isTerminal := execIsTerminal
			makeRaw := execMakeRaw
			getSize := execGetSize
			watchWinch := execWatchWinch

			pty := false
			switch {
			case noTTY:
			case forceTTY:
				if !isTerminal(syscall.Stdin) || !isTerminal(syscall.Stdout) {
					return fmt.Errorf("cannot allocate tty: stdin/stdout is not a terminal")
				}
				pty = true
			default:
				pty = len(argv) == 0 && isTerminal(syscall.Stdin) && isTerminal(syscall.Stdout)
			}
			if !pty && len(argv) == 0 {
				return usageErr{msg: "usage: portal exec -- <cmd...>"}
			}

			lc := client.New(a.Paths.APISock)
			if pty {
				stdinFD := int(os.Stdin.Fd())
				restore, err := makeRaw(stdinFD)
				if err != nil {
					return err
				}
				restoreOnce, stopSignalRestore := execRawRestoreWithSignals(restore)
				defer func() {
					stopSignalRestore()
					_ = restoreOnce()
				}()

				rows, cols, err := getSize(stdinFD)
				if err != nil {
					rows, cols = 0, 0
				}
				winch := make(chan [2]uint16, 1)
				winchCtx, stopWinch := context.WithCancel(cmd.Context())
				defer stopWinch()
				go forwardWinch(watchWinch(winchCtx), getSize, stdinFD, winch)

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

// forwardWinch reads terminal-size ticks and forwards the LATEST size to out,
// coalescing bursts so a full buffer never strands the newest size. The winch
// channel is size 1: a plain non-blocking send drops the NEW value when the
// buffer is full, leaving the remote pty a generation behind after a burst;
// here a full buffer is drained of its stale size and the current one enqueued
// instead. This goroutine is the sole producer, so the final send always lands.
func forwardWinch(ticks <-chan struct{}, getSize func(int) (uint16, uint16, error), fd int, out chan [2]uint16) {
	for range ticks {
		r, c, err := getSize(fd)
		if err != nil {
			continue
		}
		sz := [2]uint16{r, c}
		select {
		case out <- sz:
		default:
			select {
			case <-out:
			default:
			}
			select {
			case out <- sz:
			default:
			}
		}
	}
}

func execRawRestoreWithSignals(restore func() error) (func() error, func()) {
	var once sync.Once
	var restoreErr error
	restoreOnce := func() error {
		once.Do(func() { restoreErr = restore() })
		return restoreErr
	}

	sigc := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigc, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigc:
			// Raw mode clears ISIG, so keyboard Ctrl-C is sent to the remote;
			// this path restores only for externally delivered process signals.
			_ = restoreOnce()
			signal.Reset(sig)
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(os.Getpid(), s)
			}
		case <-done:
		}
	}()

	stop := func() {
		signal.Stop(sigc)
		close(done)
	}
	return restoreOnce, stop
}

func printExecError(cmd *cobra.Command, err error) {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "portal exec: %s: %s\n", apiErr.Code, apiErr.Message)
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "portal exec: %v\n", err)
}
