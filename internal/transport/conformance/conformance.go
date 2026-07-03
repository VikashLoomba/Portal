// Package conformance is the shared behavioral test suite every
// transport.Transport implementation must pass. It is a normal (non-_test)
// package so multiple _test.go files across the tree can import and run it.
//
// The suite uses ONLY argv that is invariant under the pinned shell-join model
// (single-space join, target shell re-splits): every multi-token program is
// pre-quoted into a single argv element via shellQuote, so localexec (sh -c),
// sshnative (ssh.Session), and sshctl (ssh binary) all execute the same
// command string.
package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/transport"
)

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// survives the shell-join model as ONE argv element. Identical to the
// install/doctor/bootstrap/clipupload helpers.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Run executes the full conformance suite against a fresh transport produced
// by newT. name is used to label the top-level subtest. The PortForwarder
// section runs only when the transport asserts the capability.
func Run(t *testing.T, name string, newT func(t *testing.T) transport.Transport) {
	t.Run(name, func(t *testing.T) {
		t.Run("core", func(t *testing.T) { runCore(t, newT) })
		t.Run("portforward", func(t *testing.T) { runPortForward(t, newT) })
	})
}

func runCore(t *testing.T, newT func(t *testing.T) transport.Transport) {
	ctx := context.Background()

	t.Run("exec_stdout_roundtrip", func(t *testing.T) {
		tr := newT(t)
		payload := "hello\nworld\n"
		stdout, _, err := tr.Exec(ctx, []byte(payload), "cat")
		if err != nil {
			t.Fatalf("Exec cat: %v", err)
		}
		if stdout != payload {
			t.Errorf("stdout = %q, want %q", stdout, payload)
		}
	})

	t.Run("exec_stderr_capture", func(t *testing.T) {
		tr := newT(t)
		stdout, stderr, err := tr.Exec(ctx, nil, "sh", "-c", shellQuote("echo ERRLINE >&2"))
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(stderr, "ERRLINE") {
			t.Errorf("stderr = %q, want to contain ERRLINE", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
	})

	t.Run("exec_nonzero_exit", func(t *testing.T) {
		tr := newT(t)
		if _, _, err := tr.Exec(ctx, nil, "false"); err == nil {
			t.Error("Exec false: want error, got nil")
		}
		tr2 := newT(t)
		_, _, err := tr2.Exec(ctx, nil, "sh", "-c", shellQuote("exit 7"))
		if err == nil {
			t.Fatal("Exec exit 7: want error, got nil")
		}
		if !strings.Contains(err.Error(), "7") {
			t.Errorf("error = %q, want to mention 7", err.Error())
		}
	})

	t.Run("exit_code_typed", func(t *testing.T) {
		tr := newT(t)
		_, _, err := tr.Exec(ctx, nil, "sh", "-c", shellQuote("exit 3"))
		if err == nil {
			t.Fatal("Exec exit 3: want error, got nil")
		}
		code, ok := transport.ExitCode(err)
		if !ok || code != 3 {
			t.Errorf("ExitCode(err) = (%d, %v), want (3, true)", code, ok)
		}
		tr2 := newT(t)
		_, _, err2 := tr2.Exec(ctx, nil, "true")
		if err2 != nil {
			t.Fatalf("Exec true: %v", err2)
		}
		if c2, ok2 := transport.ExitCode(err2); ok2 {
			t.Errorf("ExitCode(nil err) = (%d, %v), want (0, false)", c2, ok2)
		}
	})

	t.Run("exec_binary_stdin", func(t *testing.T) {
		tr := newT(t)
		payload := []byte{0x00, 0x01, 0xff, 0xfe, 0x0a, 0x7f, 0x80}
		stdout, _, err := tr.Exec(ctx, payload, "cat")
		if err != nil {
			t.Fatalf("Exec cat: %v", err)
		}
		if !bytes.Equal([]byte(stdout), payload) {
			t.Errorf("stdout = %v, want %v", []byte(stdout), payload)
		}
	})

	t.Run("stream_cat_bidirectional", func(t *testing.T) {
		tr := newT(t)
		stdin, stdout, _, wait, err := tr.Stream(ctx, "cat")
		if err != nil {
			t.Fatalf("Stream cat: %v", err)
		}
		payload := []byte("ping-pong\n")
		if _, err := stdin.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(stdout, got); err != nil {
			t.Fatalf("read echo: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("echo = %q, want %q", got, payload)
		}
		if err := stdin.Close(); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
		// After stdin close, cat exits, stdout hits EOF, wait returns.
		if rest, err := io.ReadAll(stdout); err != nil {
			t.Fatalf("drain stdout: %v", err)
		} else if len(rest) != 0 {
			t.Errorf("trailing stdout = %q, want empty", rest)
		}
		if err := wait(); err != nil {
			t.Errorf("wait: %v", err)
		}
	})

	t.Run("ensure_idempotent", func(t *testing.T) {
		tr := newT(t)
		if _, err := tr.Ensure(ctx); err != nil {
			t.Fatalf("Ensure #1: %v", err)
		}
		rebuilt, err := tr.Ensure(ctx)
		if err != nil {
			t.Fatalf("Ensure #2: %v", err)
		}
		if rebuilt {
			t.Error("Ensure #2 rebuilt = true, want false")
		}
	})

	t.Run("health_up_after_ensure", func(t *testing.T) {
		tr := newT(t)
		if _, err := tr.Ensure(ctx); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		h, err := tr.Health(ctx)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !h.Up {
			t.Error("Health.Up = false, want true")
		}
	})

	t.Run("close_no_panic", func(t *testing.T) {
		tr := newT(t)
		if _, err := tr.Ensure(ctx); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if _, err := tr.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// runPortForward runs the PortForwarder section as a single ordered flow. It
// executes only when the transport asserts transport.PortForwarder (localexec
// is skipped entirely).
//
// The byte-echo step is guarded by a loopback-reachability gate so it is
// deterministically green for BOTH the in-process native transport and a real
// system-ssh remote. Reasoning: localexec does not implement PortForwarder, so
// among PortForwarder impls only native is loopback-reachable — its in-process
// server's direct-tcpip dials localhost on the SAME host as the test listener;
// system-ssh forwards to a DIFFERENT machine, so a dial of localhost:remote on
// that box could never reach a listener stood up here. We therefore compute
// loopback := Describe().Impl != "system-ssh" and skip the echo for system-ssh.
func runPortForward(t *testing.T, newT func(t *testing.T) transport.Transport) {
	ctx := context.Background()
	tr := newT(t)
	pf, ok := tr.(transport.PortForwarder)
	if !ok {
		t.Skip("transport does not implement PortForwarder")
	}
	if _, err := tr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	loopback := tr.Describe().Impl != "system-ssh"

	local := freePort(t)
	remotePort := freePort(t)

	// Step 1: Forward returns nil.
	if err := pf.Forward(ctx, local, remotePort); err != nil {
		t.Fatalf("Forward(%d,%d): %v", local, remotePort, err)
	}

	// Step 2: ListForwards contains local.
	ports, err := pf.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards: %v", err)
	}
	if !containsInt(ports, local) {
		t.Fatalf("ListForwards = %v, want to contain %d", ports, local)
	}

	// Step 3 (loopback/native only): full echo round-trip while the forward
	// is STILL alive, then assert ForwardLines shows the registry-shaped
	// 127.0.0.1:<local> entry, then tear down the echo dialer + fake remote.
	if loopback {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
		if err != nil {
			t.Fatalf("fake remote listen on %d: %v", remotePort, err)
		}
		echoDone := make(chan struct{})
		go func() {
			defer close(echoDone)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			io.Copy(c, c)
		}()

		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 5*time.Second)
		if err != nil {
			ln.Close()
			t.Fatalf("dial forwarded 127.0.0.1:%d: %v", local, err)
		}
		msg := []byte("echo-through-forward\n")
		if _, err := conn.Write(msg); err != nil {
			t.Fatalf("write through forward: %v", err)
		}
		got := make([]byte, len(msg))
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read echo through forward: %v", err)
		}
		if !bytes.Equal(got, msg) {
			t.Errorf("echo = %q, want %q", got, msg)
		}

		lines, err := pf.ForwardLines(ctx)
		if err != nil {
			t.Fatalf("ForwardLines: %v", err)
		}
		wantEntry := fmt.Sprintf("127.0.0.1:%d", local)
		if !containsStr(lines, wantEntry) {
			t.Errorf("ForwardLines = %v, want to contain %q", lines, wantEntry)
		}

		conn.Close()
		ln.Close()
		<-echoDone
	}

	// Step 4: Cancel.
	if err := pf.Cancel(ctx, local, remotePort); err != nil {
		t.Fatalf("Cancel(%d,%d): %v", local, remotePort, err)
	}

	// Step 5 (LAST): dial the local port expecting connection-refused — the
	// local listener is now closed regardless of remote reachability.
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 2*time.Second); err == nil {
		c.Close()
		t.Errorf("dial 127.0.0.1:%d succeeded after Cancel, want refused", local)
	}
}

// freePort binds an ephemeral port, closes it, and returns the number. There
// is an inherent TOCTOU window; the suite tolerates it because the tests are
// serial per transport and the window is small.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	if err != nil {
		t.Fatalf("parse reserved addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse reserved port: %v", err)
	}
	return port
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
