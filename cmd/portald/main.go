// Command portald is the linux-amd64 agent half of the portal split-daemon
// architecture. It runs on the user's dev box, watches for new loopback
// TCP listening sockets via NETLINK_SOCK_DIAG, and pushes events to the
// Mac client over stdin/stdout (framed CBOR — see internal/protocol).
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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vikashl/portal/internal/agent"
	"github.com/vikashl/portal/internal/agent/watcher"
)

// readKernel is implemented per-OS (see main_linux.go / main_other.go).
var readKernel = func() string { return "" }

// gitSHA is set at build time via -ldflags "-X main.gitSHA=...".
var gitSHA = "dev"

func main() {
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

	// stderr → slog, stdout → reserved for protocol writer.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Wrap stdout so accidental fmt.Println from anywhere panics loudly
	// instead of corrupting the wire.
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
	})

	if err := srv.Serve(ctx); err != nil {
		if errors.Is(err, io.EOF) {
			return
		}
		logger.Error("agent server exited", "err", err, "proto_version", *protoVersion)
		os.Exit(2)
	}
}


// panicWriter wraps os.Stdout. Only the protocol Encoder is allowed to
// write; any rogue write (e.g. someone calls fmt.Println) panics so the
// failure is loud rather than silently desyncing the wire.
type panicWriter struct{ w io.Writer }

func newPanicWriter(w io.Writer) *panicWriter { return &panicWriter{w: w} }

func (p *panicWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
