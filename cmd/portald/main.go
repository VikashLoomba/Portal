// Command portald is the linux-amd64 agent half of the portal split-daemon
// architecture. It runs on the user's dev box, watches for new loopback
// TCP listening sockets via NETLINK_SOCK_DIAG, and pushes events to the
// Mac client over stdin/stdout (framed CBOR — see internal/protocol).
//
// Subcommands:
//
//	(default)  Run as the Mac-connected RPC agent.
//	open <url> Relay a URL to the connected Mac client via the cmd socket.
//	           Used internally by the ~/.local/bin/xdg-open wrapper.
//
// Started by the local client via:
//
//	ssh -S <ctlpath> <host> ~/.cache/portal/agent-<sha> --proto-version=1
//
// stdin/stdout carry framed CBOR; stderr carries human-readable slog text
// that the local client tee's into ~/Library/Logs/portal.log.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
)

// readKernel is implemented per-OS (see uname_linux.go / stub elsewhere).
var readKernel = func() string { return "" }

// gitSHA is set at build time via -ldflags "-X main.gitSHA=...".
var gitSHA = "dev"

// cmdSockPath returns the Unix socket path for the `portald open` IPC.
// Lives alongside the agent binary so permissions match the cache dir.
// The PID is included so two simultaneous Mac clients each get their own
// socket — they spawn separate agent processes, each writing to
// ~/.cache/portal/cmd-<pid>.sock.
func cmdSockPath() string {
	dir := filepath.Join(os.Getenv("HOME"), ".cache", "portal")
	self, err := os.Executable()
	if err == nil {
		dir = filepath.Dir(self)
	}
	return filepath.Join(dir, fmt.Sprintf("cmd-%d.sock", os.Getpid()))
}

func main() {
	// "portald open <url>" subcommand — relay a URL to the cmd socket.
	if len(os.Args) >= 2 && os.Args[1] == "open" {
		runOpen(os.Args[2:])
		return
	}

	// "portald clip <targets|image png|text>" subcommand — the read shim's
	// entry point. It is the SOLE ARBITER OF SUCCESS: it exits non-zero unless
	// every safety check passes (DESIGN §6.1), so a poisoned reply makes the
	// shim fall through to the real binary rather than feed the agent garbage.
	if len(os.Args) >= 2 && os.Args[1] == "clip" {
		runClip(os.Args[2:])
		return
	}

	// "portald notify [--hook | --title … --body …]" subcommand — relay a
	// notification to the connected Mac client via the cmd socket. Invoked by a
	// Claude Code hook (--hook reads the hook JSON from stdin → verified) or by
	// a generic caller (--title/--body → unverified). Exits 0 if a client
	// accepted it; 1 otherwise (so a hook caller can detect non-delivery).
	if len(os.Args) >= 2 && os.Args[1] == "notify" {
		runNotify(os.Args[2:])
		return
	}

	var (
		protoVersion = flag.Uint("proto-version", 2, "wire protocol version expected by the client")
		pollMs       = flag.Int("poll-ms", 75, "INET_DIAG dump period in ms (50-200 recommended)")
		useMC        = flag.Bool("destroy-mc", true, "subscribe to SKNLGRP_INET_TCP_DESTROY for instant remove events")
		printSHA     = flag.Bool("sha", false, "print the embedded git SHA and exit")
	)
	flag.Parse()

	if *printSHA {
		fmt.Println(gitSHA)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// stdout is reserved for the protocol Encoder; protect it.
	guarded := newPanicWriter(os.Stdout)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	w, err := watcher.NewNetlink(watcher.NetlinkConfig{
		PollInterval:        *pollMs,
		UseDestroyMulticast: *useMC,
	})
	if err != nil {
		logger.Error("watcher init failed", "err", err)
		os.Exit(3)
	}

	srv := agent.New(agent.Config{
		In:                os.Stdin,
		Out:               guarded,
		Watcher:           w,
		AgentSHA:          gitSHA,
		Kernel:            readKernel(),
		BootID:            agent.ReadBootID(),
		HeartbeatInterval: 5 * time.Second,
		BackpressureKill:  5 * time.Second,
		Log:               logger,
		CmdSockPath:       cmdSockPath(),
	})

	if err := srv.Serve(ctx); err != nil {
		if errors.Is(err, io.EOF) {
			return
		}
		logger.Error("agent server exited", "err", err, "proto_version", *protoVersion)
		os.Exit(2)
	}
}

// runOpen connects to a cmd socket and sends the URL. Tries all
// cmd-*.sock files in the cache dir so it works when multiple Mac
// clients are connected simultaneously (each spawns its own agent process
// with a distinct pid-keyed socket). Exits 0 if at least one client accepted
// the URL; exits 1 otherwise so xdg-open falls through to the real binary.
func runOpen(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: portald open <url>")
		os.Exit(1)
	}
	rawURL := strings.Join(args, " ")

	dir := filepath.Join(os.Getenv("HOME"), ".cache", "portal")
	self, err := os.Executable()
	if err == nil {
		dir = filepath.Dir(self)
	}

	// Collect all cmd-*.sock files (one per connected agent process).
	entries, _ := filepath.Glob(filepath.Join(dir, "cmd-*.sock"))
	if len(entries) == 0 {
		os.Exit(1) // no active client
	}

	accepted := false
	for _, sock := range entries {
		conn, err := net.DialTimeout("unix", sock, 1*time.Second)
		if err != nil {
			continue
		}
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		// The agent's handleCmdConn is a tab-framed verb dispatcher expecting
		// lines like "open\t<url>\n". Without the "open\t" prefix the agent
		// parses the entire URL as an (unknown) verb and default-denies it with
		// "rejected\n", silently breaking xdg-open relaying from the remote box.
		fmt.Fprintf(conn, "open\t%s\n", rawURL)
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		conn.Close()
		if strings.TrimSpace(string(buf[:n])) == "ok" {
			accepted = true
			break // first accepting client is sufficient
		}
	}
	if !accepted {
		os.Exit(1)
	}
}

// shaRE is the ONLY accepted shape for a clip SHA from the wire: 32 lowercase
// hex chars (the 128-bit content address clipupload.ShortSHA produces). The
// local filename is reconstructed from this and nothing else (DESIGN §6.1/§7),
// so a malicious socket reply can never name an arbitrary path.
var shaRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// pngMagic is the 8-byte PNG signature. A served image file must start with
// exactly these bytes or portald exits non-zero (so the shim falls through).
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// Clip timeouts (DESIGN §4.5). The shim's dial+read deadline is the LARGEST in
// the budget so portald never gives up before the agent answers; everything
// downstream (agent socket deadline 11s, clipTimeout 9s) is strictly smaller.
const (
	clipDialTimeout = 2 * time.Second
	clipReadTimeout = 13 * time.Second
)

// clipReply is the parsed cmd-socket answer to a clip verb.
type clipReply struct {
	ok      bool
	payload string // for targets: "image/png"; for image/text: the SHA
}

// runClip implements `portald clip <targets|image png|text>`. It fans out over
// cmd-*.sock like runOpen but REFUSES (exit 1) if more than one DISTINCT agent
// socket answers, because on a shared box two Macs could be connected and user
// A's paste must never be served from user B's clipboard (DESIGN §7.3). It then
// applies the kind-specific verification and emits bytes only when everything
// checks out. EVERY adverse path exits 1 with empty stdout so the agent's
// `>tmp || …` fallback chain advances to the real binary.
func runClip(args []string) {
	var verb, format, tool string
	switch {
	case len(args) == 2 && args[0] == "targets" && (args[1] == "xclip" || args[1] == "wl-paste"):
		// `clip targets <tool>`: the tool identity decides which target-name
		// lines to print for a text clipboard (xclip wants UTF8_STRING/TEXT/
		// STRING; wl-paste wants text/plain). Image always prints image/png.
		verb, tool = "targets", args[1]
	case len(args) == 1 && args[0] == "targets":
		// Backwards-compatible bare form: assume xclip target names.
		verb, tool = "targets", "xclip"
	case len(args) == 1 && args[0] == "text":
		verb = "text"
	case len(args) == 2 && args[0] == "image" && args[1] == "png":
		verb, format = "image", "png"
	default:
		fmt.Fprintln(os.Stderr, "usage: portald clip <targets [xclip|wl-paste]|image png|text>")
		os.Exit(1)
	}

	// Build the tab-framed request line the agent's cmd socket parses.
	var line string
	switch verb {
	case "targets":
		line = "clip\ttargets\n"
	case "text":
		line = "clip\ttext\n"
	case "image":
		line = "clip\timage\t" + format + "\n"
	}

	reply, ok := clipFanout(line)
	if !ok {
		os.Exit(1) // no client, >1 client, dial fail, EOF, non-ok — all fall through
	}

	switch verb {
	case "targets":
		// The agent answered the CANONICAL kind ("image" or "text"). Map it to
		// the tool-specific target-name line(s) the caller (xclip vs wl-paste)
		// greps for. Any other payload → exit 1 (so the shim falls through).
		out := targetLines(reply.payload, tool)
		if out == "" {
			os.Exit(1)
		}
		if _, err := os.Stdout.WriteString(out); err != nil {
			os.Exit(1)
		}
	case "image":
		emitClipFile(reply.payload, "clip-", ".png", verifyPNG)
	case "text":
		emitClipFile(reply.payload, "text-", ".txt", nil)
	}
}

// clipFanout dials every cmd-*.sock, sends line, and returns the parsed reply.
// It returns ok=false unless EXACTLY ONE distinct connected agent answered with
// an "ok" reply. Multiple distinct connected agents → refuse (DESIGN §7.3).
// "Connected" means the socket accepted the connection and answered (a stale
// socket whose agent is gone fails to dial and is ignored).
func clipFanout(line string) (clipReply, bool) {
	dir := filepath.Join(os.Getenv("HOME"), ".cache", "portal")
	if self, err := os.Executable(); err == nil {
		dir = filepath.Dir(self)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "cmd-*.sock"))
	if len(entries) == 0 {
		return clipReply{}, false
	}

	var okReplies []clipReply
	connected := 0
	for _, sock := range entries {
		conn, err := net.DialTimeout("unix", sock, clipDialTimeout)
		if err != nil {
			continue // stale socket / agent gone
		}
		connected++
		conn.SetDeadline(time.Now().Add(clipReadTimeout))
		_, _ = io.WriteString(conn, line)
		// Replies are tiny single lines ("ok\t<sha>\n", "ok\timage/png\n",
		// "none\n", "rejected\n"). A bounded read is plenty.
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		conn.Close()
		r, ok := parseClipReply(string(buf[:n]))
		if ok {
			okReplies = append(okReplies, r)
		}
	}

	// Multi-client safety: if more than one distinct agent is CONNECTED, refuse
	// regardless of how many answered ok — we cannot tell which Mac is "ours".
	if connected > 1 {
		return clipReply{}, false
	}
	if len(okReplies) != 1 {
		return clipReply{}, false
	}
	return okReplies[0], true
}

// targetLines maps the agent's canonical clipboard kind ("image"/"text") to the
// exact target-name line(s) the requesting tool greps for, terminated by
// newlines. xclip advertises text as the X11 selection target atoms
// UTF8_STRING/TEXT/STRING (matching cc-clip's TARGETS reply); wl-paste
// advertises the MIME type text/plain. Image is always image/png. An
// unrecognized kind or tool returns "" so the caller exits 1 (falls through).
func targetLines(kind, tool string) string {
	switch kind {
	case "image":
		return "image/png\n"
	case "text":
		switch tool {
		case "wl-paste":
			return "text/plain\n"
		default: // xclip
			return "UTF8_STRING\nTEXT\nSTRING\n"
		}
	default:
		return ""
	}
}

// parseClipReply parses one cmd-socket reply line. Accepts only "ok\t<payload>"
// (payload non-empty). "none", "rejected", "dropped", "no-client", EOF/empty —
// everything else — is a non-ok reply (ok=false).
func parseClipReply(raw string) (clipReply, bool) {
	line := strings.TrimRight(raw, "\r\n")
	if line == "" {
		return clipReply{}, false
	}
	verb, payload, hasTab := strings.Cut(line, "\t")
	if verb != "ok" || !hasTab {
		return clipReply{}, false
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return clipReply{}, false
	}
	return clipReply{ok: true, payload: payload}, true
}

// emitClipFile reconstructs the side-channel file path LOCALLY from the SHA
// (never from the wire), opens it O_NOFOLLOW, verifies it is a regular file
// under the 0700 cache dir with Size()>0, runs the optional content check
// (PNG magic for images), BUFFERS the whole file, and only THEN copies it to
// stdout. Buffer-then-verify, never stream-then-discover: the agent's `>tmp`
// already truncated the target, so a half-written stream that fails mid-way
// cannot be undone (DESIGN §6.1). On ANY doubt → exit 1, emit nothing.
func emitClipFile(sha, prefix, ext string, check func([]byte) error) {
	if !shaRE.MatchString(sha) {
		os.Exit(1)
	}
	home := os.Getenv("HOME")
	if home == "" {
		os.Exit(1)
	}
	dir := filepath.Join(home, ".cache", "portal", "clip")
	path := filepath.Join(dir, prefix+sha+ext)

	// O_NOFOLLOW: refuse to follow a symlink planted at the target path — a
	// same-uid attacker must not be able to make us cat ~/.ssh/id_ed25519.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		os.Exit(1)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		os.Exit(1)
	}

	// Buffer the whole file, then verify, then emit.
	data, err := io.ReadAll(f)
	if err != nil || int64(len(data)) != fi.Size() {
		os.Exit(1)
	}
	if check != nil {
		if err := check(data); err != nil {
			os.Exit(1)
		}
	}
	if _, err := os.Stdout.Write(data); err != nil {
		os.Exit(1)
	}
}

// verifyPNG checks the 8-byte PNG signature. Returns an error on mismatch.
func verifyPNG(data []byte) error {
	if len(data) < len(pngMagic) || !bytesEqualPrefix(data, pngMagic) {
		return errors.New("not a PNG")
	}
	return nil
}

func bytesEqualPrefix(data, prefix []byte) bool {
	for i := range prefix {
		if data[i] != prefix[i] {
			return false
		}
	}
	return true
}

type panicWriter struct{ w io.Writer }

func newPanicWriter(w io.Writer) *panicWriter      { return &panicWriter{w: w} }
func (p *panicWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

var _ = context.Background // suppress unused import warning if needed
