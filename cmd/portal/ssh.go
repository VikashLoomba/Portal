package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clip"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipupload"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
)

// ctrlV is the byte produced by Ctrl+V (ASCII SYN, 0x16).
const ctrlV = 0x16

func newSSHCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <host> [ssh-args...]",
		Short: "SSH to a host with clipboard-image paste (Ctrl+V uploads & inserts the path)",
		Long: `Opens an interactive SSH session, proxied through a PTY so portal can
intercept Ctrl+V. When you press Ctrl+V and your Mac clipboard holds an
image, portal uploads it to ~/.cache/portal/clip/ on the remote (over the
SAME ssh connection) and types the resulting path at your cursor — so
screenshots paste straight into a coding agent running on the dev box.

If the clipboard has no image, Ctrl+V passes through unchanged.

Everything after the host is forwarded to ssh verbatim, so this works as a
drop-in replacement for ssh (aliases from ~/.ssh/config, -L forwards, remote
commands, etc.).`,
		DisableFlagParsing: true, // pass all flags straight to ssh
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErr{msg: "usage: portal ssh <host> [ssh-args...]"}
			}
			return runSSHProxy(cmd.Context(), a, args)
		},
	}
}

func runSSHProxy(ctx context.Context, a *app.App, args []string) error {
	// Per-session ControlMaster socket. The interactive session becomes the
	// master (ControlMaster=auto); the clipboard upload multiplexes over it
	// via `ssh -S <sock>`. ControlPersist=no tears it down when we exit.
	host := args[0]
	rest := args[1:]
	sessionSock := filepath.Join(os.TempDir(),
		fmt.Sprintf("portal-ssh-%d.sock", os.Getpid()))
	defer os.Remove(sessionSock)

	sshArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + sessionSock,
		"-o", "ControlPersist=no",
	}
	sshArgs = append(sshArgs, args...)

	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs...)

	// Start ssh attached to a new PTY.
	ptmx, err := pty.Start(sshCmd)
	if err != nil {
		return fmt.Errorf("start ssh in pty: %w", err)
	}
	defer ptmx.Close()

	// Propagate window size: once now, then on every SIGWINCH.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	winch <- syscall.SIGWINCH // initial sizing

	// Put the local terminal in raw mode so keystrokes (incl. Ctrl+V) reach
	// us byte-for-byte instead of being cooked by the line discipline.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	// Upload transport multiplexes over the session master once it's up.
	uploadT := sshctl.New(sessionSock, host, app.SSHOpts, a.Runner)
	cb := clip.New()

	// PTY → stdout (remote output to the screen).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(os.Stdout, ptmx)
	}()

	// stdin → PTY, intercepting Ctrl+V. Runs in its own goroutine; when ssh
	// exits, ptmx closes and the io.Copy above returns, ending the session.
	go proxyStdin(ctx, os.Stdin, ptmx, cb, uploadT, host, rest)

	// Wait for ssh to exit, then for the output pump to drain.
	err = sshCmd.Wait()
	ptmx.Close()
	wg.Wait()

	// Surface ssh's exit code (but a clean exit is nil).
	if exitErr, ok := err.(*exec.ExitError); ok {
		os.Exit(exitErr.ExitCode())
	}
	return err
}

// proxyStdin copies stdin → ptmx, intercepting Ctrl+V. On Ctrl+V with an
// image on the clipboard, it swallows the keystroke, uploads the image, and
// writes the remote path into the PTY as if typed. Otherwise Ctrl+V passes
// through. This goroutine is intentionally not awaited — os.Stdin.Read
// can't be unblocked, so we let it die with the process.
func proxyStdin(ctx context.Context, in io.Reader, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, host string, _ []string) {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			writeWithPaste(ctx, buf[:n], ptmx, cb, t)
		}
		if err != nil {
			return
		}
	}
}

// writeWithPaste forwards chunk to ptmx, but each Ctrl+V byte is handled
// specially: if the clipboard holds an image, the byte is dropped and the
// uploaded remote path is injected; otherwise the byte is forwarded.
func writeWithPaste(ctx context.Context, chunk []byte, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport) {
	start := 0
	for i := 0; i < len(chunk); i++ {
		if chunk[i] != ctrlV {
			continue
		}
		// Forward everything before the Ctrl+V.
		if i > start {
			_, _ = ptmx.Write(chunk[start:i])
		}
		start = i + 1

		if !cb.HasImage() {
			// No image — pass the Ctrl+V through unchanged.
			_, _ = ptmx.Write([]byte{ctrlV})
			continue
		}
		handlePaste(ctx, ptmx, cb, t)
	}
	if start < len(chunk) {
		_, _ = ptmx.Write(chunk[start:])
	}
}

// handlePaste extracts the clipboard image, uploads it, and injects the
// remote path into the PTY. On failure it emits a terminal bell so the user
// knows the paste didn't take.
//
// We deliberately show NO progress spinner: the only surfaces available are
// the PTY input (which the remote process would read as typed bytes) and
// stdout (which would corrupt a raw-mode TUI's screen). A screenshot uploads
// in well under a second over a normal connection; the 30s context is only a
// ceiling for a pathological link. The bell on failure is the one safe signal.
func handlePaste(ctx context.Context, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport) {
	png, err := cb.ImagePNG()
	if err != nil {
		bell(ptmx)
		return
	}
	upCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remotePath, err := clipupload.Upload(upCtx, t, png)
	if err != nil {
		bell(ptmx)
		return
	}
	// Inject the bare path at the cursor, as if typed.
	_, _ = ptmx.Write([]byte(remotePath))
}

func bell(w io.Writer) { _, _ = w.Write([]byte{0x07}) }
