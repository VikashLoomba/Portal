package sshnative

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeResolver maps a target/hop token to a ResolvedHost. It is the hermetic
// ConfigResolver the T12 tests inject: New and expandJumpChain resolve every
// token through it, so the whole ProxyJump/ProxyCommand topology is described in
// the test with NO ssh -G and NO real config.
type fakeResolver map[string]ResolvedHost

func (f fakeResolver) resolve(_ context.Context, target string) (ResolvedHost, error) {
	rh, ok := f[target]
	if !ok {
		return ResolvedHost{}, fmt.Errorf("fake resolver: unknown target %q", target)
	}
	return rh, nil
}

// serverEndpoint returns a server's loopback host and actual listen port, so a
// fake ResolvedHost points at the real in-process server (host-key lines match
// the JoinHostPort(host,port) query address).
func serverEndpoint(t *testing.T, s *testServer) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.addr)
	if err != nil {
		t.Fatalf("split server addr %q: %v", s.addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse server port %q: %v", portStr, err)
	}
	return host, port
}

// writeKnownHostsLines writes several known_hosts lines to one temp file and
// returns its path — the combined trust store for a multi-hop chain.
func writeKnownHostsLines(t *testing.T, servers ...*testServer) string {
	t.Helper()
	var lines []string
	for _, s := range servers {
		lines = append(lines, s.knownHostsLine())
	}
	return writeKnownHosts(t, strings.Join(lines, "\n"))
}

// newProxyClient builds a native Client for target resolved through fake, wired
// to the shared client key (keyFile), the combined known_hosts (kh), agent
// disabled, and any extra options. It does NOT Ensure.
func newProxyClient(t *testing.T, fake fakeResolver, keyFile, kh string, extra ...Option) *Client {
	t.Helper()
	opts := []Option{
		WithConfigResolver(fake.resolve),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""),
	}
	opts = append(opts, extra...)
	c, err := New("target", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// --- ProxyJump ---

// TestProxyJumpTwoHopExec (EC12) proves a 1-jump chain reaches the target and
// the jump was actually traversed (A saw a direct-tcpip dial to B).
func TestProxyJumpTwoHopExec(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey()) // jump
	b := newTestServer(t, clientSigner.PublicKey()) // target
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	kh := writeKnownHostsLines(t, a, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via proxyjump: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
	if !contains(a.directDials(), b.addr) {
		t.Errorf("jump A directDials = %v, want to contain target %q", a.directDials(), b.addr)
	}
}

// TestProxyJumpNestedChain (EC12) proves a hop's OWN ProxyJump is followed
// recursively: expandJumpChain yields [A,B] then target C, and each hop dials
// the next (A->B, B->C) — depth-first left-expansion, not a flat comma list.
func TestProxyJumpNestedChain(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())
	b := newTestServer(t, clientSigner.PublicKey())
	cc := newTestServer(t, clientSigner.PublicKey()) // target
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	cHost, cPort := serverEndpoint(t, cc)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: cHost, Port: cPort, ProxyJump: "hopb"},
		"hopb":   {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	kh := writeKnownHostsLines(t, a, b, cc)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	// expandJumpChain yields dial order [hopa(A), hopb(B)].
	chain, err := c.expandJumpChain(ctx)
	if err != nil {
		t.Fatalf("expandJumpChain: %v", err)
	}
	if len(chain) != 2 || chain[0].Port != aPort || chain[1].Port != bPort {
		t.Fatalf("expandJumpChain order = %+v, want [A(%d) B(%d)]", chain, aPort, bPort)
	}

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via nested proxyjump: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
	if !contains(a.directDials(), b.addr) {
		t.Errorf("A directDials = %v, want to contain B %q", a.directDials(), b.addr)
	}
	if !contains(b.directDials(), cc.addr) {
		t.Errorf("B directDials = %v, want to contain C %q", b.directDials(), cc.addr)
	}
}

// TestProxyJumpForwardRoundTrip (EC12) proves a forward round-trips through the
// jump chain: local listener -> direct-tcpip on the target (reached via the
// jump) -> in-process echo.
func TestProxyJumpForwardRoundTrip(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())
	b := newTestServer(t, clientSigner.PublicKey()) // target
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	kh := writeKnownHostsLines(t, a, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)
	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	echoPort := startEchoRemote(t)
	local := reserveLocalPort(t)
	if err := c.Forward(ctx, local, echoPort); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	ports, err := c.ListForwards(ctx)
	if err != nil {
		t.Fatalf("ListForwards: %v", err)
	}
	if len(ports) != 1 || ports[0] != local {
		t.Errorf("ListForwards = %v, want [%d]", ports, local)
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(local), 5*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer conn.Close()
	msg := []byte("through-the-chain\n")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write through forward: %v", err)
	}
	got := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo through chain: %v", err)
	}
	if string(got) != string(msg) {
		t.Errorf("echo = %q, want %q", got, msg)
	}
}

// TestProxyJumpHopCap (EC12) proves a chain exceeding maxProxyHops is rejected,
// the client stays down, and nothing is stored.
func TestProxyJumpHopCap(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	var jumpTokens []string
	fake := fakeResolver{}
	for i := 1; i <= maxProxyHops+1; i++ {
		tok := fmt.Sprintf("h%d", i)
		jumpTokens = append(jumpTokens, tok)
		fake[tok] = ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: 1}
	}
	fake["target"] = ResolvedHost{User: "testuser", HostName: bHost, Port: bPort, ProxyJump: strings.Join(jumpTokens, ",")}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with over-cap chain: want error, got nil")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want hop-cap error mentioning 'exceeds'", err)
	}
	assertNothingStored(t, c)
}

// TestProxyJumpCycleGuard (EC12) proves the visited-set catches a recursion
// cycle — a hop whose own ProxyJump points back at itself — not just a flat
// repeat, and stores nothing.
func TestProxyJumpCycleGuard(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: "127.0.0.1", Port: 1, ProxyJump: "hopa"},
	}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with cyclic chain: want error, got nil")
	} else if !strings.Contains(err.Error(), "cycle at") || !strings.Contains(err.Error(), "hopa") {
		t.Errorf("error = %v, want cycle error naming 'hopa'", err)
	}
	assertNothingStored(t, c)
}

// TestProxyJumpWrongHopKey (EC12) proves a wrong host key for a hop aborts the
// WHOLE dial (strict per-hop verification) and leaves Exec failing afterward.
// The hop's ephemeral non-22 port exercises the JoinHostPort(host,port) branch.
func TestProxyJumpWrongHopKey(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey()) // jump
	b := newTestServer(t, clientSigner.PublicKey()) // target
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	// known_hosts holds a WRONG key for hop A (a foreign signer) + the right key
	// for target B.
	_, wrongSigner := generateKeyPair(t)
	kh := writeKnownHosts(t, lineFor(a.addr, wrongSigner)+"\n"+b.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with wrong hop key: want host-key error, got nil")
	} else if !strings.Contains(err.Error(), "host key verification failed") {
		t.Errorf("error = %v, want host-key verification failure", err)
	}
	if _, _, err := c.Exec(ctx, nil, "echo", "hi"); err == nil {
		t.Error("Exec after failed hop key: want error, got nil")
	}
}

// --- ProxyCommand ---

// recordingProxyCmd is the injected ProxyCommand seam: it records each command
// and each closer Close, and (when srv != nil) serves the target over an
// in-process loopback socketpair with NO subprocess — the client end stands in
// for the ProxyCommand process's stdio net.Conn. (A synchronous net.Pipe would
// deadlock the ssh version/KEX exchange, where both ends write before either
// reads; a loopback TCP pair has the kernel buffering the real stdio conn has.)
type recordingProxyCmd struct {
	t   *testing.T
	srv *testServer

	mu       sync.Mutex
	calls    int
	commands []string
	closes   int
}

func (r *recordingProxyCmd) dial(_ context.Context, command string) (net.Conn, io.Closer, error) {
	r.mu.Lock()
	r.calls++
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	clientEnd, serverEnd := loopbackPipe(r.t)
	if r.srv != nil {
		go r.srv.serveConn(serverEnd)
	}
	return clientEnd, closerFunc(func() error {
		r.mu.Lock()
		r.closes++
		r.mu.Unlock()
		return nil
	}), nil
}

// loopbackPipe returns a connected pair of loopback TCP conns — a buffered,
// bidirectional in-process stand-in for a ProxyCommand process's stdio.
func loopbackPipe(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopbackPipe listen: %v", err)
	}
	defer ln.Close()
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		ch <- accepted{conn, err}
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopbackPipe dial: %v", err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatalf("loopbackPipe accept: %v", a.err)
	}
	return client, a.conn
}

func (r *recordingProxyCmd) snapshot() (calls int, commands []string, closes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]string(nil), r.commands...), r.closes
}

// closerFunc adapts a func to io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestProxyCommandRoundTrip (EC13) proves the target is reached over the stdio
// net.Conn the seam returns, and the seam was invoked.
func TestProxyCommandRoundTrip(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey()) // target, served over the pipe
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: "nc %h %p"},
	}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)
	rec := &recordingProxyCmd{t: t, srv: b}
	c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(rec.dial))

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure via proxycommand: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via proxycommand: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
	if calls, _, _ := rec.snapshot(); calls != 1 {
		t.Errorf("seam calls = %d, want 1", calls)
	}
}

// TestProxyCommandTokenExpansion (EC13) proves %r/%h/%p expand to the resolved
// target and the %% escape yields a literal % (and is NOT swallowed).
func TestProxyCommandTokenExpansion(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)

	tests := []struct {
		name     string
		template string
		want     string
	}{
		{"user-host-port", "run %r@%h:%p", "run testuser@" + bHost + ":" + strconv.Itoa(bPort)},
		{"percent-escape", "echo %%done", "echo %done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakeResolver{
				"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: tt.template},
			}
			rec := &recordingProxyCmd{t: t, srv: b}
			c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(rec.dial))
			if _, err := c.Ensure(context.Background()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			_, cmds, _ := rec.snapshot()
			if len(cmds) != 1 || cmds[0] != tt.want {
				t.Errorf("captured command = %v, want [%q]", cmds, tt.want)
			}
		})
	}
}

// TestProxyCommandUnsupportedToken (EC13) proves an unsupported or dangling
// token fails BEFORE the seam is invoked (the dial errors before exec).
func TestProxyCommandUnsupportedToken(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)

	tests := []struct {
		name     string
		template string
		wantErr  string
	}{
		{"unsupported", "cmd %z", "%z"},
		{"trailing-percent", "cmd trailing %", "dangling"},
		{"lone-percent", "%", "dangling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakeResolver{
				"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: tt.template},
			}
			rec := &recordingProxyCmd{t: t, srv: b}
			c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(rec.dial))
			_, err := c.Ensure(context.Background())
			if err == nil {
				t.Fatalf("Ensure with %q: want error, got nil", tt.template)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want to mention %q", err, tt.wantErr)
			}
			if calls, _, _ := rec.snapshot(); calls != 0 {
				t.Errorf("seam invoked %d times, want 0 (dial fails before exec)", calls)
			}
		})
	}
}

// TestProxyJumpPrecedenceOverCommand (EC13) proves ProxyJump wins when both are
// set (matches OpenSSH): the ProxyCommand seam is never called.
func TestProxyJumpPrecedenceOverCommand(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())
	b := newTestServer(t, clientSigner.PublicKey())
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa", ProxyCommand: "nc %h %p"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	kh := writeKnownHostsLines(t, a, b)
	keyFile := writeIdentityFile(t, clientPriv)
	rec := &recordingProxyCmd{t: t, srv: b}
	c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(rec.dial))

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
	if calls, _, _ := rec.snapshot(); calls != 0 {
		t.Errorf("proxycommand seam calls = %d, want 0 (proxyjump wins)", calls)
	}
}

// --- Chain teardown (EC15) ---

// TestChainTeardownProxyJump (EC15) proves Close tears down the jump chain (the
// jump clients are drained and the old jump connection is closed) and a
// subsequent Ensure rebuilds a fresh working chain.
func TestChainTeardownProxyJump(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())
	b := newTestServer(t, clientSigner.PublicKey())
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	kh := writeKnownHostsLines(t, a, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c.mu.Lock()
	if len(c.jumpClients) != 1 {
		c.mu.Unlock()
		t.Fatalf("jumpClients after Ensure = %d, want 1", len(c.jumpClients))
	}
	oldJump := c.jumpClients[0]
	c.mu.Unlock()

	if _, err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.mu.Lock()
	drained := len(c.jumpClients)
	c.mu.Unlock()
	if drained != 0 {
		t.Errorf("jumpClients after Close = %d, want 0 (drained)", drained)
	}
	// The old jump connection is torn down: Wait returns instead of blocking.
	if !waitClosed(oldJump, 3*time.Second) {
		t.Error("old jump client still connected after Close (leaked conn)")
	}

	// Re-dial rebuilds a fresh working chain.
	rebuilt, err := c.Ensure(ctx)
	if err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if !rebuilt {
		t.Error("re-Ensure rebuilt = false, want true")
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec after rebuild: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
}

// TestChainTeardownProxyCommand (EC15) proves the ProxyCommand closer's Close is
// called on native.Close (process-exit teardown in production) and a redial
// invokes the seam again.
func TestChainTeardownProxyCommand(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: "nc %h %p"},
	}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)
	rec := &recordingProxyCmd{t: t, srv: b}
	c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(rec.dial))

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, closes := rec.snapshot(); closes < 1 {
		t.Errorf("closer Close calls after native Close = %d, want >= 1", closes)
	}

	rebuilt, err := c.Ensure(ctx)
	if err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if !rebuilt {
		t.Error("re-Ensure rebuilt = false, want true")
	}
	if calls, _, _ := rec.snapshot(); calls != 2 {
		t.Errorf("seam calls after redial = %d, want 2", calls)
	}
}

// --- helpers ---

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// assertNothingStored proves a failed proxy dial left the client fully down: no
// client, no chain state, and Health.Up false.
func assertNothingStored(t *testing.T, c *Client) {
	t.Helper()
	c.mu.Lock()
	clientNil := c.client == nil
	jumps := len(c.jumpClients)
	closerNil := c.proxyCloser == nil
	c.mu.Unlock()
	if !clientNil {
		t.Error("client stored after failed dial, want nil")
	}
	if jumps != 0 {
		t.Errorf("jumpClients = %d after failed dial, want 0", jumps)
	}
	if !closerNil {
		t.Error("proxyCloser stored after failed dial, want nil")
	}
	if h, _ := c.Health(context.Background()); h.Up {
		t.Error("Health.Up true after failed dial, want false")
	}
}

// waitClosed reports whether client's connection closes within timeout — proof
// the chain tore the hop down rather than leaking it.
func waitClosed(client *ssh.Client, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		client.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
