package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
)

// newSSHCmd is a THIN PASSTHROUGH to the system ssh, kept only as an alias so
// muscle memory and existing scripts that call `portal ssh <host>` keep working
// after the PTY clipboard proxy was retired (DESIGN §8.2/§11).
//
// The old implementation wrapped ssh in a ~1271-line PTY proxy that intercepted
// the Ctrl+V keystroke and typed an uploaded remote path into the agent. That
// approach was fragile (per-terminal keystroke matching) and forced users
// through `portal ssh`. It is REPLACED by transparent clipboard-READ
// interception: deploy `xclip`/`wl-paste` shims on the dev box (see
// internal/setup) and serve the Mac clipboard over the existing
// daemon pipe. With that in place a coding agent reads the clipboard through
// plain `ssh <host>` — no special command, no PTY, no keystroke matching.
//
// So `portal ssh <args...>` now simply execs the real ssh with the args
// verbatim, replacing this process image (no extra PTY layer, no interception).
//
// DEPRECATION: prefer plain `ssh`. This alias exists for continuity only.
func newSSHCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <host> [ssh-args...]",
		Short: "Deprecated alias for plain `ssh` (clipboard paste now works over plain ssh)",
		Long: `Execs the system ssh with your args verbatim — a thin passthrough kept as
an alias for continuity.

DEPRECATED: prefer plain ` + "`ssh <host>`" + `. portal no longer wraps ssh in a
PTY to intercept Ctrl+V. Clipboard-image (and opt-in text) paste now works
transparently over plain ssh: the daemon deploys xclip/wl-paste read shims on
the dev box and serves your Mac clipboard over the existing portal connection,
so a coding agent's own Ctrl+V "just works" with no special command.

All arguments are forwarded to ssh unchanged.`,
		DisableFlagParsing: true, // pass all flags straight to ssh
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErr{msg: "usage: portal ssh <host> [ssh-args...]"}
			}
			return execSSH(args)
		},
	}
}

// execSSH replaces the current process with the system ssh, forwarding args
// verbatim. On success it does NOT return (the image is replaced). It falls
// back to a child-process exec only if syscall.Exec is unavailable for some
// reason, preserving ssh's exit code via exitCodeErr.
func execSSH(args []string) error {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found on PATH: %w", err)
	}
	// Replace this process image with ssh: simplest possible passthrough, no
	// PTY, no interception, ssh owns the terminal directly. argv[0] is "ssh".
	argv := append([]string{"ssh"}, args...)
	if err := syscall.Exec(sshPath, argv, os.Environ()); err != nil {
		// syscall.Exec only returns on failure; fall back to a child process so
		// `portal ssh` still works on the off chance Exec is unsupported.
		c := exec.Command(sshPath, args...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if runErr := c.Run(); runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				return exitCodeErr{code: exitErr.ExitCode()}
			}
			return runErr
		}
	}
	return nil
}

// exitCodeErr carries ssh's non-zero exit code out to main so it can mirror
// it. Retained from the old PTY proxy (which used it to run deferreds before
// exiting); the passthrough only reaches it on the syscall.Exec fallback path.
// main() recognizes it and calls os.Exit(code) with no extra output.
type exitCodeErr struct{ code int }

func (e exitCodeErr) Error() string { return fmt.Sprintf("ssh exited %d", e.code) }
