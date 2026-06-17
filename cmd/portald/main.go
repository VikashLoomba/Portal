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
	"strings"
	"syscall"
	"time"

	"github.com/vikashl/portal/internal/agent"
	"github.com/vikashl/portal/internal/agent/watcher"
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

	var (
		protoVersion = flag.Uint("proto-version", 1, "wire protocol version expected by the client")
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
		In:                 os.Stdin,
		Out:                guarded,
		Watcher:            w,
		AgentSHA:           gitSHA,
		Kernel:             readKernel(),
		BootID:             agent.ReadBootID(),
		HeartbeatInterval:  5 * time.Second,
		BackpressureKill:   5 * time.Second,
		Log:                logger,
		CmdSockPath:        cmdSockPath(),
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
		fmt.Fprintf(conn, "%s\n", rawURL)
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

type panicWriter struct{ w io.Writer }

func newPanicWriter(w io.Writer) *panicWriter { return &panicWriter{w: w} }
func (p *panicWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

var _ = context.Background // suppress unused import warning if needed
