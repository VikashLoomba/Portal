package main

import (
	"context"
	"fmt"
	"io"
	"log"
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

// dbg logs to the file named by PORTAL_SSH_DEBUG (if set). We cannot log to
// stdout/stderr during a session — those are the PTY surfaces and would
// corrupt the remote TUI. Returns a no-op logger when unset.
func newDebugLogger() *log.Logger {
	path := os.Getenv("PORTAL_SSH_DEBUG")
	if path == "" {
		return log.New(io.Discard, "", 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.New(f, "portal-ssh ", log.LstdFlags|log.Lmicroseconds)
}

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
	host := args[0]

	// Choose the ControlMaster socket. When the session targets the
	// configured dev box, reuse the daemon's master socket so the session
	// and the clipboard upload share ONE connection to the box (and attach
	// to the already-warm master if the daemon has one up). For any other
	// host there is no daemon master to share, so use a session-local
	// socket that is torn down on exit.
	//
	// ControlPath is keyed purely by file path, not by destination — so it
	// is essential to only point at the daemon socket when the host
	// actually matches, otherwise ssh would silently multiplex the session
	// onto the wrong box.
	configuredHost, _ := a.Cfg.ReadHost()
	var ctlSock string
	var persist string
	if configuredHost != "" && host == configuredHost {
		ctlSock = a.Paths.Sock
		// Leave the master up for the daemon to keep using after we exit.
		persist = "yes"
	} else {
		ctlSock = filepath.Join(os.TempDir(),
			fmt.Sprintf("portal-ssh-%d.sock", os.Getpid()))
		persist = "no"
		defer os.Remove(ctlSock)
	}

	sshArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + ctlSock,
		"-o", "ControlPersist=" + persist,
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

	// Upload transport multiplexes over the same master the session uses.
	uploadT := sshctl.New(ctlSock, host, app.SSHOpts, a.Runner)
	cb := clip.New()
	dbg := newDebugLogger()
	dbg.Printf("session start: host=%s ctlSock=%s", host, ctlSock)

	// PTY → stdout (remote output to the screen).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(os.Stdout, ptmx)
	}()

	// stdin → PTY, intercepting Ctrl+V. Runs in its own goroutine; when ssh
	// exits, ptmx closes and the io.Copy above returns, ending the session.
	go proxyStdin(ctx, os.Stdin, ptmx, cb, uploadT, dbg)

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
func proxyStdin(ctx context.Context, in io.Reader, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, dbg *log.Logger) {
	trace := os.Getenv("PORTAL_SSH_TRACE") != ""
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if trace {
				dbg.Printf("stdin %d byte(s): % x", n, buf[:n])
			}
			writeWithPaste(ctx, buf[:n], ptmx, cb, t, dbg)
		}
		if err != nil {
			return
		}
	}
}

// writeWithPaste forwards chunk to ptmx, but each Ctrl+V byte is handled
// specially: if the clipboard holds an image, the byte is dropped and the
// uploaded remote path is injected; otherwise the byte is forwarded.
func writeWithPaste(ctx context.Context, chunk []byte, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, dbg *log.Logger) {
	start := 0
	for i := 0; i < len(chunk); i++ {
		if chunk[i] != ctrlV {
			continue
		}
		dbg.Printf("Ctrl+V detected in input chunk (offset %d of %d bytes)", i, len(chunk))
		// Forward everything before the Ctrl+V.
		if i > start {
			_, _ = ptmx.Write(chunk[start:i])
		}
		start = i + 1

		if !cb.HasImage() {
			dbg.Printf("no image on clipboard; passing Ctrl+V through")
			// No image — pass the Ctrl+V through unchanged.
			_, _ = ptmx.Write([]byte{ctrlV})
			continue
		}
		dbg.Printf("image detected; handling paste")
		handlePaste(ctx, ptmx, cb, t, dbg)
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
func handlePaste(ctx context.Context, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, dbg *log.Logger) {
	png, err := cb.ImagePNG()
	if err != nil {
		dbg.Printf("ImagePNG failed: %v", err)
		bell(ptmx)
		return
	}
	dbg.Printf("extracted %d-byte PNG; uploading", len(png))
	upCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remotePath, err := clipupload.Upload(upCtx, t, png)
	if err != nil {
		dbg.Printf("upload failed: %v", err)
		bell(ptmx)
		return
	}
	dbg.Printf("uploaded -> %s; injecting path", remotePath)
	// Inject the bare path at the cursor, as if typed.
	_, _ = ptmx.Write([]byte(remotePath))
}

func bell(w io.Writer) { _, _ = w.Write([]byte{0x07}) }
