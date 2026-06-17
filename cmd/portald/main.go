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
func cmdSockPath() string {
	self, err := os.Executable()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".cache", "portal", "cmd.sock")
	}
	return filepath.Join(filepath.Dir(self), "cmd.sock")
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
		In:                os.Stdin,
		Out:               guarded,
		Watcher:           w,
		AgentSHA:          gitSHA,
		Kernel:            readKernel(),
		BootID:            agent.ReadBootID(),
		HeartbeatInterval: 5 * time.Second,
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

// runOpen connects to the cmd socket and sends the URL. If the socket
// doesn't exist or the agent reports "no-client", we exit 1 so the
// xdg-open wrapper knows to fall back to the system xdg-open.
func runOpen(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: portald open <url>")
		os.Exit(1)
	}
	url := strings.Join(args, " ")
	sock := cmdSockPath()

	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		// Socket not present — no active client.
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := fmt.Fprintf(conn, "%s\n", url); err != nil {
		os.Exit(1)
	}
	buf := make([]byte, 32)
	n, _ := conn.Read(buf)
	resp := strings.TrimSpace(string(buf[:n]))
	if resp != "ok" {
		os.Exit(1)
	}
	// Exit 0 — Mac will open it.
}

type panicWriter struct{ w io.Writer }

func newPanicWriter(w io.Writer) *panicWriter { return &panicWriter{w: w} }
func (p *panicWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

var _ = context.Background // suppress unused import warning if needed
