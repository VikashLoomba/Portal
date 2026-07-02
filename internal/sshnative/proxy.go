package sshnative

// This file implements the T12 ProxyJump / ProxyCommand dialing for the native
// ssh client. ProxyJump builds the connection through the resolved hop chain
// with NO ssh binary: net.Dial to the first hop, then a chained direct-tcpip
// channel to each subsequent hop (and finally the target) as the next net.Conn.
// Each hop is resolved by the SAME ConfigResolver; expandJumpChain is a
// depth-first LEFT-EXPANSION — a hop that itself carries a ProxyJump is reached
// through its own jumps FIRST (matching OpenSSH) — under a visited-set + hop-cap
// guard whose termination is guaranteed: the visited set rejects any repeated
// token (including a hop that loops back on itself) and the cap bounds a runaway
// resolver, both aborting with nothing dialed. Every hop enforces STRICT
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
	"time"

	"golang.org/x/crypto/ssh"
)

// maxProxyHops caps the flattened ProxyJump chain length so a runaway config
// (or a resolver that keeps chaining) cannot dial without bound.
const maxProxyHops = 10

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
// `ProxyJump x` reaches x before a. The visited-set (keyed by token) rejects
// cycles (including a hop whose own ProxyJump loops back), and the hop-cap
// bounds runaway chains; both return a clear error and leave nothing dialed.
func (c *Client) expandJumpChain(ctx context.Context) ([]ResolvedHost, error) {
	var chain []ResolvedHost
	visited := make(map[string]bool)
	var expand func(jump string) error
	expand = func(jump string) error {
		for _, token := range splitJumpList(jump) {
			if visited[token] {
				return fmt.Errorf("sshnative: proxyjump cycle at %q", token)
			}
			visited[token] = true
			if len(chain) >= maxProxyHops {
				return fmt.Errorf("sshnative: proxyjump exceeds %d hops", maxProxyHops)
			}
			rh, err := c.resolver(ctx, token)
			if err != nil {
				return fmt.Errorf("sshnative: resolve proxyjump hop %q: %w", token, err)
			}
			if rh.ProxyJump != "" && rh.ProxyJump != "none" {
				if err := expand(rh.ProxyJump); err != nil {
					return err
				}
			}
			chain = append(chain, rh)
		}
		return nil
	}
	if err := expand(c.proxyJump); err != nil {
		return nil, err
	}
	return chain, nil
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
		hopCB, err := newStrictHostKeyCallback(c.knownHostsPath, c.hostKeyOverride, rh.HostKeyAlias, rh.HostName, rh.Port)
		if err != nil {
			return fail(err)
		}
		auths, agentConn, err := c.authMethods()
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
			raw, err = net.DialTimeout("tcp", hopAddr, dialTimeout)
		} else {
			// A direct-tcpip channel from the current jump client is the next
			// hop's underlying net.Conn.
			raw, err = prev.Dial("tcp", hopAddr)
		}
		if err != nil {
			return fail(fmt.Errorf("sshnative: dial proxyjump hop %s: %w", hopAddr, err))
		}
		conn, chans, reqs, err := ssh.NewClientConn(raw, hopAddr, hopCfg)
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
	raw, err := prev.Dial("tcp", targetAddr)
	if err != nil {
		return fail(fmt.Errorf("sshnative: dial target %s via proxyjump: %w", targetAddr, err))
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, targetAddr, targetCfg)
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
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, targetCfg)
	if err != nil {
		conn.Close()
		if closer != nil {
			closer.Close()
		}
		return nil, nil, fmt.Errorf("sshnative: proxycommand handshake %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), closer, nil
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
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
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
	return &stdioConn{stdout: stdout, stdin: stdin}, proxyProcessCloser{cmd: cmd}, nil
}

// proxyProcessCloser kills and reaps the ProxyCommand subprocess. Close is the
// process-exit teardown invoked from teardownLocked on Close/redial.
type proxyProcessCloser struct{ cmd *exec.Cmd }

func (p proxyProcessCloser) Close() error {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}

// stdioConn adapts a subprocess's stdout (Read) and stdin (Write) to a net.Conn
// for the ssh handshake. Deadlines are best-effort no-ops (a pipe has none);
// CloseWrite closes stdin so the peer observes EOF on its read side.
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

func (s *stdioConn) LocalAddr() net.Addr                { return stdioAddr{} }
func (s *stdioConn) RemoteAddr() net.Addr               { return stdioAddr{} }
func (s *stdioConn) SetDeadline(t time.Time) error      { return nil }
func (s *stdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (s *stdioConn) SetWriteDeadline(t time.Time) error { return nil }

// stdioAddr is the placeholder net.Addr for a stdio-backed ProxyCommand conn.
type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }
