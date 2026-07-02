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

	"github.com/VikashLoomba/Portal/internal/transport"
)

const (
	defaultPort             = 22
	dialTimeout             = 15 * time.Second
	defaultKeepalivePeriod  = 15 * time.Second
	defaultKeepaliveStrikes = 3
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
// ~/.ssh/id_ed25519 then ~/.ssh/id_rsa). Setting it explicitly also pins the
// list against ssh_config resolution: resolved IdentityFiles never replace an
// explicitly-supplied list.
func WithIdentityFiles(paths ...string) Option {
	return func(c *Client) {
		c.identityFiles = append([]string(nil), paths...)
		c.identityFilesExplicit = true
	}
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

// WithKeepalive overrides the keepalive cadence (default 15s period, 3 strikes).
// It is a test seam: the 15s/3-strike production timing would make dead-connection
// detection take ~45s to observe, so tests shrink the period to drive the real
// startKeepaliveLocked detection loop deterministically. A non-positive period or
// strike count is ignored (the default stands).
func WithKeepalive(period time.Duration, strikes int) Option {
	return func(c *Client) {
		if period > 0 {
			c.keepalivePeriod = period
		}
		if strikes > 0 {
			c.keepaliveStrikes = strikes
		}
	}
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

	// resolver resolves target through ssh_config at New (default
	// DefaultConfigResolver, overridable by WithConfigResolver). It never dials.
	resolver ConfigResolver
	// identityFilesExplicit records that WithIdentityFiles was supplied, so
	// resolved ssh_config IdentityFiles do not replace the explicit list.
	identityFilesExplicit bool
	// hostKeyAlias, when non-empty, keys strict host-key verification instead of
	// the resolved HostName (matching OpenSSH's HostKeyAlias).
	hostKeyAlias string
	// proxyJump/proxyCommand are the resolved proxy directives. Populated at New;
	// CONSUMED only by the T12 dialing (u8) — a direct dial ignores them.
	proxyJump    string
	proxyCommand string

	// Keepalive cadence: defaults from New (15s/3), overridable by WithKeepalive.
	// Read once when startKeepaliveLocked launches; never mutated after New.
	keepalivePeriod  time.Duration
	keepaliveStrikes int

	// hostKeyCB is the built STRICT callback (from knownHostsPath, or the
	// override). Populated by New so Ensure never touches ~/.ssh again.
	hostKeyCB ssh.HostKeyCallback

	mu            sync.Mutex
	client        *ssh.Client
	dead          bool
	agentConn     net.Conn      // held open for the client's lifetime; closed on redial/Close
	keepaliveStop chan struct{} // closed to stop the current keepalive goroutine

	// fwdMu guards forwards, the in-process port-forward registry (keyed by
	// local port). This registry is the GROUND TRUTH for ListForwards/
	// ForwardLines (see forward.go) — more truthful than lsof. Native forwards
	// die with this process: there is no ControlPersist analogue, so Close (or
	// daemon exit) drops every forward. See forward.go.
	fwdMu    sync.Mutex
	forwards map[int]net.Listener
}

var _ transport.Transport = (*Client)(nil)

// New builds a ready-to-Ensure Client for target by RESOLVING it through the
// ConfigResolver (default DefaultConfigResolver, i.e. `ssh -G`) at construction —
// EVERY target is resolved, with no literal short-circuit, so a bare alias, a
// user@alias (honoring its ssh_config Host block), and a raw user@host[:port]
// (which resolves to itself verbatim when no Host block matches) all become the
// resolved dial endpoint. It applies opts first, then resolves, then builds the
// STRICT host-key callback. It does NOT dial, so the u5 factory and doctor's
// daemon-down fallback can construct native cheaply without a live box.
// Describe().Endpoint reports the RESOLVED endpoint; host-key verification keys
// on the resolved HostKeyAlias when set, else the resolved HostName.
func New(target string, opts ...Option) (*Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("sshnative: resolve home dir for ~/.ssh defaults: %w", err)
	}
	c := &Client{}
	c.knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	c.identityFiles = []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
	}
	c.agentSocket = os.Getenv("SSH_AUTH_SOCK")
	c.keepalivePeriod = defaultKeepalivePeriod
	c.keepaliveStrikes = defaultKeepaliveStrikes

	for _, o := range opts {
		o(c)
	}
	if c.resolver == nil {
		c.resolver = DefaultConfigResolver()
	}

	rh, rerr := c.resolver(context.Background(), target)
	if rerr != nil {
		return nil, fmt.Errorf("sshnative: resolve %q: %w", target, rerr)
	}
	if rh.HostName == "" {
		return nil, fmt.Errorf("sshnative: resolved host for %q is empty; not a native target", target)
	}
	c.user = rh.User
	c.host = rh.HostName
	c.port = rh.Port
	if c.port == 0 {
		c.port = defaultPort
	}
	c.hostKeyAlias = rh.HostKeyAlias
	c.proxyJump = rh.ProxyJump
	c.proxyCommand = rh.ProxyCommand

	// Resolved IdentityFiles (existing files only) REPLACE the id_ed25519/id_rsa
	// defaults; an explicit WithIdentityFiles still wins.
	if !c.identityFilesExplicit {
		var filtered []string
		for _, path := range rh.IdentityFiles {
			if _, err := os.Stat(path); err == nil {
				filtered = append(filtered, path)
			}
		}
		if len(filtered) > 0 {
			c.identityFiles = filtered
		}
	}

	cb, err := c.buildHostKeyCallback()
	if err != nil {
		return nil, err
	}
	c.hostKeyCB = cb
	return c, nil
}

// ValidTarget reports whether target RESOLVES via resolver to a non-empty
// HostName — i.e. whether native can dial it. It calls resolver directly (with
// DefaultConfigResolver it execs `ssh -G`), so an ssh_config alias that resolves
// is a valid native target (the T8/T11 amendment retired the parse-only
// user@host guard). Selection-time callers use it to reject `transport native`
// for an unresolvable host BEFORE the bad selection is persisted.
func ValidTarget(ctx context.Context, target string, resolver ConfigResolver) error {
	rh, err := resolver(ctx, target)
	if err != nil {
		return err
	}
	if rh.HostName == "" {
		return fmt.Errorf("resolved host for %q is empty", target)
	}
	return nil
}

// buildHostKeyCallback constructs the STRICT host-key verifier from the resolved
// config, delegating to newStrictHostKeyCallback. WithHostKeyCallback replaces it
// wholesale.
func (c *Client) buildHostKeyCallback() (ssh.HostKeyCallback, error) {
	return newStrictHostKeyCallback(c.knownHostsPath, c.hostKeyOverride, c.hostKeyAlias, c.host, c.port)
}

// newStrictHostKeyCallback builds the STRICT host-key verifier. override replaces
// the known_hosts-file construction wholesale (test escape hatch). Otherwise the
// callback is built from knownHostsPath; a missing file is treated as "no known
// hosts" (every host unknown → strict) so New still succeeds and the failure
// surfaces at Ensure with the remediation hint, never a raw file-not-found.
//
// LOCKED QUERY-ADDRESS MECHANIC: the address handed to the knownhosts base MUST
// always carry an explicit port and is a RAW net.JoinHostPort, NEVER a
// knownhosts.Normalize'd value. When hostKeyAlias != "" the lookup key is
// net.JoinHostPort(hostKeyAlias, "22") — the fixed :22 makes x/crypto's internal
// Normalize collapse to the bare alias regardless of dial port, matching
// OpenSSH's port-less HostKeyAlias lookup. Otherwise it is
// net.JoinHostPort(host, port) — byte-identical to the dial address ssh.Dial
// hands the callback for all ports (incl. 22). A knownhosts.Normalize'd address
// would strip a default :22 to a bare host, and x/crypto's knownhosts.check runs
// net.SplitHostPort on the query and errors on any port-less string — failing
// verification unconditionally for every alias-keyed or port-22 target.
func newStrictHostKeyCallback(knownHostsPath string, override ssh.HostKeyCallback, hostKeyAlias, host string, port int) (ssh.HostKeyCallback, error) {
	if override != nil {
		return override, nil
	}
	base, err := knownhosts.New(knownHostsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			base = func(string, net.Addr, ssh.PublicKey) error {
				return fmt.Errorf("no known_hosts entry (%s does not exist)", knownHostsPath)
			}
		} else {
			return nil, fmt.Errorf("sshnative: load known_hosts %s: %w", knownHostsPath, err)
		}
	}

	var lookupKey, errHost string
	if hostKeyAlias != "" {
		lookupKey = net.JoinHostPort(hostKeyAlias, "22")
		errHost = hostKeyAlias
	} else {
		lookupKey = net.JoinHostPort(host, strconv.Itoa(port))
		errHost = host
	}
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		if err := base(lookupKey, remote, key); err != nil {
			return fmt.Errorf("host key verification failed for %s: %w; run `ssh %s` once manually to record the host key", errHost, err, errHost)
		}
		return nil
	}, nil
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
	// Snapshot cadence under mu so the goroutine never races a later Option
	// mutation (there is none today, but the capture keeps the goroutine
	// self-contained and -race clean).
	period := c.keepalivePeriod
	strikeLimit := c.keepaliveStrikes
	go func() {
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		strikes := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if keepaliveProbe(client, period, stop) {
					strikes = 0
					continue
				}
				strikes++
				if strikes >= strikeLimit {
					c.markDead(client)
					return
				}
			}
		}
	}()
}

// keepaliveProbe sends one keepalive@openssh.com global request and waits at
// most timeout for the reply, returning true only on a reply. A transport error
// OR a timeout returns false (a strike). The reply deadline is ESSENTIAL: on a
// black-holed half-open connection (laptop sleep, wifi drop, NAT rebind — the
// primary failure this tool must self-heal from) the peer never replies and
// never sends a FIN/RST, so a bare SendRequest blocks until the OS TCP
// retransmit timeout (~15 min on Linux), leaving Health.Up=true and the ticker
// stuck for the whole window. The probe goroutine may outlive this call while
// still blocked in SendRequest; res is buffered so it never leaks, and it exits
// when the connection is eventually torn down. A stop closes the wait promptly
// (teardown) and is reported as success so the caller's next loop observes stop.
func keepaliveProbe(client *ssh.Client, timeout time.Duration, stop <-chan struct{}) bool {
	res := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		res <- err
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-res:
		return err == nil
	case <-t.C:
		return false // black-holed: no reply within the deadline -> strike
	case <-stop:
		return true // teardown; do not count a strike, the loop will exit on stop
	}
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

	// Per-call ctx must interrupt a started session (the sole cancellation seam
	// x/crypto/ssh exposes is closing the session), mirroring the exec.CommandContext
	// semantics sshctl/localexec give the SAME interface.
	stop := watchSessionCtx(ctx, sess)
	defer stop()

	runErr := sess.Run(strings.Join(argv, " "))
	stdout, stderr := outBuf.String(), errBuf.String()
	if runErr == nil {
		return stdout, stderr, nil
	}
	// A ctx deadline/cancel that tore the session down surfaces as the ctx error,
	// not the opaque channel-closed error the interrupted Run returns.
	if ctx != nil && ctx.Err() != nil {
		return stdout, stderr, fmt.Errorf("sshnative: exec: %w", ctx.Err())
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
	// A ctx deadline/cancel closes the session so a hung Stream consumer unblocks
	// (exec.CommandContext parity — see watchSessionCtx). The watcher lives until
	// wait observes the session end.
	stop := watchSessionCtx(ctx, sess)
	wait := func() error {
		werr := sess.Wait()
		stop()
		sess.Close()
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return werr
	}
	// ssh.Session pipes are io.Reader; adapt to io.ReadCloser (session Close, in
	// wait, tears the underlying channel down — the reader Close is a no-op).
	return stdin, nopReadCloser{stdout}, nopReadCloser{stderr}, wait, nil
}

// watchSessionCtx closes sess when ctx is done, giving Exec/Stream the per-call
// cancellation the other Transport impls get from exec.CommandContext: after the
// session starts, ctx's deadline/cancel is otherwise ignored because x/crypto/ssh
// only watches ctx during the dial. Closing the session unblocks Run/Wait. The
// returned stop func ends the watcher once the session finishes on its own. A nil
// or never-cancelable ctx (e.g. context.Background) installs no goroutine.
func watchSessionCtx(ctx context.Context, sess *ssh.Session) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// nopReadCloser adapts the ssh.Session pipe io.Readers to io.ReadCloser.
type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

// Close closes the client. stopped is true iff there was a live client to stop.
func (c *Client) Close(ctx context.Context) (bool, error) {
	// Native forwards die with the daemon (no ControlPersist analogue), so a
	// graceful Close drops every live forward before tearing down the client.
	c.closeAllForwards()
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
