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

// Bracketed-paste markers. When a terminal in bracketed-paste mode delivers a
// paste it brackets the literal payload with ESC[200~ ... ESC[201~. The payload
// is data the user pasted, NOT keystrokes, so a 0x16 inside it is a literal SYN
// byte and must pass through VERBATIM — scanning it for Ctrl+V would corrupt the
// paste (F8). We track the region and forward it untouched.
var (
	bracketStart = []byte("\x1b[200~")
	bracketEnd   = []byte("\x1b[201~")
)

// ctrlV0x03 is Ctrl+C (ASCII ETX, 0x03). A 0x03 on stdin while a clipboard
// upload is in flight cancels that upload so the session is never wedged (F6c).
const ctrlC = 0x03

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
//
// SECURITY: with PORTAL_SSH_TRACE=1 the stdin path additionally writes EVERY
// raw stdin byte to this file (see proxyStdin) — that includes anything you
// type, such as passwords/passphrases entered at the remote, in cleartext. The
// file is opened 0600 but is NOT rotated or scrubbed; treat it as sensitive and
// only enable tracing when actively debugging, then delete the file.
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
additionally logs every stdin byte to that file (SENSITIVE: this captures
everything you type, including passwords, in cleartext to an unrotated file —
enable only while debugging, then delete it); PORTAL_SSH_ALLOW_OSC52=1 disables
the OSC 52 clipboard-write filter described above.`,
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

	// Handle SIGTERM/SIGHUP (NOT SIGINT — in raw mode Ctrl+C is just a byte
	// destined for the remote, so we must not trap it) by cancelling ctx; the
	// deferred term.Restore below then runs as the session unwinds, so the
	// terminal is never left in raw mode when we are killed (F-nit b).
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignals()

	// Choose the ControlMaster socket. When the session targets the
	// configured dev box AND a live daemon master is up to share, reuse the
	// daemon's master socket so the session and the clipboard upload share
	// ONE connection to the box. For any other host (or when there is no live
	// daemon master, or the args carry connection-affecting flags) we use a
	// session-local socket that is torn down on exit. chooseCtlSock is the
	// single source of truth for that decision (see its doc comment).
	configuredHost, _ := a.Cfg.ReadHost()
	ctlSock, persist, sessionLocal := chooseCtlSock(configuredHost, args, a.Paths.Sock,
		func() bool { return daemonMasterLive(ctx, a, configuredHost) })
	if sessionLocal {
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
	// closePTY is idempotent: it is called both from this defer (so the PTY
	// is released on every exit path) and explicitly after sshCmd.Wait below.
	// sync.Once keeps the second call a no-op (F-nit c — was a double Close).
	var ptmxOnce sync.Once
	closePTY := func() { ptmxOnce.Do(func() { _ = ptmx.Close() }) }
	defer closePTY()

	// Put the local terminal in raw mode so keystrokes (incl. Ctrl+V) reach
	// us byte-for-byte instead of being cooked by the line discipline. A
	// failure here is fatal: running cooked would mangle interception (the
	// line discipline eats Ctrl+V/escape encodings) and leave the user in a
	// half-broken session, so we require a real TTY rather than soldier on
	// silently (F-nit a). The deferred Restore is what F3 relies on — it must
	// run on EVERY exit path, including a non-zero ssh exit, so runSSHProxy
	// RETURNS the exit code (as exitCodeErr) instead of calling os.Exit, which
	// would skip all deferreds and strand the terminal in raw mode.
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("portal ssh requires an interactive terminal (stdin is not a TTY): %w", err)
	}
	defer term.Restore(fd, oldState)

	// Propagate window size: once now, then on every SIGWINCH. The goroutine
	// owns winchDone so it terminates on exit instead of leaking (F-nit c).
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	winchDone := make(chan struct{})
	defer close(winchDone)
	go func() {
		for {
			select {
			case <-winch:
				_ = pty.InheritSize(os.Stdin, ptmx)
			case <-winchDone:
				return
			}
		}
	}()
	winch <- syscall.SIGWINCH // initial sizing

	// Upload transport multiplexes over the same master the session uses.
	uploadT := sshctl.New(ctlSock, host, app.SSHOpts, a.Runner)
	cb := clip.New()
	dbg := newDebugLogger()
	dbg.Printf("session start: host=%s ctlSock=%s sessionLocal=%v", host, ctlSock, sessionLocal)

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
	// We always reach here with a raw-mode TTY (MakeRaw succeeded above or we
	// returned), so Ctrl+V / its escape encodings are treated as a paste
	// trigger. proxyStdin honors ctx, so a SIGTERM/SIGHUP cancellation also
	// unblocks any in-flight upload.
	go proxyStdin(ctx, os.Stdin, ptmx, cb, uploadT, dbg, true)

	// Wait for ssh to exit, then for the output pump to drain.
	err = sshCmd.Wait()
	closePTY()
	wg.Wait()

	// Surface ssh's exit code WITHOUT calling os.Exit here: returning an
	// exitCodeErr lets every deferred above run (term.Restore, socket removal,
	// goroutine teardown) before main() exits with the code (F3). A clean exit
	// is nil.
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitCodeErr{code: exitErr.ExitCode()}
	}
	return err
}

// chooseCtlSock is the single source of truth for which ControlMaster socket a
// `portal ssh` session uses. It is consumed by runSSHProxy and exercised
// directly by the unit tests (no duplicated logic to drift out of sync).
//
// The daemon's master socket is reused ONLY when the invocation is EXACTLY the
// bare configured host with NO connection-affecting flags, AND a live daemon
// master is actually up to join (masterLive). ssh multiplexing matches purely
// by ControlPath, so any per-invocation connection flag (-p / -l / -J / -o, a
// user@host form, a different port) would be silently ignored once we point at
// the daemon socket — the session and the clipboard upload could land on the
// daemon's box instead of the one the flags describe (F4). When in doubt we
// fall back to a session-local socket with ControlPersist=no: correct, just not
// shared. Requiring a live master (probed via masterLive) also prevents portal
// ssh from itself becoming a persistent orphan master under
// ControlMaster=auto + ControlPersist=yes when no daemon is running (F18).
//
// masterLive is invoked at most once and only when the args otherwise qualify,
// so the ssh -O check probe is skipped entirely for non-configured hosts.
func chooseCtlSock(configuredHost string, args []string, daemonSock string, masterLive func() bool) (sock, persist string, sessionLocal bool) {
	if argsAreBareConfiguredHost(configuredHost, args) && masterLive() {
		// Reuse the daemon master; leave it up for the daemon after we exit.
		return daemonSock, "yes", false
	}
	// Session-local socket, torn down on exit; never persist (so we don't
	// strand an orphan master of our own — F18).
	return filepath.Join(os.TempDir(),
		fmt.Sprintf("portal-ssh-%d.sock", os.Getpid())), "no", true
}

// argsAreBareConfiguredHost reports whether args is EXACTLY the configured host
// and nothing else — i.e. a single argument equal to configuredHost, with no
// extra ssh flags, no user@host override, no remote command. Anything else
// (a port flag, a jump host, an -o option, a trailing command, a different
// user) means the connection target/parameters differ from what the daemon
// master was built with, so the daemon socket must NOT be reused (F4).
func argsAreBareConfiguredHost(configuredHost string, args []string) bool {
	return configuredHost != "" && len(args) == 1 && args[0] == configuredHost
}

// daemonMasterLive probes the daemon's ControlMaster (ssh -O check) and reports
// whether a master is actually running on the daemon socket. Used by
// chooseCtlSock to avoid both multiplexing onto a dead socket and (F18)
// becoming an orphan persistent master when no daemon is up.
func daemonMasterLive(ctx context.Context, a *app.App, host string) bool {
	pid, _ := sshctl.New(a.Paths.Sock, host, app.SSHOpts, a.Runner).MasterPID(ctx)
	return pid != 0
}

// exitCodeErr carries ssh's non-zero exit code out of runSSHProxy so its
// deferreds (term.Restore, socket removal, goroutine teardown) all run before
// the process exits. main() recognizes it and calls os.Exit(code) AFTER
// Execute returns, printing nothing — mirroring how usageErr signals exit 2.
type exitCodeErr struct{ code int }

func (e exitCodeErr) Error() string { return fmt.Sprintf("ssh exited %d", e.code) }

// stdinIdleFlush bounds how long a partial-token carry may be held before we
// give up waiting for its completing bytes and forward it verbatim. A genuine
// multi-byte Ctrl+V (or bracketed-paste marker) split across two reads has its
// second half arrive essentially back-to-back, so this never fires mid-token;
// but a lone Escape keypress (byte 0x1b, which is a prefix of every escape
// token) would otherwise sit in carry until the NEXT keystroke, making Escape
// feel dead in vim/less/fzf (F5). The idle flush caps that latency.
const stdinIdleFlush = 20 * time.Millisecond

// proxyStdin copies stdin → ptmx, intercepting Ctrl+V (only when interactive —
// stdin is an interactive raw-mode TTY). On Ctrl+V with an image on the
// clipboard it swallows the keystroke, uploads the image, and writes the remote
// path into the PTY as if typed; otherwise Ctrl+V passes through. When NOT
// interactive (piped/redirected stdin) everything is forwarded verbatim so
// binary input with an image on the clipboard is never corrupted (F8).
//
// Concurrency (F6): reading stdin must never stall, so the clipboard probe and
// upload run on a separate, serialized worker goroutine; a single writer
// goroutine owns ptmx and drains an ordered queue so the bytes before a paste,
// the injected path, and the bytes after all reach the PTY in order; and a
// 0x03 (Ctrl+C) arriving while an upload is in flight cancels it so the session
// is never wedged. The reader is intentionally not awaited — os.Stdin.Read
// can't be unblocked, so it dies with the process; the writer/worker stop when
// done is closed (after ssh exits) so a late upload can't write a closed ptmx.
func proxyStdin(ctx context.Context, in io.Reader, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, dbg *log.Logger, interactive bool) {
	trace := os.Getenv("PORTAL_SSH_TRACE") != ""

	p := newPasteOrchestrator(ctx, ptmx, cb, t, dbg)
	defer p.stop()

	sc := &stdinScanner{interactive: interactive}

	// Read on a child goroutine so the main loop can also wake on the idle
	// timer (F5) — os.Stdin.Read itself can't be unblocked.
	type readResult struct {
		data []byte
		err  error
	}
	reads := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				reads <- readResult{data: append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				reads <- readResult{err: err}
				return
			}
		}
	}()

	var idle *time.Timer
	var idleC <-chan time.Time
	for {
		select {
		case r := <-reads:
			if len(r.data) > 0 {
				if trace {
					dbg.Printf("stdin %d byte(s) (+%d carried): % x", len(r.data), sc.carryLen(), r.data)
				}
				// A 0x03 cancels any in-flight upload immediately, regardless of
				// how the bytes get scanned below (F6c).
				if interactive && containsByte(r.data, ctrlC) {
					p.cancelInFlight()
				}
				for _, seg := range sc.scan(r.data) {
					p.enqueue(seg)
				}
				// (Re)arm the idle timer iff a partial token is being held.
				// Stop+drain before reset to avoid a stale fire (which could
				// flush a still-arriving token early and reintroduce the
				// split-token bug); disable idleC while not holding anything.
				idle = resetIdle(idle)
				if sc.carryLen() > 0 {
					if idle == nil {
						idle = time.NewTimer(stdinIdleFlush)
					} else {
						idle.Reset(stdinIdleFlush)
					}
					idleC = idle.C
				} else {
					idleC = nil
				}
			}
			if r.err != nil {
				// Flush any held-back bytes before exiting.
				if c := sc.flush(); len(c) > 0 {
					p.enqueue(pasteSegment{data: c})
				}
				return
			}
		case <-idleC:
			// No completing bytes arrived within the idle window — the held
			// carry is not a partial token after all (most often a lone
			// Escape). Forward it verbatim so latency stays bounded (F5).
			if c := sc.flush(); len(c) > 0 {
				p.enqueue(pasteSegment{data: c})
			}
			idleC = nil
		}
	}
}

// resetIdle stops t (if any) and drains a pending fire from its channel so a
// subsequent Reset can't observe a stale tick. It returns t unchanged (nil-safe)
// for caller convenience.
func resetIdle(t *time.Timer) *time.Timer {
	if t == nil {
		return nil
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	return t
}

// stdinScanner splits the keystroke stream into ordered segments to forward,
// keeping state ACROSS reads so a Ctrl+V token, a bracketed-paste marker, or a
// bracketed-paste region split across two reads is still handled correctly. A
// segment is either a verbatim byte run (data) or a paste trigger (paste).
type stdinScanner struct {
	interactive bool
	inPaste     bool   // inside a bracketed-paste region: forward verbatim
	carry       []byte // trailing bytes held back as a possible partial token/marker
}

// pasteSegment is one ordered unit handed to the writer: either literal bytes
// (data, forwarded verbatim) or a paste trigger (paste, which probes the
// clipboard and may inject an uploaded path).
type pasteSegment struct {
	data  []byte
	paste bool
}

func (s *stdinScanner) carryLen() int { return len(s.carry) }

// flush returns the held carry and clears it (used at EOF and on idle timeout).
func (s *stdinScanner) flush() []byte {
	c := s.carry
	s.carry = nil
	return c
}

// scan consumes one read's worth of bytes (prepended with any held carry) and
// returns the ordered segments to forward. Whatever trailing bytes might be the
// start of a token/marker whose remainder arrives next read are retained in
// s.carry rather than emitted.
func (s *stdinScanner) scan(in []byte) []pasteSegment {
	data := in
	if len(s.carry) > 0 {
		data = append(s.carry[:len(s.carry):len(s.carry)], in...)
		s.carry = nil
	}
	// Non-interactive (piped) stdin: never intercept; forward everything
	// verbatim so binary input is never corrupted (F8). No carry needed.
	if !s.interactive {
		if len(data) == 0 {
			return nil
		}
		return []pasteSegment{{data: data}}
	}

	var segs []pasteSegment
	start := 0
	emit := func(end int) {
		if end > start {
			segs = append(segs, pasteSegment{data: append([]byte(nil), data[start:end]...)})
		}
	}
	i := 0
	for i < len(data) {
		if s.inPaste {
			// Inside a bracketed paste: forward verbatim, watching only for the
			// end marker. Pass 0x16 etc. straight through (F8). A partial end
			// marker at the tail is held back by the trailing-prefix logic
			// below (s.trailingPrefix returns bracketEnd's prefix when inPaste).
			if matchAt(data, i, bracketEnd) {
				// The end marker forwards verbatim with the rest of the paste,
				// so leave start untouched; only exit paste mode here. The bytes
				// up to and including the marker are emitted by the trailing
				// emit (they are part of [start:len-hold]).
				i += len(bracketEnd)
				s.inPaste = false
				continue
			}
			i++
			continue
		}
		// A bracketed-paste start marker: enter paste mode. The marker and the
		// region forward verbatim, so we leave start untouched (they are part
		// of the verbatim run) and just skip past the marker.
		if matchAt(data, i, bracketStart) {
			i += len(bracketStart)
			s.inPaste = true
			continue
		}
		// A Ctrl+V token: emit preceding bytes, then a paste trigger; drop the
		// token (writeWithPaste forwards it unchanged if there's no image).
		if tl := matchCtrlV(data, i); tl > 0 {
			emit(i)
			segs = append(segs, pasteSegment{data: append([]byte(nil), data[i:i+tl]...), paste: true})
			i += tl
			start = i
			continue
		}
		i++
	}
	// Hold back a trailing partial token/marker for the next read.
	hold := s.trailingPrefix(data, start)
	emit(len(data) - hold)
	if hold > 0 {
		s.carry = append([]byte(nil), data[len(data)-hold:]...)
	}
	return segs
}

// trailingPrefix returns how many trailing bytes (no earlier than start) are a
// strict prefix of some token we may need to complete next read: a Ctrl+V
// escape token, or a bracketed-paste start/end marker (depending on whether we
// are inside a paste). A lone trailing 0x16 is complete on its own and never
// held; a lone trailing 0x1b IS held here (it could begin an escape token), but
// the idle flush bounds how long (F5).
func (s *stdinScanner) trailingPrefix(data []byte, start int) int {
	tail := data[start:]
	if s.inPaste {
		return trailingPrefixLen(tail, bracketEnd)
	}
	best := trailingPrefixLen(tail, bracketStart)
	if l := trailingCtrlVPrefixLen(tail); l > best {
		best = l
	}
	return best
}

// trailingCtrlVPrefixLen returns the length of the longest suffix of data that
// is a strict prefix of some MULTI-byte Ctrl+V escape token — i.e. bytes that
// might be the start of a token whose remainder arrives in the next read. The
// single 0x16 byte is always complete on its own, so it is never held. Returns
// 0 when the tail can be processed immediately.
func trailingCtrlVPrefixLen(data []byte) int {
	best := 0
	for _, tok := range ctrlVTokens {
		if len(tok) <= 1 {
			continue
		}
		if l := trailingPrefixLen(data, tok); l > best {
			best = l
		}
	}
	return best
}

// matchAt reports whether tok occurs at data[i] (and fits within data).
func matchAt(data []byte, i int, tok []byte) bool {
	return i+len(tok) <= len(data) && bytesEqual(data[i:i+len(tok)], tok)
}

// containsByte reports whether b appears anywhere in data.
func containsByte(data []byte, b byte) bool {
	for _, c := range data {
		if c == b {
			return true
		}
	}
	return false
}

// pasteOrchestrator owns ptmx writes and the upload worker. The reader hands it
// ordered segments via enqueue (never blocking on a probe/upload); a single
// writer goroutine drains them in order, running each paste trigger through the
// synchronous writeWithPaste so the injected path lands between the surrounding
// bytes (F6a, F6b). cancelInFlight cancels the upload context for a Ctrl+C
// (F6c); every ptmx write goes through write, which recovers from a write to an
// already-closed ptmx so a late upload completing after ssh exit cannot panic
// (F6d).
type pasteOrchestrator struct {
	ctx  context.Context
	ptmx io.Writer
	cb   clip.Clipboard
	t    sshctl.Transport
	dbg  *log.Logger

	queue *segmentQueue
	done  chan struct{}
	wg    sync.WaitGroup

	mu           sync.Mutex
	cancelUpload context.CancelFunc // cancel of the in-flight upload, or nil
}

func newPasteOrchestrator(ctx context.Context, ptmx io.Writer, cb clip.Clipboard, t sshctl.Transport, dbg *log.Logger) *pasteOrchestrator {
	p := &pasteOrchestrator{
		ctx:   ctx,
		ptmx:  ptmx,
		cb:    cb,
		t:     t,
		dbg:   dbg,
		queue: newSegmentQueue(),
		done:  make(chan struct{}),
	}
	p.wg.Add(1)
	go p.run()
	return p
}

// enqueue appends a segment for the writer to process in order. It never blocks
// on a clipboard probe or upload (those happen on the writer), so the reader
// keeps draining stdin (F6a).
func (p *pasteOrchestrator) enqueue(seg pasteSegment) { p.queue.push(seg) }

// run is the single ptmx writer. It drains the ordered queue; a paste segment
// is handled synchronously by writeWithPaste (which probes the clipboard and
// uploads), so bytes queued behind it wait until the path is injected — keeping
// output ordering (F6b). The probe+upload uses a cancelable context registered
// so a Ctrl+C can abort it (F6c). On stop the queue is drained to completion
// (so a clean EOF still flushes pending output) before the writer exits.
func (p *pasteOrchestrator) run() {
	defer p.wg.Done()
	for {
		seg, ok := p.queue.pop(p.done)
		if !ok {
			return // stopped and the queue is empty
		}
		if seg.paste {
			p.runPaste(seg.data)
			continue
		}
		p.write(seg.data)
	}
}

// runPaste handles a Ctrl+V trigger carrying the original token bytes: it
// probes the clipboard and, with an image, uploads and injects the path; with
// no image it forwards the ORIGINAL token unchanged (preserving whatever
// encoding the terminal sent). The upload context is registered for Ctrl+C
// cancellation (F6c).
func (p *pasteOrchestrator) runPaste(token []byte) {
	upCtx, cancel := context.WithCancel(p.ctx)
	p.mu.Lock()
	p.cancelUpload = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.cancelUpload = nil
		p.mu.Unlock()
		cancel()
	}()
	// writeWithPaste is synchronous and guards ptmx writes via p.write (through
	// the wrapper below) so a late completion after ptmx.Close() is dropped.
	writeWithPaste(upCtx, token, writerFunc(p.write), p.cb, p.t, p.dbg)
}

// cancelInFlight aborts an upload currently running on the writer, so a Ctrl+C
// pressed during a slow upload is not wedged behind it (F6c). No-op if idle.
func (p *pasteOrchestrator) cancelInFlight() {
	p.mu.Lock()
	cancel := p.cancelUpload
	p.mu.Unlock()
	if cancel != nil {
		p.dbg.Printf("Ctrl+C during upload; cancelling")
		cancel()
	}
}

// write forwards bytes to ptmx, recovering from a write to an already-closed
// ptmx (e.g. ssh has exited and runSSHProxy closed it) so a late upload
// completion can never panic the process (F6d). The returned error is ignored
// by callers, matching the original best-effort ptmx writes.
func (p *pasteOrchestrator) write(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	defer func() {
		if r := recover(); r != nil {
			p.dbg.Printf("recovered ptmx write after close: %v", r)
			n, err = 0, os.ErrClosed
		}
	}()
	return p.ptmx.Write(b)
}

// stop signals the writer to drain the queue and exit, then waits for it. It
// cancels any in-flight upload so a stuck probe/upload cannot keep the writer
// (and thus stop) blocked forever. Called from a defer in proxyStdin once stdin
// reaches EOF; a clean EOF still flushes everything already queued.
func (p *pasteOrchestrator) stop() {
	close(p.done)
	p.mu.Lock()
	cancel := p.cancelUpload
	p.mu.Unlock()
	if cancel != nil {
		cancel() // unblock a stuck upload so the writer can finish draining
	}
	p.wg.Wait()
}

// writerFunc adapts a write method to io.Writer so writeWithPaste (which takes
// an io.Writer) routes its ptmx writes through the orchestrator's guarded path.
type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }

// segmentQueue is an unbounded FIFO of pasteSegments. push never blocks the
// reader on the writer's progress (F6a): a slow upload only grows the queue
// (a human types few bytes during one), it doesn't stall stdin. pop blocks
// until an item is available, returning ok=false only once done is closed AND
// the queue has fully drained — so a clean EOF still flushes pending output.
type segmentQueue struct {
	mu    sync.Mutex
	items []pasteSegment
	notC  chan struct{} // 1-buffered signal that items may be available
}

func newSegmentQueue() *segmentQueue {
	return &segmentQueue{notC: make(chan struct{}, 1)}
}

func (q *segmentQueue) push(seg pasteSegment) {
	q.mu.Lock()
	q.items = append(q.items, seg)
	q.mu.Unlock()
	select {
	case q.notC <- struct{}{}:
	default:
	}
}

// pop returns the next item, blocking until one is available. Once done is
// closed it keeps returning any items still queued and only reports ok=false
// when the queue is empty, so output enqueued before EOF is never dropped.
func (q *segmentQueue) pop(done <-chan struct{}) (pasteSegment, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			seg := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return seg, true
		}
		q.mu.Unlock()
		select {
		case <-done:
			// Drain whatever remains, then stop. We re-check under the lock to
			// race-safely catch a push that happened just before done closed.
			q.mu.Lock()
			empty := len(q.items) == 0
			q.mu.Unlock()
			if empty {
				return pasteSegment{}, false
			}
		case <-q.notC:
		}
	}
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

// osc52MaxSeq caps how many bytes we will buffer for a single in-progress OSC
// 52 sequence (marker + payload + terminator) before giving up on it. A real
// clipboard write is tiny — even a long path or a small base64 blob is well
// under a kilobyte — so a few KB is generous. The cap exists purely so an
// adversarial (or buggy) remote that emits «ESC ] 52 ;» and never terminates
// it cannot make our carry buffer grow without bound (F2) nor freeze the
// screen by withholding all subsequent output forever: on overflow we forward
// the buffered bytes verbatim and resync.
const osc52MaxSeq = 4 * 1024

// osc52 parser states. The filter is a small state machine kept ACROSS reads
// so that, once inside a sequence, only newly-arrived bytes are scanned for
// the terminator instead of re-scanning the whole buffer every read (F9).
const (
	oscScan  = iota // outside any sequence: looking for the ESC]52; marker
	oscFrame        // marker matched: validating the payload shape (Pc ; Pd)
	oscBody         // shape ok: scanning for the BEL/ST terminator
)

// osc52Filter is a bounded, stateful OSC 52 stripper. It is driven one read at
// a time via feed and keeps its in-progress state in fields so a sequence
// straddling many reads costs O(bytes) total, not O(bytes²) (F9).
//
// Framing (F7): the bare 5-byte marker is not enough to claim a sequence —
// the remote could print "\x1b]52;" inside unrelated text, or the BEL/ST we
// scan for could belong to an ENCLOSING OSC (e.g. a window-title OSC 0/2).
// So after the marker we require a plausible payload shape (selection chars,
// then ';', then a base64/'?'/'-' data byte) before committing, and while
// scanning for the terminator we abort the moment a fresh "\x1b]" opener
// appears — that means the marker was never a real OSC 52 and we must not
// swallow the unrelated output that follows.
type osc52Filter struct {
	strip bool
	dbg   *log.Logger

	state int
	// seq accumulates the bytes of the in-progress sequence (only populated
	// in oscFrame/oscBody). It is bounded by osc52MaxSeq; on overflow the
	// sequence is abandoned and seq is flushed verbatim.
	seq []byte
	// markerLen is how many leading bytes of the osc52Prefix marker we have
	// matched while in oscScan (for a marker split across reads). 0 means we
	// are not mid-marker.
	markerLen int
	// pendingESC is set in oscBody when the last byte seen was a lone ESC
	// that could begin an ST (ESC \) split across reads, so we only need to
	// look at the single next byte rather than re-scan (F9).
	pendingESC bool
	// frameSeenSemi records, in oscFrame, that we have passed the ';' that
	// separates the selection parameter from the data, so the next byte is a
	// data byte to validate.
	frameSeenSemi bool
}

// isOSC52Selection reports whether c is a legal OSC 52 selection-parameter
// byte (the Pc field): c/p/q/s/0-7 (clipboard/primary/secondary/select/cut
// buffers). The set is intentionally narrow so unrelated text after a stray
// "\x1b]52;" fails the framing check fast.
func isOSC52Selection(c byte) bool {
	switch c {
	case 'c', 'p', 'q', 's', '0', '1', '2', '3', '4', '5', '6', '7':
		return true
	}
	return false
}

// isOSC52Data reports whether c is a plausible first data byte after the ';':
// the base64 alphabet, '=' padding, '?' (a clipboard QUERY) or '-' (clear).
func isOSC52Data(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '+', c == '/', c == '=', c == '?', c == '-':
		return true
	}
	return false
}

// feed consumes one read's worth of bytes and returns the bytes to forward to
// the screen. All parser state is retained in the receiver, so the caller must
// NOT re-prepend anything (unlike the old carry-based contract). Output is
// appended to out, which may be nil.
func (f *osc52Filter) feed(out, data []byte) []byte {
	i := 0
	for i < len(data) {
		switch f.state {
		case oscScan:
			out, i = f.scan(out, data, i)
		case oscFrame:
			out, i = f.frame(out, data, i)
		case oscBody:
			out, i = f.body(out, data, i)
		}
	}
	return out
}

// scan forwards normal output until the ESC]52; marker is found (handling a
// marker split across reads via markerLen), then transitions to oscFrame.
func (f *osc52Filter) scan(out, data []byte, i int) ([]byte, int) {
	// Already part-way through matching the marker from a previous read.
	if f.markerLen > 0 {
		for i < len(data) && f.markerLen < len(osc52Prefix) {
			if data[i] == osc52Prefix[f.markerLen] {
				f.markerLen++
				i++
				continue
			}
			// Mismatch: the held marker prefix was not a marker after all.
			// Flush it verbatim and restart scanning at this byte.
			out = append(out, osc52Prefix[:f.markerLen]...)
			f.markerLen = 0
			return out, i
		}
		if f.markerLen == len(osc52Prefix) {
			f.beginSeq()
		}
		return out, i
	}

	rest := data[i:]
	idx := indexOf(rest, osc52Prefix)
	if idx < 0 {
		// No complete marker ahead. The tail might be a prefix of the marker
		// spanning into the next read — remember that much and forward the
		// rest. (We track markerLen rather than re-prepending so the next
		// read scans only its own new bytes.)
		hold := trailingPrefixLen(rest, osc52Prefix)
		out = append(out, rest[:len(rest)-hold]...)
		f.markerLen = hold
		return out, len(data)
	}
	out = append(out, rest[:idx]...)
	f.beginSeq()
	return out, i + idx + len(osc52Prefix)
}

// beginSeq enters the framing state with the marker already buffered.
func (f *osc52Filter) beginSeq() {
	f.state = oscFrame
	f.frameSeenSemi = false
	f.markerLen = 0
	f.seq = append(f.seq[:0], osc52Prefix...)
}

// frame validates the payload shape right after the marker: selection chars,
// then ';', then a single data byte. The moment it can prove the shape is good
// it switches to oscBody; the moment it proves the shape is bad (or overflows)
// it abandons the sequence and flushes the buffered bytes verbatim.
func (f *osc52Filter) frame(out, data []byte, i int) ([]byte, int) {
	for i < len(data) {
		c := data[i]
		// A fresh OSC opener before we even validated the payload means the
		// marker was not a real OSC 52 — bail out and resync (F7).
		if c == 0x1b {
			return f.abandon(out, data, i)
		}
		if !f.frameSeenSemi {
			switch {
			case c == ';':
				f.frameSeenSemi = true
			case isOSC52Selection(c):
				// keep consuming selection chars
			default:
				// Not a valid selection byte before ';': bad framing.
				return f.abandon(out, data, i)
			}
			f.seq = append(f.seq, c)
			i++
			// Advance past c BEFORE the overflow check so overflow resyncs
			// after the buffered byte (which is already in f.seq and gets
			// flushed verbatim) — otherwise c is emitted twice. Mirrors body.
			if len(f.seq) > osc52MaxSeq {
				return f.overflow(out, data, i)
			}
			continue
		}
		// We have the ';'; the very next byte must look like data.
		if !isOSC52Data(c) {
			return f.abandon(out, data, i)
		}
		f.seq = append(f.seq, c)
		f.state = oscBody
		i++
		return out, i
	}
	return out, i
}

// body scans only the NEWLY-arrived bytes for the terminator (BEL, or ST = ESC
// \), carrying a one-byte pending-ESC state across reads so a split ST is still
// matched without re-scanning (F9). It aborts on a fresh ESC] opener (F7) and
// on overflow (F2).
//
// NOTE on the 8-bit ST (0x9C): we deliberately do NOT treat 0x9C as a
// terminator. The OSC 52 payload is base64 (7-bit ASCII), but a remote can
// interleave arbitrary bytes, and 0x9C is also a UTF-8 continuation byte
// (10011100). Recognizing it as ST would risk truncating a sequence — or, in
// pass-through mode, misframing — on a legitimate multi-byte UTF-8 run that
// merely happens to contain 0x9C. Real emitters (zellij, tmux, xterm) use BEL
// or 7-bit ST, so the 8-bit form buys nothing and only adds a corruption risk.
func (f *osc52Filter) body(out, data []byte, i int) ([]byte, int) {
	// Resolve an ESC that was the last byte of the previous read.
	if f.pendingESC {
		f.pendingESC = false
		c := data[i]
		switch c {
		case '\\': // ESC \ — 7-bit ST: sequence complete.
			f.seq = append(f.seq, c)
			return f.complete(out, i+1)
		case ']':
			// ESC ] — a new OSC opener inside what we thought was OSC 52.
			// The original marker was not a real clipboard write; resync so
			// we don't swallow the enclosing/following output (F7). Re-handle
			// the ESC (already in seq) by abandoning at the buffered ESC.
			return f.abandonPendingESC(out, data, i)
		default:
			// A lone ESC that began some other escape sequence inside the
			// payload — keep scanning; the ESC is already buffered.
		}
	}
	for i < len(data) {
		c := data[i]
		switch c {
		case 0x07: // BEL terminator.
			f.seq = append(f.seq, c)
			return f.complete(out, i+1)
		case 0x1b: // ESC: could be ST (ESC \) or a fresh OSC opener (ESC ]).
			if i+1 < len(data) {
				next := data[i+1]
				if next == '\\' {
					f.seq = append(f.seq, c, next)
					return f.complete(out, i+2)
				}
				if next == ']' {
					// Fresh OSC opener — abandon and resync (F7).
					return f.abandon(out, data, i)
				}
				// Some other ESC sequence embedded in the payload.
				f.seq = append(f.seq, c)
				i++
			} else {
				// ESC is the last byte: remember it and wait for the next
				// read's first byte to disambiguate ST vs opener.
				f.seq = append(f.seq, c)
				f.pendingESC = true
				i++
			}
		default:
			f.seq = append(f.seq, c)
			i++
		}
		if len(f.seq) > osc52MaxSeq {
			return f.overflow(out, data, i)
		}
	}
	return out, i
}

// complete finalizes a recognized OSC 52 sequence: logs it and, when stripping,
// drops it from the output (otherwise forwards the buffered bytes verbatim).
func (f *osc52Filter) complete(out []byte, i int) ([]byte, int) {
	f.dbg.Printf("OSC52 clipboard-write seen (%d bytes); strip=%v", len(f.seq), f.strip)
	if !f.strip {
		out = append(out, f.seq...)
	}
	f.reset()
	return out, i
}

// abandon gives up on a candidate that failed framing or hit a fresh OSC
// opener: it flushes the bytes buffered so far verbatim (they were NOT a real
// OSC 52 write) and resyncs the scanner at data[i] so the byte that triggered
// the abort is re-examined (it may itself start a new marker).
func (f *osc52Filter) abandon(out, data []byte, i int) ([]byte, int) {
	out = append(out, f.seq...)
	f.reset()
	return out, i
}

// abandonPendingESC handles the case where a pending ESC (already buffered, as
// the last byte of the previous read) is followed by ']' at data[i] — a fresh
// OSC opener. The original marker was not a real OSC 52 clipboard write, so we
// flush everything buffered EXCEPT that trailing ESC verbatim, then resync as
// if the marker scanner has already matched the leading ESC (markerLen=1). The
// ']' at data[i] continues the candidate marker, so we return at i still in
// oscScan and let scan carry on matching ESC]52; from byte two.
func (f *osc52Filter) abandonPendingESC(out, data []byte, i int) ([]byte, int) {
	if n := len(f.seq); n > 0 {
		out = append(out, f.seq[:n-1]...) // everything before the trailing ESC
	}
	f.reset()
	f.markerLen = 1 // ESC of a possible new marker already consumed
	return out, i
}

// overflow abandons an in-progress sequence that exceeded osc52MaxSeq: the
// buffered bytes are forwarded verbatim and scanning resumes at data[i], so
// neither memory (F2) nor the display is held hostage by an unterminated
// marker.
func (f *osc52Filter) overflow(out, data []byte, i int) ([]byte, int) {
	f.dbg.Printf("OSC52 sequence exceeded %d bytes without terminating; forwarding verbatim and resyncing", osc52MaxSeq)
	out = append(out, f.seq...)
	f.reset()
	return out, i
}

// reset returns the filter to the scanning state, dropping any buffered
// sequence bytes but keeping the backing array for reuse.
func (f *osc52Filter) reset() {
	f.state = oscScan
	f.seq = f.seq[:0]
	f.markerLen = 0
	f.pendingESC = false
	f.frameSeenSemi = false
}

// flushEOF returns the bytes (if any) to emit when the input reaches EOF.
// A complete sequence cannot be pending (complete consumes it immediately), so
// anything still buffered is an UNTERMINATED candidate. When stripping we drop
// it — emitting a half marker would defeat the strip guarantee and could leave
// the local terminal waiting for a terminator. When not stripping we forward it
// verbatim, since in pass-through mode our job is only to avoid altering bytes.
func (f *osc52Filter) flushEOF(out []byte) []byte {
	switch f.state {
	case oscFrame, oscBody:
		if !f.strip {
			out = append(out, f.seq...)
		} else {
			f.dbg.Printf("EOF with dangling OSC52 marker (%d buffered bytes); dropping", len(f.seq))
		}
	case oscScan:
		// A held partial marker prefix (markerLen>0) is, by definition, a
		// strict prefix of "\x1b]52;" with no payload — also dangling. Drop
		// it when stripping, forward it verbatim otherwise.
		if f.markerLen > 0 {
			if !f.strip {
				out = append(out, osc52Prefix[:f.markerLen]...)
			} else {
				f.dbg.Printf("EOF with dangling OSC52 marker prefix (%d bytes); dropping", f.markerLen)
			}
		}
	}
	f.reset()
	return out
}

// copyFilteringOSC52 copies src→dst, optionally removing OSC 52
// clipboard-write sequences (which remote apps like zellij use to overwrite
// the LOCAL clipboard — clobbering a copied image and breaking Ctrl+V image
// paste). When strip is false it only logs them. Parser state is kept across
// reads (see osc52Filter) so an unterminated marker is bounded and never
// freezes the screen, and an in-progress sequence is not re-scanned each read.
func copyFilteringOSC52(dst io.Writer, src io.Reader, strip bool, dbg *log.Logger) {
	buf := make([]byte, 32*1024)
	f := &osc52Filter{strip: strip, dbg: dbg}
	var out []byte
	for {
		n, err := src.Read(buf)
		if n > 0 {
			out = f.feed(out[:0], buf[:n])
			if len(out) > 0 {
				_, _ = dst.Write(out)
			}
		}
		if err != nil {
			out = f.flushEOF(out[:0])
			if len(out) > 0 {
				_, _ = dst.Write(out)
			}
			return
		}
	}
}

// filterOSC52 is the stateless, per-call form retained for tests and any
// caller that prefers the old carry-based contract: it returns the bytes to
// forward plus any trailing bytes to hold back and RE-PREPEND to the next
// call's input (a sequence/marker not yet terminated). copyFilteringOSC52 uses
// osc52Filter directly instead so it need not re-prepend (F9). When strip is
// true, complete OSC 52 sequences are dropped from the forwarded output; either
// way they are logged.
func filterOSC52(data []byte, strip bool, dbg *log.Logger) (out, carry []byte) {
	f := &osc52Filter{strip: strip, dbg: dbg}
	out = f.feed(make([]byte, 0, len(data)), data)
	// Whatever is still in-progress at the end of this chunk is the carry the
	// old contract expects the caller to re-prepend next time: the buffered
	// (unterminated) sequence, or a held marker prefix.
	switch f.state {
	case oscFrame, oscBody:
		carry = append([]byte(nil), f.seq...)
	case oscScan:
		if f.markerLen > 0 {
			carry = append([]byte(nil), osc52Prefix[:f.markerLen]...)
		}
	}
	return out, carry
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
