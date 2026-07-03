package sshnative

// This file implements the T12 ProxyJump / ProxyCommand dialing for the native
// ssh client. ProxyJump builds the connection through the resolved hop chain
// with NO ssh binary: net.Dial to the first hop, then a chained direct-tcpip
// channel to each subsequent hop (and finally the target) as the next net.Conn.
// Each hop is resolved by the SAME ConfigResolver; expandJumpChain is a
// depth-first LEFT-EXPANSION — a hop that itself carries a ProxyJump is reached
// through its own jumps FIRST (matching OpenSSH) — under a per-branch
// ancestor-path cycle guard + hop-cap whose termination is guaranteed: the
// ancestor path rejects a hop that is its own ancestor (a true loop) while
// allowing a hop shared by two sibling branches, and the cap counts every hop
// occurrence before recursive descent so a runaway resolver cannot recurse or
// dial without bound. Both abort with nothing dialed. Every hop enforces STRICT
// host-key verification keyed by the RAW net.JoinHostPort query address — the
// same locked mechanic New uses for the target: net.JoinHostPort(alias,"22")
// when the hop has a HostKeyAlias, else net.JoinHostPort(host,port); NEVER a
// knownhosts.Normalize'd (port-less) string, which knownhosts.check rejects. A
// wrong hop key aborts the WHOLE dial. ProxyJump WINS when both directives are
// set (Ensure's dispatch), and the case-sensitive `none` sentinel disables
// either directive (direct dial). ProxyCommand execs `sh -c` with %h/%p/%r
// expansion behind an injectable seam; expandProxyCommand treats %% as a literal
// % and errors on a dangling lone % or any unsupported %X token, all BEFORE the
// seam is invoked. The whole chain — jump clients, per-hop agent conns, and the
// ProxyCommand process — dies on Close/redial (teardownLocked, LIFO): native has
// NO ControlPersist analogue, so nothing outlives the client.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// maxProxyHops caps total ProxyJump hop occurrences in the flattened chain,
	// counted before recursive descent, so flat and nested configs cannot dial or
	// recurse without bound.
	maxProxyHops = 10
	// proxyCommandStderrLimit caps helper diagnostics so a chatty or looping
	// ProxyCommand cannot grow memory without bound; the tail is kept because the
	// useful failure detail is usually the last thing printed.
	proxyCommandStderrLimit = 4 * 1024
)

// proxyCommandDialer execs a ProxyCommand and adapts its stdio to a net.Conn.
// It is an injectable seam (WithProxyCommandDialer) so the ProxyCommand round
// trip is testable over an in-process pipe with no real subprocess. The returned
// io.Closer tears the process (or fake) down on Close/redial.
type proxyCommandDialer func(ctx context.Context, command string) (net.Conn, io.Closer, error)

// WithProxyCommandDialer overrides the seam used to exec a ProxyCommand. When
// unset, New installs defaultProxyCommandDialer (real `sh -c` subprocess).
func WithProxyCommandDialer(d proxyCommandDialer) Option {
	return func(c *Client) { c.proxyCommandDialer = d }
}

// splitJumpList splits a ProxyJump value on commas, trimming spaces and dropping
// empty tokens.
func splitJumpList(s string) []string {
	var out []string
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// portOr22 returns 22 when p is the unspecified port (0), else p.
func portOr22(p int) int {
	if p == 0 {
		return defaultPort
	}
	return p
}

// expandJumpChain builds the FLATTENED hop chain in dial order (first-dialed
// first) by depth-first LEFT-EXPANSION of ProxyJump: each token is resolved by
// the SAME ConfigResolver, and if the resolved hop carries its OWN ProxyJump it
// is expanded (and thus dialed) FIRST — matching OpenSSH, where a hop `a` with
// `ProxyJump x` reaches x before a. Cycle detection tracks the ANCESTOR PATH
// currently being expanded (per branch), not a global set of every hop ever
// appended: a hop reached independently by two sibling branches is NOT a cycle
// (OpenSSH dials it once per branch through its own connection context — e.g.
// `target -> a,b` with `a -> bastion` and `b -> bastion` flattens to
// [bastion,a,bastion,b], each direct-tcpip hop reaching the next), whereas a hop
// that is its own ancestor IS a cycle and aborts. The hop-cap counts every token
// occurrence at loop entry, before resolving it or descending into its own
// ProxyJump, so flat and nested chains longer than maxProxyHops fail before any
// dial and before recursion can grow past the cap. Both return a clear error and
// leave nothing dialed.
func (c *Client) expandJumpChain(ctx context.Context) ([]ResolvedHost, error) {
	var chain []ResolvedHost
	resolvedHops := 0
	var expand func(jump string, ancestors map[string]bool) error
	expand = func(jump string, ancestors map[string]bool) error {
		for _, token := range splitJumpList(jump) {
			if ancestors[token] {
				return fmt.Errorf("sshnative: proxyjump cycle at %q", token)
			}
			resolvedHops++
			if resolvedHops > maxProxyHops {
				return fmt.Errorf("sshnative: proxyjump exceeds %d hops", maxProxyHops)
			}
			rh, err := c.resolver(ctx, token)
			if err != nil {
				return fmt.Errorf("sshnative: resolve proxyjump hop %q: %w", token, err)
			}
			if rh.ProxyCommand != "" && rh.ProxyCommand != "none" {
				return fmt.Errorf("sshnative: proxyjump hop %q uses ProxyCommand, which native does not support for hops", token)
			}
			if rh.ProxyJump != "" && rh.ProxyJump != "none" {
				// Mark this token as an ancestor ONLY while its own ProxyJump is
				// expanded, then unmark: a later sibling branch may legitimately
				// reach the same hop again without it being a cycle.
				ancestors[token] = true
				if err := expand(rh.ProxyJump, ancestors); err != nil {
					return err
				}
				delete(ancestors, token)
			}
			chain = append(chain, rh)
		}
		return nil
	}
	if err := expand(c.proxyJump, make(map[string]bool)); err != nil {
		return nil, err
	}
	return chain, nil
}

// hopHostKeyCallback builds the STRICT per-hop host-key verifier keyed by the
// hop's HostKeyAlias:22 or HostName:port, defaulting an unspecified hop port to
// 22 via portOr22 so the known_hosts query address matches the dial address
// dialViaProxyJump forms on the very next lines — a port-less hop must query
// host:22 (which collapses to the bare-host entry a real :22 server records),
// never host:0 (which knownhosts.check rejects). Mirrors New's target path,
// which defaults c.port to 22 before building its callback.
func (c *Client) hopHostKeyCallback(rh ResolvedHost) (ssh.HostKeyCallback, error) {
	return newStrictHostKeyCallback(c.knownHostsPath, c.hostKeyOverride, rh.HostKeyAlias, rh.HostName, portOr22(rh.Port))
}

// dialViaProxyJump dials the target through the resolved hop chain via chained
// direct-tcpip channels. It returns the target client, the ordered jump clients,
// and any agent connections opened for hop auth. On ANY error mid-chain it closes
// everything opened so far (jump clients in reverse, agent conns) and returns the
// error with nothing stored.
func (c *Client) dialViaProxyJump(ctx context.Context, targetCfg *ssh.ClientConfig) (*ssh.Client, []*ssh.Client, []net.Conn, error) {
	chain, err := c.expandJumpChain(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	var jumps []*ssh.Client
	var agentConns []net.Conn
	fail := func(err error) (*ssh.Client, []*ssh.Client, []net.Conn, error) {
		for i := len(jumps) - 1; i >= 0; i-- {
			jumps[i].Close()
		}
		for _, ac := range agentConns {
			if ac != nil {
				ac.Close()
			}
		}
		return nil, nil, nil, err
	}

	var prev *ssh.Client
	for _, rh := range chain {
		// Per-hop strict host-key check keyed by the raw net.JoinHostPort query
		// address (u7 mechanic): each hop is verified by its own HostKeyAlias:22
		// or HostName:port, never a Normalize'd value, so a wrong hop key aborts
		// the WHOLE dial.
		hopCB, err := c.hopHostKeyCallback(rh)
		if err != nil {
			return fail(err)
		}
		auths, agentConn, err := c.hopAuthMethods(rh)
		if err != nil {
			return fail(err)
		}
		agentConns = append(agentConns, agentConn)
		hopCfg := &ssh.ClientConfig{
			User:            rh.User,
			Auth:            auths,
			HostKeyCallback: hopCB,
			Timeout:         dialTimeout,
		}
		hopAddr := net.JoinHostPort(rh.HostName, strconv.Itoa(portOr22(rh.Port)))
		var raw net.Conn
		if prev == nil {
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			raw, err = (&net.Dialer{Timeout: dialTimeout}).DialContext(dialCtx, "tcp", hopAddr)
			cancel()
		} else {
			// A direct-tcpip channel from the current jump client is the next
			// hop's underlying net.Conn.
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			raw, err = prev.DialContext(dialCtx, "tcp", hopAddr)
			cancel()
		}
		if err != nil {
			return fail(fmt.Errorf("sshnative: dial proxyjump hop %s: %w", hopAddr, err))
		}
		conn, chans, reqs, err := newClientConnWithWatchdog(ctx, raw, hopAddr, hopCfg)
		if err != nil {
			raw.Close()
			return fail(fmt.Errorf("sshnative: handshake proxyjump hop %s: %w", hopAddr, err))
		}
		cl := ssh.NewClient(conn, chans, reqs)
		jumps = append(jumps, cl)
		prev = cl
	}

	// Reach the TARGET through the last jump. targetCfg already carries the
	// target's strict host-key callback (built by New), so the target is verified
	// with the same raw-JoinHostPort mechanic.
	targetAddr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	raw, err := prev.DialContext(dialCtx, "tcp", targetAddr)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("sshnative: dial target %s via proxyjump: %w", targetAddr, err))
	}
	conn, chans, reqs, err := newClientConnWithWatchdog(ctx, raw, targetAddr, targetCfg)
	if err != nil {
		raw.Close()
		return fail(fmt.Errorf("sshnative: handshake target %s via proxyjump: %w", targetAddr, err))
	}
	target := ssh.NewClient(conn, chans, reqs)
	return target, jumps, agentConns, nil
}

// dialViaProxyCommand execs the resolved ProxyCommand (via the seam) and hands
// the target ssh handshake its stdio net.Conn. The token expansion runs BEFORE
// anything is exec'd, so an unsupported/dangling token fails without spawning a
// process. On handshake failure the conn and the process closer are both closed.
func (c *Client) dialViaProxyCommand(ctx context.Context, targetCfg *ssh.ClientConfig) (*ssh.Client, io.Closer, error) {
	cmdStr, err := expandProxyCommand(c.proxyCommand, c.host, c.port, c.user)
	if err != nil {
		return nil, nil, err
	}
	conn, closer, err := c.proxyCommandDialer(ctx, cmdStr)
	if err != nil {
		return nil, nil, fmt.Errorf("sshnative: proxycommand dial: %w", err)
	}
	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	sshConn, chans, reqs, err := newClientConnWithWatchdog(ctx, conn, addr, targetCfg)
	if err != nil {
		conn.Close()
		if closer != nil {
			closer.Close()
		}
		stderr := proxyCommandStderr(closer)
		if stderr != "" {
			return nil, nil, fmt.Errorf("sshnative: proxycommand handshake %s: %w: %s", addr, err, stderr)
		}
		return nil, nil, fmt.Errorf("sshnative: proxycommand handshake %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), closer, nil
}

// newClientConnWithWatchdog bounds ssh.NewClientConn for conns whose deadline
// methods are ineffective (notably ProxyCommand stdio and direct-tcpip channels).
// Closing the underlying net.Conn is the only portable way to unblock x/crypto's
// handshake when the peer accepts but never speaks SSH.
func newClientConnWithWatchdog(ctx context.Context, raw net.Conn, addr string, cfg *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	done := make(chan struct{})
	watchdogDone := make(chan struct{})
	causeCh := make(chan error, 1)
	timer := time.NewTimer(dialTimeout)

	go func() {
		defer close(watchdogDone)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			causeCh <- ctx.Err()
			raw.Close()
		case <-timer.C:
			causeCh <- fmt.Errorf("handshake timeout after %s", dialTimeout)
			raw.Close()
		case <-done:
		}
	}()

	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	close(done)
	<-watchdogDone
	if err != nil {
		select {
		case cause := <-causeCh:
			if cause != nil {
				return nil, nil, nil, fmt.Errorf("%w: %v", cause, err)
			}
		default:
		}
		return nil, nil, nil, err
	}
	select {
	case cause := <-causeCh:
		if cause != nil {
			conn.Close()
			return nil, nil, nil, cause
		}
	default:
	}
	return conn, chans, reqs, nil
}

// expandProxyCommand expands an OpenSSH ProxyCommand template in a single
// left-to-right scan: %% -> %, %h -> host, %p -> port, %r -> user. A dangling
// lone % at end of the template errors (so the %% escape cannot swallow a
// malformed trailing %), as does any other unsupported %X token.
func expandProxyCommand(tmpl, host string, port int, user string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tmpl); i++ {
		ch := tmpl[i]
		if ch != '%' {
			b.WriteByte(ch)
			continue
		}
		if i+1 >= len(tmpl) {
			return "", fmt.Errorf("sshnative: ProxyCommand has a dangling %% at end of %q", tmpl)
		}
		i++
		switch tmpl[i] {
		case '%':
			b.WriteByte('%')
		case 'h':
			b.WriteString(host)
		case 'p':
			b.WriteString(strconv.Itoa(port))
		case 'r':
			b.WriteString(user)
		default:
			return "", fmt.Errorf("sshnative: unsupported ProxyCommand token %%%c", tmpl[i])
		}
	}
	return b.String(), nil
}

// defaultProxyCommandDialer execs the ProxyCommand via `sh -c`, wiring its
// stdin/stdout into a stdioConn net.Conn. The returned io.Closer kills the
// process and Waits it (process-exit teardown). os/exec is stdlib — no new
// dependency.
func defaultProxyCommandDialer(ctx context.Context, command string) (net.Conn, io.Closer, error) {
	// The subprocess must OUTLIVE the dial ctx: the ssh.Client built on its stdio
	// is stored in c.client and PERSISTS across calls (guarded by mu, kept alive by
	// the keepalive goroutine), reaped only by proxyProcessCloser.Close at
	// teardownLocked on Close/redial. exec.CommandContext installs a watchdog that
	// SIGKILLs the process the instant its context is Done, so binding it to the
	// per-call dial ctx would asynchronously tear down the still-live master the
	// moment the triggering caller's ctx is cancelled/deadlined. WithoutCancel
	// decouples the process lifetime from ctx (ctx still bounds only this dial via
	// the caller), matching the direct-dial and ProxyJump paths, neither of which
	// ties the persistent connection to ctx.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "sh", "-c", command)
	stderr := newLockedCappedBuffer(proxyCommandStderrLimit)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sshnative: proxycommand stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sshnative: proxycommand stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("sshnative: proxycommand start: %w", err)
	}
	return &stdioConn{stdout: stdout, stdin: stdin}, proxyProcessCloser{cmd: cmd, stderr: stderr}, nil
}

// proxyProcessCloser kills and reaps the ProxyCommand subprocess. Close is the
// process-exit teardown invoked from teardownLocked on Close/redial.
type proxyProcessCloser struct {
	cmd    *exec.Cmd
	stderr *lockedCappedBuffer
}

func (p proxyProcessCloser) Close() error {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}

func (p proxyProcessCloser) proxyCommandStderr() string {
	if p.stderr == nil {
		return ""
	}
	return strings.TrimSpace(p.stderr.String())
}

type proxyCommandStderrProvider interface {
	proxyCommandStderr() string
}

func proxyCommandStderr(closer io.Closer) string {
	provider, ok := closer.(proxyCommandStderrProvider)
	if !ok {
		return ""
	}
	return provider.proxyCommandStderr()
}

// lockedCappedBuffer keeps only the stderr tail. os/exec may write to Stderr
// from a copier goroutine while Close/Wait is still in progress, so readers and
// writers synchronize even though dialViaProxyCommand reads after Close returns.
type lockedCappedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newLockedCappedBuffer(limit int) *lockedCappedBuffer {
	return &lockedCappedBuffer{limit: limit}
}

func (b *lockedCappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		copy(b.buf, b.buf[over:])
		b.buf = b.buf[:b.limit]
	}
	return len(p), nil
}

func (b *lockedCappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.buf...))
}

// stdioConn adapts a subprocess's stdout (Read) and stdin (Write) to a net.Conn
// for the ssh handshake. Deadlines are best-effort no-ops (a pipe has none);
// CloseWrite closes stdin so the peer observes EOF on its read side.
//
// LocalAddr/RemoteAddr MUST return an addr whose String() splits into host:port:
// x/crypto's knownhosts.check runs net.SplitHostPort(remote.String()) as its FIRST
// step and returns that error before it ever consults the (correct) lookup-key
// address, so an unsplittable remote (e.g. String()=="stdio") fails STRICT
// host-key verification for EVERY ProxyCommand target even when the key is
// recorded. A zero *net.TCPAddr (String()=="0.0.0.0:0") is splittable and mirrors
// exactly what x/crypto's own direct-tcpip chanConn returns for the ProxyJump
// path, so verification reaches the lookup-key preference and succeeds.
type stdioConn struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
}

func (s *stdioConn) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *stdioConn) Write(p []byte) (int, error) { return s.stdin.Write(p) }

func (s *stdioConn) Close() error {
	werr := s.stdin.Close()
	rerr := s.stdout.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

func (s *stdioConn) CloseWrite() error { return s.stdin.Close() }

// stdioZeroAddr is the placeholder remote/local addr for a stdio-backed
// ProxyCommand conn. Its String() ("0.0.0.0:0") is net.SplitHostPort-splittable,
// which knownhosts.check requires before it consults the real lookup-key address;
// it mirrors x/crypto's own chanConn zero addr on the ProxyJump path.
var stdioZeroAddr net.Addr = &net.TCPAddr{}

func (s *stdioConn) LocalAddr() net.Addr                { return stdioZeroAddr }
func (s *stdioConn) RemoteAddr() net.Addr               { return stdioZeroAddr }
func (s *stdioConn) SetDeadline(t time.Time) error      { return nil }
func (s *stdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (s *stdioConn) SetWriteDeadline(t time.Time) error { return nil }
