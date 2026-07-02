package sshnative

import (
	"bytes"
	"context"
	"io"
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// newForwardClient stands up the T6 in-process server plus a native Client
// wired to it through the temp-dir injection seams, Ensures it, and returns the
// live client. Cleanup closes the client (dropping any live forwards).
func newForwardClient(t *testing.T) *Client {
	t.Helper()
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)

	c, err := New(srv.target("testuser"),
		WithConfigResolver(passthroughResolver),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return c
}

// startEchoRemote stands up a stdlib TCP listener that echoes every accepted
// connection — the real 'remote' service the forward proxies to via
// direct-tcpip. It returns the listening port and closes the listener on
// cleanup.
func startEchoRemote(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("remote listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return portOf(t, ln.Addr())
}

// startRequestResponseRemote stands up a TCP listener whose handler reads the
// ENTIRE request to EOF (i.e. it waits for the client's shutdown(SHUT_WR)) and
// only THEN writes its response before closing — the classic request/response
// TCP protocol that a half-close-unaware proxy truncates. It returns the port.
func startRequestResponseRemote(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("remote listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, _ := io.ReadAll(c) // blocks until the client half-closes its write side
				c.Write([]byte("response-for:" + string(req)))
			}(conn)
		}
	}()
	return portOf(t, ln.Addr())
}

// reserveLocalPort binds an ephemeral loopback port, closes it, and returns the
// number for use as a Forward local port. Small TOCTOU window tolerated (tests
// are serial).
func reserveLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	port := portOf(t, ln.Addr())
	ln.Close()
	return port
}

func portOf(t *testing.T, addr net.Addr) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("split host/port %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return port
}

// TestForwardRoundTrip (EC5) proves the full local-listener -> direct-tcpip ->
// in-process echo path, that ListForwards/ForwardLines reflect the registry,
// and that Cancel closes the listener (connection-refused afterward).
func TestForwardRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newForwardClient(t)
	remotePort := startEchoRemote(t)
	local := reserveLocalPort(t)

	if err := c.Forward(ctx, local, remotePort); err != nil {
		t.Fatalf("Forward(%d,%d): %v", local, remotePort, err)
	}

	// ListForwards reflects reality: contains the live local port.
	ports, err := c.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards: %v", err)
	}
	if !reflect.DeepEqual(ports, []int{local}) {
		t.Errorf("ListForwards = %v, want [%d]", ports, local)
	}

	// ForwardLines returns the registry-shaped 127.0.0.1:<local> entry.
	lines, err := c.ForwardLines(ctx)
	if err != nil {
		t.Fatalf("ForwardLines: %v", err)
	}
	wantLine := "127.0.0.1:" + strconv.Itoa(local)
	if !reflect.DeepEqual(lines, []string{wantLine}) {
		t.Errorf("ForwardLines = %v, want [%s]", lines, wantLine)
	}

	// Echo round-trip through the forward while it is alive.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(local), 5*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded 127.0.0.1:%d: %v", local, err)
	}
	msg := []byte("payload-through-direct-tcpip\n")
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
	conn.Close()

	// Cancel closes the listener; a subsequent dial is refused.
	if err := c.Cancel(ctx, local, remotePort); err != nil {
		t.Fatalf("Cancel(%d,%d): %v", local, remotePort, err)
	}
	ports, err = c.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards after Cancel: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("ListForwards after Cancel = %v, want empty", ports)
	}
	if refused := dialRefused(t, local); !refused {
		t.Errorf("dial 127.0.0.1:%d after Cancel succeeded, want connection-refused", local)
	}
}

// TestForwardHalfCloseDeliversResponse (finding 1) proves the forward honors TCP
// half-close: a client that writes its request, shutdown(SHUT_WR)s to signal
// end-of-request, and then reads the reply must receive the FULL response. The
// remote service writes its reply only AFTER reading the request to EOF, so a
// proxy that tears down both directions on the client FIN would truncate/drop
// the reply. `cat`-style echo tests cannot catch this because they never
// half-close; this test does.
func TestForwardHalfCloseDeliversResponse(t *testing.T) {
	ctx := context.Background()
	c := newForwardClient(t)
	remotePort := startRequestResponseRemote(t)
	local := reserveLocalPort(t)

	if err := c.Forward(ctx, local, remotePort); err != nil {
		t.Fatalf("Forward(%d,%d): %v", local, remotePort, err)
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(local), 5*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded 127.0.0.1:%d: %v", local, err)
	}
	defer conn.Close()

	req := []byte("half-close-request")
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// Signal end-of-request by half-closing the write side (SHUT_WR) while keeping
	// the read side open for the reply.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response after half-close: %v", err)
	}
	want := []byte("response-for:" + string(req))
	if !bytes.Equal(got, want) {
		t.Errorf("response after half-close = %q, want %q (reply was truncated — half-close not honored)", got, want)
	}
}

// TestForwardListForwardsSortedUnique proves ListForwards/ForwardLines return
// sorted local ports/lines and that a cancelled port is excluded.
func TestForwardListForwardsSortedUnique(t *testing.T) {
	ctx := context.Background()
	c := newForwardClient(t)
	remotePort := startEchoRemote(t)

	p1 := reserveLocalPort(t)
	p2 := reserveLocalPort(t)
	for p2 == p1 {
		p2 = reserveLocalPort(t)
	}
	for _, p := range []int{p1, p2} {
		if err := c.Forward(ctx, p, remotePort); err != nil {
			t.Fatalf("Forward(%d): %v", p, err)
		}
	}

	ports, err := c.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards: %v", err)
	}
	lo, hi := p1, p2
	if lo > hi {
		lo, hi = hi, lo
	}
	if !reflect.DeepEqual(ports, []int{lo, hi}) {
		t.Errorf("ListForwards = %v, want sorted [%d %d]", ports, lo, hi)
	}
	lines, err := c.ForwardLines(ctx)
	if err != nil {
		t.Fatalf("ForwardLines: %v", err)
	}
	want := []string{"127.0.0.1:" + strconv.Itoa(lo), "127.0.0.1:" + strconv.Itoa(hi)}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("ForwardLines = %v, want %v", lines, want)
	}

	// Cancel one; it drops out of the list, the other remains.
	if err := c.Cancel(ctx, p1, remotePort); err != nil {
		t.Fatalf("Cancel(%d): %v", p1, err)
	}
	ports, err = c.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards after Cancel: %v", err)
	}
	if !reflect.DeepEqual(ports, []int{p2}) {
		t.Errorf("ListForwards after Cancel = %v, want [%d]", ports, p2)
	}
}

// TestForwardListenFailureRegistersNothing proves a listen failure returns the
// error and registers nothing.
func TestForwardListenFailureRegistersNothing(t *testing.T) {
	ctx := context.Background()
	c := newForwardClient(t)
	remotePort := startEchoRemote(t)

	// Occupy a port so Forward's net.Listen fails with address-in-use.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listen: %v", err)
	}
	defer occupied.Close()
	local := portOf(t, occupied.Addr())

	if err := c.Forward(ctx, local, remotePort); err == nil {
		t.Fatalf("Forward on occupied port %d: want error, got nil", local)
	}
	ports, err := c.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("ListForwards = %v after failed Forward, want empty (registers nothing)", ports)
	}
}

// TestCancelUnknownPortNoop proves Cancel of an unregistered port is a no-op.
func TestCancelUnknownPortNoop(t *testing.T) {
	ctx := context.Background()
	c := newForwardClient(t)
	if err := c.Cancel(ctx, 65000, 65001); err != nil {
		t.Errorf("Cancel of unknown port: want nil, got %v", err)
	}
}

// dialRefused reports whether a dial to 127.0.0.1:port is refused (listener
// closed). A short retry loop absorbs the brief window where the OS has not yet
// reaped the closed listener.
func dialRefused(t *testing.T, port int) bool {
	t.Helper()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
