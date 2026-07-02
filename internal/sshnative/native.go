// Package sshnative implements transport.Transport over golang.org/x/crypto/ssh
// — a self-contained ssh client that depends on neither the system ssh binary
// nor the user's ssh_config. It maintains a single *ssh.Client guarded by a
// mutex, authenticates via the ssh-agent then unencrypted key files (see
// auth.go), verifies the host key STRICTLY against a known_hosts file, and runs
// a keepalive that marks the connection dead so the next Ensure re-dials.
//
// The argv contract is the shell-join model pinned in transport.go: Exec and
// Stream join argv with single ASCII spaces into ONE command string handed to
// an ssh.Session; the remote login shell re-splits it. Callers pre-quote any
// multi-token payload into a single argv element (bootstrap/clipupload/doctor
// already do). Native does not run `sh -c` itself — the remote shell that the
// ssh server spawns for the exec request is the splitter.
//
// Injection seams (Options) exist so tests point the client at temp-dir
// fixtures instead of the runner's real ~/.ssh; production (the u5 factory)
// calls New(host) with NO options so the real ~/.ssh defaults apply. See
// New and the four With* helpers.
package sshnative

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

const (
	defaultPort      = 22
	dialTimeout      = 15 * time.Second
	keepalivePeriod  = 15 * time.Second
	keepaliveStrikes = 3
)

// Option mutates a Client's resolved configuration before Ensure dials. New
// applies the real ~/.ssh defaults first, then the Options, so an Option always
// wins over the default it overrides.
type Option func(*Client)

// WithKnownHostsPath overrides the known_hosts file used to build the STRICT
// HostKeyCallback (default ~/.ssh/known_hosts). Ignored when
// WithHostKeyCallback is also supplied.
func WithKnownHostsPath(path string) Option {
	return func(c *Client) { c.knownHostsPath = path }
}

// WithIdentityFiles overrides the private-key candidate list (default
// ~/.ssh/id_ed25519 then ~/.ssh/id_rsa).
func WithIdentityFiles(paths ...string) Option {
	return func(c *Client) { c.identityFiles = append([]string(nil), paths...) }
}

// WithAgentSocket overrides the ssh-agent socket (default $SSH_AUTH_SOCK). The
// EMPTY string DISABLES agent auth entirely — auth then relies only on identity
// files.
func WithAgentSocket(sock string) Option {
	return func(c *Client) { c.agentSocket = sock }
}

// WithHostKeyCallback is a test escape hatch that REPLACES the known_hosts-file
// construction wholesale: when supplied, the resolved known_hosts path is not
// read and cb is used verbatim as the HostKeyCallback.
func WithHostKeyCallback(cb ssh.HostKeyCallback) Option {
	return func(c *Client) { c.hostKeyOverride = cb }
}

// Client is one native ssh transport bound to a single target. The embedded
// *ssh.Client is guarded by mu; a nil or dead client makes the next Ensure
// re-dial.
type Client struct {
	user string
	host string
	port int

	// Resolved auth/knownhosts config: defaults from New, overridable by Options.
	knownHostsPath  string
	identityFiles   []string
	agentSocket     string
	hostKeyOverride ssh.HostKeyCallback

	// hostKeyCB is the built STRICT callback (from knownHostsPath, or the
	// override). Populated by New so Ensure never touches ~/.ssh again.
	hostKeyCB ssh.HostKeyCallback

	mu            sync.Mutex
	client        *ssh.Client
	dead          bool
	agentConn     net.Conn      // held open for the client's lifetime; closed on redial/Close
	keepaliveStop chan struct{} // closed to stop the current keepalive goroutine
}

var _ transport.Transport = (*Client)(nil)

// New parses target (user@host[:port], default port 22), applies opts, resolves
// the effective known_hosts path / identity-file list / agent socket from the
// real ~/.ssh defaults (overridden by opts), and builds the STRICT host-key
// callback. It does NOT dial: it returns a ready-to-Ensure Client so the u5
// factory and doctor's daemon-down fallback can construct native cheaply
// without a live box. ssh_config alias resolution is out of scope.
func New(target string, opts ...Option) (*Client, error) {
	user, host, port, err := parseTarget(target)
	if err != nil {
		return nil, err
	}
	c := &Client{user: user, host: host, port: port}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("sshnative: resolve home dir for ~/.ssh defaults: %w", err)
	}
	c.knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	c.identityFiles = []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
	}
	c.agentSocket = os.Getenv("SSH_AUTH_SOCK")

	for _, o := range opts {
		o(c)
	}

	cb, err := c.buildHostKeyCallback()
	if err != nil {
		return nil, err
	}
	c.hostKeyCB = cb
	return c, nil
}

// parseTarget splits user@host[:port]. A missing user is an error naming the
// accepted form. Missing port defaults to 22.
func parseTarget(target string) (user, host string, port int, err error) {
	at := strings.LastIndex(target, "@")
	if at < 0 {
		return "", "", 0, fmt.Errorf("sshnative: target %q missing user; expected user@host[:port]", target)
	}
	user = target[:at]
	hostport := target[at+1:]
	if user == "" {
		return "", "", 0, fmt.Errorf("sshnative: target %q has empty user; expected user@host[:port]", target)
	}
	if hostport == "" {
		return "", "", 0, fmt.Errorf("sshnative: target %q has empty host; expected user@host[:port]", target)
	}
	// A ':' denotes an explicit port. IPv6 literals (bracketed) also parse via
	// SplitHostPort; a bare IPv6 without a port is out of scope.
	if strings.Contains(hostport, ":") {
		h, p, serr := net.SplitHostPort(hostport)
		if serr != nil {
			return "", "", 0, fmt.Errorf("sshnative: target %q: %v; expected user@host[:port]", target, serr)
		}
		n, aerr := strconv.Atoi(p)
		if aerr != nil || n <= 0 || n > 65535 {
			return "", "", 0, fmt.Errorf("sshnative: target %q has invalid port %q", target, p)
		}
		return user, h, n, nil
	}
	return user, hostport, defaultPort, nil
}

// buildHostKeyCallback constructs the STRICT host-key verifier. WithHostKeyCallback
// replaces this wholesale; otherwise it is built from the resolved known_hosts
// path. A missing known_hosts file is treated as "no known hosts" so every host
// is unknown (strict) — New still succeeds and the failure surfaces at Ensure
// with the remediation hint, never a raw file-not-found.
func (c *Client) buildHostKeyCallback() (ssh.HostKeyCallback, error) {
	if c.hostKeyOverride != nil {
		return c.hostKeyOverride, nil
	}
	base, err := knownhosts.New(c.knownHostsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			base = func(string, net.Addr, ssh.PublicKey) error {
				return fmt.Errorf("no known_hosts entry (%s does not exist)", c.knownHostsPath)
			}
		} else {
			return nil, fmt.Errorf("sshnative: load known_hosts %s: %w", c.knownHostsPath, err)
		}
	}
	return c.strictHostKey(base), nil
}

// strictHostKey wraps base so an unknown OR mismatched key aborts the dial with
// an error that CONTAINS the host and a remediation hint (run `ssh <host>` once
// manually to record the correct host key).
func (c *Client) strictHostKey(base ssh.HostKeyCallback) ssh.HostKeyCallback {
	host := c.host
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := base(hostname, remote, key); err != nil {
			return fmt.Errorf("host key verification failed for %s: %w; run `ssh %s` once manually to record the host key", host, err, host)
		}
		return nil
	}
}

// Ensure dials when no live client exists (or the previous one is dead).
// rebuilt is true iff THIS call dialed. On dial failure it returns the wrapped
// error and leaves the client down.
func (c *Client) Ensure(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil && !c.dead {
		return false, nil
	}
	// Tear down a dead client and its keepalive/agent before re-dialing.
	c.teardownLocked()

	auths, agentConn, err := c.authMethods()
	if err != nil {
		return false, err
	}
	cfg := &ssh.ClientConfig{
		User:            c.user,
		Auth:            auths,
		HostKeyCallback: c.hostKeyCB,
		Timeout:         dialTimeout,
	}
	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		if agentConn != nil {
			agentConn.Close()
		}
		return false, fmt.Errorf("sshnative: dial %s: %w", addr, err)
	}
	c.client = client
	c.dead = false
	c.agentConn = agentConn
	c.startKeepaliveLocked(client)
	return true, nil
}

// teardownLocked releases the current client, keepalive goroutine, and agent
// connection. Caller holds mu.
func (c *Client) teardownLocked() {
	if c.keepaliveStop != nil {
		close(c.keepaliveStop)
		c.keepaliveStop = nil
	}
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
	if c.agentConn != nil {
		c.agentConn.Close()
		c.agentConn = nil
	}
}

// startKeepaliveLocked launches a goroutine that sends keepalive@openssh.com
// global requests every keepalivePeriod; keepaliveStrikes consecutive failures
// mark the client dead so the next Ensure re-dials. Caller holds mu.
func (c *Client) startKeepaliveLocked(client *ssh.Client) {
	stop := make(chan struct{})
	c.keepaliveStop = stop
	go func() {
		ticker := time.NewTicker(keepalivePeriod)
		defer ticker.Stop()
		strikes := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					strikes++
					if strikes >= keepaliveStrikes {
						c.markDead(client)
						return
					}
					continue
				}
				strikes = 0
			}
		}
	}()
}

// markDead flags the connection dead if client is still the current one, so the
// next Ensure re-dials. It does not close the client — Ensure/Close own that.
func (c *Client) markDead(client *ssh.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == client {
		c.dead = true
	}
}

// Health reports liveness. Pid is ALWAYS 0 for native — there is no pid ground
// truth for an in-process connection.
func (c *Client) Health(ctx context.Context) (transport.Health, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	up := c.client != nil && !c.dead
	detail := "disconnected"
	if up {
		detail = "connected"
	}
	return transport.Health{Up: up, Pid: 0, Detail: detail}, nil
}

// liveClient returns a connected *ssh.Client, dialing via Ensure if needed.
func (c *Client) liveClient(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	if c.client != nil && !c.dead {
		cl := c.client
		c.mu.Unlock()
		return cl, nil
	}
	c.mu.Unlock()

	if _, err := c.Ensure(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	cl := c.client
	c.mu.Unlock()
	if cl == nil {
		return nil, errors.New("sshnative: no live client after Ensure")
	}
	return cl, nil
}

// Exec opens a session, runs the space-joined argv, feeds stdin, and captures
// stdout/stderr. A non-zero remote exit maps to an error carrying the code and
// trimmed stderr; stdout/stderr are still returned.
func (c *Client) Exec(ctx context.Context, stdin []byte, argv ...string) (string, string, error) {
	client, err := c.liveClient(ctx)
	if err != nil {
		return "", "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", "", fmt.Errorf("sshnative: new session: %w", err)
	}
	defer sess.Close()

	if len(stdin) > 0 {
		sess.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	sess.Stdout = &outBuf
	sess.Stderr = &errBuf

	runErr := sess.Run(strings.Join(argv, " "))
	stdout, stderr := outBuf.String(), errBuf.String()
	if runErr == nil {
		return stdout, stderr, nil
	}
	var ee *ssh.ExitError
	if errors.As(runErr, &ee) {
		return stdout, stderr, fmt.Errorf("sshnative exit %d: %s", ee.ExitStatus(), strings.TrimSpace(stderr))
	}
	return stdout, stderr, fmt.Errorf("sshnative: exec: %w", runErr)
}

// Stream opens a session on the space-joined argv with live stdin/stdout/stderr
// pipes; wait returns the remote command's exit error after the streams close
// (exactly today's ExecStream semantics agentclient depends on).
func (c *Client) Stream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	client, err := c.liveClient(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("sshnative: new session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, nil, nil, nil, fmt.Errorf("sshnative: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, nil, nil, nil, fmt.Errorf("sshnative: stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return nil, nil, nil, nil, fmt.Errorf("sshnative: stderr pipe: %w", err)
	}
	if err := sess.Start(strings.Join(argv, " ")); err != nil {
		sess.Close()
		return nil, nil, nil, nil, fmt.Errorf("sshnative: start: %w", err)
	}
	wait := func() error {
		werr := sess.Wait()
		sess.Close()
		return werr
	}
	// ssh.Session pipes are io.Reader; adapt to io.ReadCloser (session Close, in
	// wait, tears the underlying channel down — the reader Close is a no-op).
	return stdin, nopReadCloser{stdout}, nopReadCloser{stderr}, wait, nil
}

// nopReadCloser adapts the ssh.Session pipe io.Readers to io.ReadCloser.
type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

// Close closes the client. stopped is true iff there was a live client to stop.
func (c *Client) Close(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	had := c.client != nil
	c.teardownLocked()
	c.dead = false
	return had, nil
}

// Describe returns identifying metadata; Impl is always "native-ssh".
func (c *Client) Describe() transport.Desc {
	return transport.Desc{
		Impl:     "native-ssh",
		Host:     c.host,
		Endpoint: fmt.Sprintf("%s@%s:%d", c.user, c.host, c.port),
	}
}
