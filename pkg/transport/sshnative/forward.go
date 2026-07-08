package sshnative

// This file implements the optional transport.PortForwarder capability for the
// native ssh client. Each Forward stands up a LOCAL net.Listener on
// 127.0.0.1:<local> and, per inbound connection, opens an ssh direct-tcpip
// channel to localhost:<remote> on the live client, then copies bytes
// bidirectionally. The in-process registry (Client.forwards, guarded by
// Client.fwdMu) is the GROUND TRUTH for ListForwards/ForwardLines — more
// truthful than lsof because it is exactly the set of listeners this process
// owns.
//
// FAILURE-MODE HONESTY (T10): native forwards have NO ControlPersist analogue.
// They live only as long as this process (and its listeners) do — Close and
// daemon exit drop every forward immediately. doctor surfaces this when native
// is the active transport (u5).

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

var _ transport.PortForwarder = (*Client)(nil)

// Forward stands up a local listener on 127.0.0.1:<local>, registers it, and
// starts an accept goroutine that opens an ssh direct-tcpip channel to
// localhost:<remote> for each inbound connection. A listen failure returns the
// error and registers nothing.
func (c *Client) Forward(ctx context.Context, local, remote int) error {
	// Fail fast if the transport can't come up, so Forward doesn't register a
	// listener whose connections could never reach the remote.
	if _, err := c.liveClient(ctx); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(local))
	if err != nil {
		return fmt.Errorf("sshnative: listen 127.0.0.1:%d: %w", local, err)
	}

	c.fwdMu.Lock()
	if c.forwards == nil {
		c.forwards = make(map[int]net.Listener)
	}
	c.forwards[local] = ln
	c.fwdMu.Unlock()

	go c.acceptForwards(ln, remote)
	return nil
}

// acceptForwards runs until ln is closed (by Cancel or Close), spawning a
// handler per inbound connection.
func (c *Client) acceptForwards(ln net.Listener, remote int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed: Cancel/Close removed this forward
		}
		go c.handleForwardConn(conn, remote)
	}
}

// handleForwardConn opens a direct-tcpip channel to localhost:<remote> on the
// current live client and copies bytes bidirectionally with TCP half-close
// semantics: when one direction reaches EOF it half-closes ONLY that direction
// (so the peer observes EOF on its read side) and leaves the reverse direction
// open. Both endpoints are fully closed (via the defers) only after BOTH copies
// finish. This matches what system-ssh forwarding does and is REQUIRED for
// request/response protocols where a client shutdown(SHUT_WR)s to signal
// end-of-request and then reads the reply: full-closing on the first EOF would
// drop the reply before the remote service writes it.
func (c *Client) handleForwardConn(conn net.Conn, remote int) {
	defer conn.Close()

	cl := c.currentClient()
	if cl == nil {
		return // connection lost; drop this conn (native forwards die with it)
	}
	ch, err := cl.Dial("tcp", net.JoinHostPort("localhost", strconv.Itoa(remote)))
	if err != nil {
		return
	}
	defer ch.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(ch, conn)  // client -> remote
		halfCloseWrite(ch) // client FIN -> remote sees EOF (reverse stays open)
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn, ch)    // remote -> client
		halfCloseWrite(conn) // remote EOF -> client sees FIN (forward stays open)
	}()
	wg.Wait()
}

// halfCloseWrite shuts down only the write half of c when it supports it (both
// *net.TCPConn and the ssh direct-tcpip channel do), so the peer observes EOF on
// its read side while the reverse direction keeps flowing. A conn without
// CloseWrite is left untouched — the deferred full Close still tears it down.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// currentClient returns the live *ssh.Client, or nil if the connection is down
// or marked dead. It never dials (forward handlers must not block on a redial).
func (c *Client) currentClient() *ssh.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return nil
	}
	return c.client
}

// Cancel closes and deregisters the listener for local (best-effort). An
// unknown port is a no-op. Closing the listener stops its accept goroutine.
func (c *Client) Cancel(ctx context.Context, local, remote int) error {
	c.fwdMu.Lock()
	ln, ok := c.forwards[local]
	if ok {
		delete(c.forwards, local)
	}
	c.fwdMu.Unlock()
	if !ok {
		return nil
	}
	return ln.Close()
}

// closeAllForwards closes and deregisters every listener. Called by Close (and
// implicitly on daemon exit) because native forwards do not persist.
func (c *Client) closeAllForwards() {
	c.fwdMu.Lock()
	for local, ln := range c.forwards {
		ln.Close()
		delete(c.forwards, local)
	}
	c.fwdMu.Unlock()
}

// ListForwards returns the sorted-unique local ports currently registered —
// the in-process registry, not lsof.
func (c *Client) ListForwards(ctx context.Context) ([]int, error) {
	c.fwdMu.Lock()
	ports := make([]int, 0, len(c.forwards))
	for local := range c.forwards {
		ports = append(ports, local)
	}
	c.fwdMu.Unlock()
	sort.Ints(ports)
	return ports, nil
}

// ForwardLines returns one 127.0.0.1:<local> line per registered listener,
// sort-unique by string. The shape parallels the lsof NAME lines the status
// renderer prints; this is the exact shape the conformance loopback echo
// section asserts.
func (c *Client) ForwardLines(ctx context.Context) ([]string, error) {
	c.fwdMu.Lock()
	lines := make([]string, 0, len(c.forwards))
	for local := range c.forwards {
		lines = append(lines, fmt.Sprintf("127.0.0.1:%d", local))
	}
	c.fwdMu.Unlock()
	sort.Strings(lines)
	return lines, nil
}
