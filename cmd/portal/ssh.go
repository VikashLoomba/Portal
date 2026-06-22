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

// ctrlV is the legacy byte produced by Ctrl+V (ASCII SYN, 0x16).
const ctrlV = 0x16

// ctrlVTokens are every byte sequence a terminal may send for Ctrl+V.
// Most coding-agent TUIs enable an enhanced keyboard protocol (Kitty's, or
// xterm's modifyOtherKeys), which changes Ctrl+V from the single byte 0x16
// into a CSI escape sequence. We must match all of them or the keystroke
// passes straight through to the remote app (which is exactly the "nothing
// happens inside the agent, works at a bare shell" symptom).
//
//	0x16                — legacy control byte (bare shells)
//	ESC [ 118 ; 5 u     — Kitty keyboard protocol ('v'=118, mod 5 = ctrl)
//	ESC [ 27 ; 5 ; 118 ~ — xterm modifyOtherKeys form
var ctrlVTokens = [][]byte{
	{ctrlV},
	[]byte("\x1b[118;5u"),
	[]byte("\x1b[27;5;118~"),
}

// matchCtrlV reports the length of a Ctrl+V token at chunk[i], or 0 if none.
func matchCtrlV(chunk []byte, i int) int {
	for _, tok := range ctrlVTokens {
		if i+len(tok) <= len(chunk) && bytesEqual(chunk[i:i+len(tok)], tok) {
			return len(tok)
		}
	}
	return 0
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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
commands, etc.).

By default portal strips remote OSC 52 clipboard-WRITE sequences from the
session output. Multiplexers like zellij emit these to overwrite your local
clipboard, which would clobber a copied image before you paste it. Set
PORTAL_SSH_ALLOW_OSC52=1 to let remote apps write your clipboard instead.

Env vars: PORTAL_SSH_DEBUG=<file> logs interception events; PORTAL_SSH_TRACE=1
additionally logs every stdin byte; PORTAL_SSH_ALLOW_OSC52=1 disables the
OSC 52 clipboard-write filter described above.`,
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

	// PTY → stdout (remote output to the screen). We pass it through an
	// OSC 52 filter: remote apps (notably zellij's clipboard integration)
	// emit OSC 52 to WRITE the local Mac clipboard, which clobbers a copied
	// image with text and breaks Ctrl+V image paste. The filter logs and
	// (by default) strips those clipboard-write sequences so the image the
	// user copied survives until they paste it.
	stripOSC52 := os.Getenv("PORTAL_SSH_ALLOW_OSC52") == "" // default: strip
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		copyFilteringOSC52(os.Stdout, ptmx, stripOSC52, dbg)
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
	var carry []byte // bytes held back because they may be a partial Ctrl+V token
	for {
		n, err := in.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			if trace {
				dbg.Printf("stdin %d byte(s) (+%d carried): % x", n, len(carry), data)
			}
			// If the data ends with a prefix of a Ctrl+V escape token, hold
			// that tail back for the next read so a token split across two
			// reads is still matched intact.
			keep := len(data) - trailingCtrlVPrefixLen(data)
			carry = append(carry[:0:0], data[keep:]...)
			writeWithPaste(ctx, data[:keep], ptmx, cb, t, dbg)
		}
		if err != nil {
			// Flush any held-back bytes before exiting.
			if len(carry) > 0 {
				_, _ = ptmx.Write(carry)
			}
			return
		}
	}
}

// trailingCtrlVPrefixLen returns the length of the longest suffix of data
// that is a strict prefix of some Ctrl+V token — i.e. bytes that might be
// the start of a token whose remainder arrives in the next read. Returns 0
// when the tail can be processed immediately.
func trailingCtrlVPrefixLen(data []byte) int {
	for _, tok := range ctrlVTokens {
		// Only escape sequences (len>1) can be split; the single 0x16 byte
		// is always complete on its own.
		if len(tok) <= 1 {
			continue
		}
		max := len(tok) - 1
		if max > len(data) {
			max = len(data)
		}
		for l := max; l >= 1; l-- {
			if bytesEqual(data[len(data)-l:], tok[:l]) {
				return l
			}
		}
	}
	return 0
}

// writeWithPaste forwards chunk to ptmx, but each Ctrl+V token (in any of
// its terminal encodings) is handled specially: if the clipboard holds an
// image, the token is dropped and the uploaded remote path is injected;
// otherwise the original token is forwarded unchanged.
func writeWithPaste(ctx context.Context, chunk []byte, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, dbg *log.Logger) {
	start := 0
	for i := 0; i < len(chunk); {
		tokLen := matchCtrlV(chunk, i)
		if tokLen == 0 {
			i++
			continue
		}
		token := chunk[i : i+tokLen]
		dbg.Printf("Ctrl+V detected (% x) at offset %d of %d bytes", token, i, len(chunk))
		// Forward everything before the token.
		if i > start {
			_, _ = ptmx.Write(chunk[start:i])
		}
		i += tokLen
		start = i

		if !cb.HasImage() {
			dbg.Printf("no image on clipboard; passing Ctrl+V through")
			// No image — pass the ORIGINAL token through unchanged so the
			// remote app still sees a real Ctrl+V.
			_, _ = ptmx.Write(token)
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

// osc52Prefix is the start of an OSC 52 clipboard sequence: ESC ] 52 ;
var osc52Prefix = []byte("\x1b]52;")

// copyFilteringOSC52 copies src→dst, optionally removing OSC 52
// clipboard-write sequences (which remote apps like zellij use to overwrite
// the LOCAL clipboard — clobbering a copied image and breaking Ctrl+V image
// paste). When strip is false it only logs them. A sequence straddling two
// reads is held in carry until complete.
func copyFilteringOSC52(dst io.Writer, src io.Reader, strip bool, dbg *log.Logger) {
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := src.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			out, held := filterOSC52(data, strip, dbg)
			_, _ = dst.Write(out)
			carry = append(carry[:0:0], held...)
		}
		if err != nil {
			if len(carry) > 0 {
				_, _ = dst.Write(carry)
			}
			return
		}
	}
}

// filterOSC52 returns the bytes to forward and any trailing bytes to hold
// back (an OSC 52 sequence that hasn't terminated yet, or a prefix of the
// ESC]52; marker). When strip is true, complete OSC 52 sequences are dropped
// from the forwarded output; either way they are logged.
func filterOSC52(data []byte, strip bool, dbg *log.Logger) (out, carry []byte) {
	out = make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		rest := data[i:]
		idx := indexOf(rest, osc52Prefix)
		if idx < 0 {
			// No complete marker ahead. The tail might be a prefix of the
			// marker spanning into the next read — hold that much back.
			hold := trailingPrefixLen(rest, osc52Prefix)
			out = append(out, rest[:len(rest)-hold]...)
			return out, rest[len(rest)-hold:]
		}
		// Forward everything before the marker verbatim.
		out = append(out, rest[:idx]...)
		seqStart := i + idx
		end, complete := osc52End(data[seqStart:])
		if !complete {
			// Sequence not finished in this chunk — hold it for next read.
			return out, data[seqStart:]
		}
		seq := data[seqStart : seqStart+end]
		dbg.Printf("OSC52 clipboard-write seen (%d bytes); strip=%v", len(seq), strip)
		if !strip {
			out = append(out, seq...)
		}
		i = seqStart + end
	}
	return out, nil
}

// osc52End returns the length of the OSC 52 sequence starting at data[0]
// (including its terminator) and whether it is complete within data. The
// sequence terminates with BEL (0x07) or ST (ESC \ = 0x1b 0x5c).
func osc52End(data []byte) (int, bool) {
	for j := len(osc52Prefix); j < len(data); j++ {
		if data[j] == 0x07 {
			return j + 1, true
		}
		if data[j] == 0x1b && j+1 < len(data) && data[j+1] == 0x5c {
			return j + 2, true
		}
		if data[j] == 0x1b && j+1 >= len(data) {
			return 0, false // ESC at the very end — need the next byte
		}
	}
	return 0, false
}

// indexOf returns the index of sub in b, or -1.
func indexOf(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if bytesEqual(b[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// trailingPrefixLen returns the length of the longest suffix of b that is a
// strict prefix of sub (so a marker split across reads is held back).
func trailingPrefixLen(b, sub []byte) int {
	max := len(sub) - 1
	if max > len(b) {
		max = len(b)
	}
	for l := max; l >= 1; l-- {
		if bytesEqual(b[len(b)-l:], sub[:l]) {
			return l
		}
	}
	return 0
}
