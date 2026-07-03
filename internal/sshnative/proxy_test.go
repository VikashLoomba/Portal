package sshnative

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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
	c := newProxyClientNoCleanup(t, fake, keyFile, kh, extra...)
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func newProxyClientNoCleanup(t *testing.T, fake fakeResolver, keyFile, kh string, extra ...Option) *Client {
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
	return c
}

// blackholeListener accepts TCP and keeps each conn open without speaking SSH.
// Cleanup is explicit so timeout tests can close it before closing a client that
// may be blocked in Ensure on a regression.
type blackholeListener struct {
	ln net.Listener

	mu      sync.Mutex
	conns   []net.Conn
	accepts int

	accepted     chan struct{}
	acceptedOnce sync.Once
	closeOnce    sync.Once
}

func startBlackholeListener(t *testing.T) *blackholeListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blackhole listen: %v", err)
	}
	b := &blackholeListener{ln: ln, accepted: make(chan struct{})}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, conn)
			b.accepts++
			b.mu.Unlock()
			b.acceptedOnce.Do(func() { close(b.accepted) })
		}
	}()
	return b
}

func (b *blackholeListener) endpoint(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(b.ln.Addr().String())
	if err != nil {
		t.Fatalf("split blackhole addr %q: %v", b.ln.Addr().String(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse blackhole port %q: %v", portStr, err)
	}
	return host, port
}

func (b *blackholeListener) acceptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepts
}

func (b *blackholeListener) close() {
	b.closeOnce.Do(func() {
		b.ln.Close()
		b.mu.Lock()
		conns := append([]net.Conn(nil), b.conns...)
		b.mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
}

func ensureErrorWithin(t *testing.T, c *Client, ctx context.Context, limit time.Duration, onTimeout func()) (error, time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.Ensure(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("Ensure: want error, got nil")
		}
		return err, elapsed
	case <-time.After(limit):
		if onTimeout != nil {
			onTimeout()
		}
		select {
		case err := <-done:
			t.Fatalf("Ensure did not return within %s; returned after timeout cleanup with %v", limit, err)
		case <-time.After(time.Second):
			t.Fatalf("Ensure did not return within %s", limit)
		}
	}
	return nil, 0
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

// TestProxyJumpBlackHoleHopHandshakeHonorsContext proves a first hop that accepts
// TCP but never speaks SSH is bounded by the caller ctx, not the 15s dialTimeout.
func TestProxyJumpBlackHoleHopHandshakeHonorsContext(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	target := newTestServer(t, clientSigner.PublicKey())
	blackhole := startBlackholeListener(t)
	t.Cleanup(func() { blackhole.close() })
	bhHost, bhPort := blackhole.endpoint(t)
	tHost, tPort := serverEndpoint(t, target)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: tHost, Port: tPort, ProxyJump: "blackhole"},
		"blackhole": {
			User:     "testuser",
			HostName: bhHost,
			Port:     bhPort,
		},
	}
	kh := writeKnownHostsLines(t, target)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClientNoCleanup(t, fake, keyFile, kh)
	defer c.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	err, elapsed := ensureErrorWithin(t, c, ctx, 4*time.Second, blackhole.close)
	if !strings.Contains(err.Error(), "handshake proxyjump hop") {
		t.Errorf("error = %v, want proxyjump hop handshake failure", err)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Errorf("Ensure elapsed = %s, want near the 1.2s ctx timeout and well below dialTimeout %s", elapsed, dialTimeout)
	}
	assertNothingStored(t, c)
}

// TestProxyJumpChainedBlackHoleHonorsCancel proves ctx cancellation aborts a
// chained direct-tcpip hop promptly after the first jump has connected.
func TestProxyJumpChainedBlackHoleHonorsCancel(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())
	target := newTestServer(t, clientSigner.PublicKey())
	blackhole := startBlackholeListener(t)
	t.Cleanup(func() { blackhole.close() })
	aHost, aPort := serverEndpoint(t, a)
	bhHost, bhPort := blackhole.endpoint(t)
	tHost, tPort := serverEndpoint(t, target)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: tHost, Port: tPort, ProxyJump: "hopa,blackhole"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
		"blackhole": {
			User:     "testuser",
			HostName: bhHost,
			Port:     bhPort,
		},
	}
	kh := writeKnownHostsLines(t, a, target)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClientNoCleanup(t, fake, keyFile, kh)
	defer c.Close(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Ensure(ctx)
		done <- err
	}()
	select {
	case <-blackhole.accepted:
	case <-time.After(3 * time.Second):
		blackhole.close()
		t.Fatal("blackhole hop was not reached through the first jump")
	}

	cancelStarted := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Ensure after cancel: want error, got nil")
		}
		if !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("error = %v, want context cancellation to surface", err)
		}
		if elapsed := time.Since(cancelStarted); elapsed >= 2*time.Second {
			t.Errorf("Ensure returned %s after cancel, want prompt cancellation", elapsed)
		}
	case <-time.After(3 * time.Second):
		blackhole.close()
		t.Fatal("Ensure did not return promptly after ctx cancellation")
	}
	assertNothingStored(t, c)
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

func nestedProxyJumpFake(hops int, target ResolvedHost) fakeResolver {
	fake := fakeResolver{}
	if hops > 0 {
		target.ProxyJump = "h1"
	}
	fake["target"] = target
	for i := 1; i <= hops; i++ {
		tok := fmt.Sprintf("h%d", i)
		rh := ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: i}
		if i < hops {
			rh.ProxyJump = fmt.Sprintf("h%d", i+1)
		}
		fake[tok] = rh
	}
	return fake
}

// TestProxyJumpNestedHopCap proves a depth-first nested chain exceeding
// maxProxyHops is rejected before any hop is appended or dialed.
func TestProxyJumpNestedHopCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	clientPriv, clientSigner := generateKeyPair(t)
	target := newTestServer(t, clientSigner.PublicKey())
	listener := startBlackholeListener(t)
	defer listener.close()
	tHost, tPort := serverEndpoint(t, target)
	hHost, hPort := listener.endpoint(t)
	fake := nestedProxyJumpFake(maxProxyHops+1, ResolvedHost{User: "testuser", HostName: tHost, Port: tPort})
	for tok, rh := range fake {
		if tok == "target" {
			continue
		}
		rh.HostName = hHost
		rh.Port = hPort
		fake[tok] = rh
	}
	kh := writeKnownHostsLines(t, target)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with nested over-cap chain: want error, got nil")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want hop-cap error mentioning 'exceeds'", err)
	}
	if got := listener.acceptCount(); got != 0 {
		t.Errorf("hop listener accepts = %d, want 0 (rejected before dial)", got)
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

// TestProxyJumpPerHopIdentity proves each hop authenticates with ITS OWN
// resolved IdentityFile, not the target's. Hop A accepts ONLY a bastion key
// (never in any agent — the client disables the agent); the target accepts the
// shared client key. With per-hop auth the bastion key is offered to A and the
// chain reaches the target; offering the target's key to A (the bug) is rejected
// by A and aborts the whole dial.
func TestProxyJumpPerHopIdentity(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)   // target credential
	bastionPriv, bastionSigner := generateKeyPair(t) // hop-only credential
	a := newTestServer(t, bastionSigner.PublicKey()) // jump: accepts ONLY the bastion key
	b := newTestServer(t, clientSigner.PublicKey())  // target: accepts the shared key
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	bastionKeyFile := writeIdentityFile(t, bastionPriv)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort, IdentityFiles: []string{bastionKeyFile}},
	}
	kh := writeKnownHostsLines(t, a, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure with per-hop identity: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via per-hop identity: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
}

// TestHopHostKeyCallbackPortlessHop proves a hop resolved with an unspecified
// port (Port==0) queries known_hosts at host:22 — the address portOr22 forms for
// the dial — not host:0 (which knownhosts.check rejects), matching the target
// path that defaults the port to 22 before building its callback.
func TestHopHostKeyCallbackPortlessHop(t *testing.T) {
	hostKey := generateSSHKey(t)
	// A real :22 server records the bare-host entry (Normalize drops the default
	// port), so key known_hosts by that canonical form.
	entry := knownhosts.Line([]string{knownhosts.Normalize("127.0.0.1:22")}, hostKey.PublicKey())
	kh := writeKnownHosts(t, entry)
	c := &Client{knownHostsPath: kh}

	cb, err := c.hopHostKeyCallback(ResolvedHost{HostName: "127.0.0.1", Port: 0})
	if err != nil {
		t.Fatalf("hopHostKeyCallback: %v", err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := cb("127.0.0.1:22", addr, hostKey.PublicKey()); err != nil {
		t.Errorf("port-less hop rejected a matching host key: %v", err)
	}
	// Guard: the raw-port construction (the bug) queries host:0 and fails to match
	// the bare-host entry, proving portOr22 is load-bearing.
	bad, err := newStrictHostKeyCallback(kh, nil, "", "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("newStrictHostKeyCallback raw port 0: %v", err)
	}
	if err := bad("127.0.0.1:0", addr, hostKey.PublicKey()); err == nil {
		t.Error("raw port-0 host-key query unexpectedly matched; the portOr22 fix would be untested")
	}
}

// TestProxyJumpHopCapPositiveBoundary proves a chain of EXACTLY maxProxyHops hops
// is ACCEPTED and reaches the target — the positive boundary the over-cap
// rejection test (TestProxyJumpHopCap) leaves untested, so an over-strict
// off-by-one (or a lowered cap constant) that rejects a legitimate deep chain is
// caught here.
func TestProxyJumpHopCapPositiveBoundary(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	target := newTestServer(t, clientSigner.PublicKey())
	tHost, tPort := serverEndpoint(t, target)

	fake := fakeResolver{}
	allServers := []*testServer{}
	var tokens []string
	for i := 1; i <= maxProxyHops; i++ {
		hop := newTestServer(t, clientSigner.PublicKey())
		hHost, hPort := serverEndpoint(t, hop)
		tok := fmt.Sprintf("h%d", i)
		tokens = append(tokens, tok)
		fake[tok] = ResolvedHost{User: "testuser", HostName: hHost, Port: hPort}
		allServers = append(allServers, hop)
	}
	allServers = append(allServers, target)
	fake["target"] = ResolvedHost{User: "testuser", HostName: tHost, Port: tPort, ProxyJump: strings.Join(tokens, ",")}

	kh := writeKnownHostsLines(t, allServers...)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	chain, err := c.expandJumpChain(ctx)
	if err != nil {
		t.Fatalf("expandJumpChain of maxProxyHops: %v", err)
	}
	if len(chain) != maxProxyHops {
		t.Fatalf("chain length = %d, want maxProxyHops=%d", len(chain), maxProxyHops)
	}
	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure with %d-hop chain: %v", maxProxyHops, err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via %d-hop chain: %v", maxProxyHops, err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
}

// TestProxyJumpNestedHopCapPositiveBoundary proves a nested chain of EXACTLY
// maxProxyHops hops is accepted by the pre-recursion cap.
func TestProxyJumpNestedHopCapPositiveBoundary(t *testing.T) {
	ctx := context.Background()
	fake := nestedProxyJumpFake(maxProxyHops, ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: 22})
	c := &Client{proxyJump: fake["target"].ProxyJump, resolver: fake.resolve}

	chain, err := c.expandJumpChain(ctx)
	if err != nil {
		t.Fatalf("expandJumpChain of nested maxProxyHops: %v", err)
	}
	if len(chain) != maxProxyHops {
		t.Fatalf("chain length = %d, want maxProxyHops=%d", len(chain), maxProxyHops)
	}
	for i, rh := range chain {
		wantPort := maxProxyHops - i
		if rh.Port != wantPort {
			t.Fatalf("chain[%d].Port = %d, want %d", i, rh.Port, wantPort)
		}
	}
}

// TestProxyJumpAgentConnsClosedOnTeardown proves the shared-agent hop-auth path
// runs with a REAL agent conn — the production default, where $SSH_AUTH_SOCK is
// non-empty — and that teardownLocked closes EVERY per-hop agent conn on Close.
// The rest of the proxy suite disables the agent (newProxyClient hardcodes
// WithAgentSocket("")), so c.hopAgentConns stays nil and neither the per-hop
// acquisition (hopAuthMethods -> buildAuthMethods dials a fresh unix agent conn)
// nor the teardown close-loop is ever exercised with a live conn. Here the
// identity file is absent, so BOTH the hop and the target authenticate SOLELY
// through the agent: each opens an agent conn (the hop's lands in
// c.hopAgentConns), and the counting agent makes closing them observable.
// Dropping the teardown close-loop leaks the hop conn — live() never returns to 0
// — where every agent-disabled proxy test still passes.
func TestProxyJumpAgentConnsClosedOnTeardown(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey()) // hop: accepts the agent key
	b := newTestServer(t, clientSigner.PublicKey()) // target: accepts the agent key
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	kh := writeKnownHostsLines(t, a, b)
	ca := startCountingAgent(t, clientPriv)
	// A missing identity file forces auth through the agent for BOTH hops, so the
	// hop agent conn is real and non-empty (not the nil the rest of the suite has).
	missingKey := filepath.Join(t.TempDir(), "no-identity")
	c := newProxyClient(t, fake, missingKey, kh, WithAgentSocket(ca.sock))

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure via agent-authenticated proxyjump: %v", err)
	}
	// The chain actually authenticated through the agent for both hop and target.
	if _, _, err := c.Exec(ctx, nil, "echo", "hi"); err != nil {
		t.Fatalf("Exec via agent-authenticated proxyjump: %v", err)
	}
	// Acquisition path ran with a live conn: one hop => one stored hop agent conn,
	// and the agent holds both the hop and target conns open.
	c.mu.Lock()
	hopConns := append([]net.Conn(nil), c.hopAgentConns...)
	c.mu.Unlock()
	if len(hopConns) != 1 || hopConns[0] == nil {
		t.Fatalf("hopAgentConns after Ensure = %v, want 1 non-nil conn", hopConns)
	}
	if live := ca.live(); live != 2 {
		t.Fatalf("agent live conns after Ensure = %d, want 2 (hop + target)", live)
	}

	if _, err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.mu.Lock()
	drained := len(c.hopAgentConns)
	c.mu.Unlock()
	if drained != 0 {
		t.Errorf("hopAgentConns after Close = %d, want 0 (drained)", drained)
	}
	// Every agent conn (hop + target) is actually closed: each served goroutine
	// returns and live() reaches 0. A dropped teardown close-loop leaves the hop
	// conn open, so live() stays >=1 and this never settles.
	if !waitFor(func() bool { return ca.live() == 0 }, 3*time.Second) {
		t.Errorf("agent live conns after Close = %d, want 0; a hop agent conn leaked", ca.live())
	}
}

// TestProxyJumpHopProxyCommandRejectedBeforeDial proves native rejects a
// ProxyCommand on a ProxyJump hop during chain expansion, before any hop dial.
func TestProxyJumpHopProxyCommandRejectedBeforeDial(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	target := newTestServer(t, clientSigner.PublicKey())
	listener := startBlackholeListener(t)
	defer listener.close()
	tHost, tPort := serverEndpoint(t, target)
	hHost, hPort := listener.endpoint(t)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: tHost, Port: tPort, ProxyJump: "hopcmd"},
		"hopcmd": {
			User:         "testuser",
			HostName:     hHost,
			Port:         hPort,
			ProxyCommand: "ssh -W %h:%p inner",
		},
	}
	kh := writeKnownHostsLines(t, target)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClientNoCleanup(t, fake, keyFile, kh)
	defer c.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Ensure(ctx)
	want := `sshnative: proxyjump hop "hopcmd" uses ProxyCommand, which native does not support for hops`
	if err == nil {
		t.Fatal("Ensure with ProxyCommand on hop: want error, got nil")
	}
	if err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if got := listener.acceptCount(); got != 0 {
		t.Errorf("hop listener accepts = %d, want 0 (rejected before dial)", got)
	}
	assertNothingStored(t, c)
}

// TestProxyJumpHopProxyCommandNoneAllowed proves the case-sensitive "none"
// sentinel disables a hop ProxyCommand and leaves the ProxyJump path usable.
func TestProxyJumpHopProxyCommandNoneAllowed(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())
	b := newTestServer(t, clientSigner.PublicKey())
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort, ProxyCommand: "none"},
	}
	kh := writeKnownHostsLines(t, a, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure through ProxyCommand=none hop: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec through ProxyCommand=none hop: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
	if !contains(a.directDials(), b.addr) {
		t.Errorf("jump directDials = %v, want to contain target %q", a.directDials(), b.addr)
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

// countingCloser is a recording io.Closer: it counts Close calls so a test can
// assert the ProxyCommand process closer was reaped exactly once.
type countingCloser struct {
	mu     sync.Mutex
	closes int
}

func (c *countingCloser) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *countingCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// closeCountingConn wraps a net.Conn and counts Close calls, so a test can assert
// the handshake-failure cleanup closed the ProxyCommand stdio conn too.
type closeCountingConn struct {
	net.Conn
	mu     sync.Mutex
	closes int
}

func (c *closeCountingConn) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *closeCountingConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// TestProxyCommandHandshakeFailureClosesCloser (EC15) proves dialViaProxyCommand's
// handshake-failure cleanup closes BOTH the stdio conn AND the process closer when
// ssh.NewClientConn fails on a peer that connects but never speaks SSH. Every other
// ProxyCommand test hands back a server end that handshakes successfully (or fails
// before the seam), so the closer.Close on proxy.go's error branch — the ONLY thing
// that reaps the real `sh -c` subprocess on a post-connect handshake failure, since
// nothing is stored in c.proxyCloser for teardownLocked to reap — is otherwise
// unexercised. The seam returns a live loopback conn whose server end is severed
// immediately, so the SSH version exchange reads EOF and NewClientConn fails.
// Dropping closer.Close() there leaks the subprocess and its fds; this test catches
// that regression.
func TestProxyCommandHandshakeFailureClosesCloser(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: "nc %h %p"},
	}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)

	closer := &countingCloser{}
	var conn *closeCountingConn
	dialer := func(_ context.Context, _ string) (net.Conn, io.Closer, error) {
		clientEnd, serverEnd := loopbackPipe(t)
		// Sever the peer immediately: the client's SSH version exchange reads EOF,
		// so ssh.NewClientConn fails without ever completing the handshake — the
		// hermetic stand-in for a proxy conn that connects but speaks non-SSH.
		serverEnd.Close()
		conn = &closeCountingConn{Conn: clientEnd}
		return conn, closer, nil
	}
	c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(dialer))

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with non-SSH proxycommand peer: want handshake error, got nil")
	} else if !strings.Contains(err.Error(), "proxycommand handshake") {
		t.Errorf("error = %v, want proxycommand handshake failure", err)
	}
	if n := closer.count(); n != 1 {
		t.Errorf("process closer Close calls = %d, want exactly 1 (handshake-failure cleanup)", n)
	}
	if conn == nil {
		t.Fatal("seam was never invoked")
	}
	if n := conn.count(); n < 1 {
		t.Errorf("stdio conn Close calls = %d, want >= 1 (handshake-failure cleanup)", n)
	}
	// The failed dial stored nothing: c.proxyCloser stays nil, so teardownLocked
	// never re-reaps the closer — the error-branch Close is the sole reaper.
	assertNothingStored(t, c)
}

// TestDefaultProxyCommandStderrIncludedOnHandshakeFailure drives the REAL
// defaultProxyCommandDialer and proves helper stderr is surfaced when the stdio
// peer exits before SSH can complete its handshake.
func TestDefaultProxyCommandStderrIncludedOnHandshakeFailure(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey())
	bHost, bPort := serverEndpoint(t, b)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: "echo helper-diag >&2; exit 1"},
	}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with failing real ProxyCommand: want error, got nil")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "proxycommand handshake") {
			t.Errorf("error = %v, want proxycommand handshake context", err)
		}
		if !strings.Contains(msg, "helper-diag") {
			t.Errorf("error = %v, want helper stderr", err)
		}
	}
	assertNothingStored(t, c)
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

// TestDefaultProxyCommandDialerReapsProcess (EC15) drives the REAL
// defaultProxyCommandDialer against a long-lived process and proves
// proxyProcessCloser.Close KILLS and reaps it. The hermetic
// TestChainTeardownProxyCommand only exercises a fake closer, so dropping the
// Kill — leaving Close to Wait on a hung ProxyCommand forever — would pass it.
// This test catches that regression two ways: Close must return promptly (a
// Wait-only Close would block ~30s on the live process) and the pid must be gone
// afterward.
func TestDefaultProxyCommandDialerReapsProcess(t *testing.T) {
	// `exec sleep` so sh replaces itself with sleep — cmd.Process IS the sleep
	// process, so Kill reaps it directly with no orphan. sleep never writes, so a
	// Wait-only (Kill-less) Close would block until the 30s natural exit.
	conn, closer, err := defaultProxyCommandDialer(context.Background(), "exec sleep 30")
	if err != nil {
		t.Fatalf("defaultProxyCommandDialer: %v", err)
	}
	pc, ok := closer.(proxyProcessCloser)
	if !ok {
		t.Fatalf("closer type = %T, want proxyProcessCloser", closer)
	}
	pid := pc.cmd.Process.Pid
	if err := pc.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("proxycommand process (pid %d) not alive before Close: %v", pid, err)
	}

	conn.Close()
	// Close must return promptly: a Kill unblocks Wait at once, whereas a Wait-only
	// Close blocks on the live sleep past this deadline.
	done := make(chan error, 1)
	go func() { done <- closer.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Close did not return within 5s; the ProxyCommand process (pid %d) was not killed", pid)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess(%d): %v", pid, err)
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Errorf("proxycommand subprocess (pid %d) still alive after Close; Kill/Wait did not reap it", pid)
	}
}

// TestDefaultProxyCommandDialerSurvivesDialCtxCancel proves the ProxyCommand
// subprocess OUTLIVES the dial ctx: the ssh master built on its stdio persists
// across calls and must die only at teardownLocked (Close/redial), never when the
// short-lived caller ctx that first triggered the dial is cancelled. Binding the
// process to that ctx via exec.CommandContext(ctx, ...) (the bug) has a watchdog
// SIGKILL it the instant ctx is Done, silently tearing the still-live master
// down. With the WithoutCancel decoupling the process survives cancellation.
func TestDefaultProxyCommandDialerSurvivesDialCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn, closer, err := defaultProxyCommandDialer(ctx, "exec sleep 30")
	if err != nil {
		cancel()
		t.Fatalf("defaultProxyCommandDialer: %v", err)
	}
	pc, ok := closer.(proxyProcessCloser)
	if !ok {
		cancel()
		t.Fatalf("closer type = %T, want proxyProcessCloser", closer)
	}

	// Own Wait here so the process is reaped whether it dies (bug) or we kill it
	// (cleanup); the watchdog-killed process would otherwise linger as a zombie
	// that still answers signal 0, defeating a liveness probe.
	waited := make(chan error, 1)
	go func() { waited <- pc.cmd.Wait() }()

	cancel() // dial ctx done: the buggy ctx-bound watchdog SIGKILLs the process here.

	select {
	case <-waited:
		t.Error("ProxyCommand process exited after the dial ctx was cancelled; its lifetime is wrongly bound to the dial ctx")
	case <-time.After(2 * time.Second):
		// Survived cancellation -> lifetime correctly decoupled from the dial ctx.
	}

	// Cleanup: Kill unblocks the Wait goroutine (a no-op if the bug already killed
	// it). waited is buffered, so the goroutine exits without a second receive here
	// (which would deadlock in the failure branch that already drained it).
	conn.Close()
	pc.cmd.Process.Kill()
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
	hopAgents := len(c.hopAgentConns)
	closerNil := c.proxyCloser == nil
	c.mu.Unlock()
	if !clientNil {
		t.Error("client stored after failed dial, want nil")
	}
	if jumps != 0 {
		t.Errorf("jumpClients = %d after failed dial, want 0", jumps)
	}
	if hopAgents != 0 {
		t.Errorf("hopAgentConns = %d after failed dial, want 0", hopAgents)
	}
	if !closerNil {
		t.Error("proxyCloser stored after failed dial, want nil")
	}
	if h, _ := c.Health(context.Background()); h.Up {
		t.Error("Health.Up true after failed dial, want false")
	}
}

// waitFor polls cond until it is true or timeout elapses, so a test can wait on
// asynchronous teardown (e.g. an agent's served goroutine returning) without a
// fixed sleep. It returns cond's final value.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
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

// TestMain lets this test binary re-exec itself as a stdio<->TCP relay: when
// PROXY_RELAY_ADDR is set it becomes the relay (a real ProxyCommand subprocess)
// and never runs the suite. TestProxyCommandRealStdioConn uses this to drive the
// REAL defaultProxyCommandDialer/stdioConn through a real SSH handshake.
func TestMain(m *testing.M) {
	if addr := os.Getenv("PROXY_RELAY_ADDR"); addr != "" {
		os.Exit(runStdioTCPRelay(addr))
	}
	os.Exit(m.Run())
}

// runStdioTCPRelay copies this process's stdin->conn and conn->stdout for a TCP
// conn to addr, mapping a stdin EOF to a TCP half-close — a minimal `nc`-style
// relay so a real ProxyCommand subprocess reaches an in-process test server.
func runStdioTCPRelay(addr string) int {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay dial:", err)
		return 1
	}
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, os.Stdin)
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	<-done
	<-done
	return 0
}

// shellSingleQuote wraps s in single quotes for safe `sh -c` interpolation.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestProxyCommandRealStdioConn (EC13) drives the REAL defaultProxyCommandDialer
// and stdioConn end to end: a real ProxyCommand subprocess (this test binary
// re-exec'd as a stdio<->TCP relay) reaches the in-process target's TCP listener,
// and the whole SSH handshake + an Exec flow over the real stdioConn against the
// REAL knownhosts-backed host-key callback (no WithProxyCommandDialer seam, no
// WithHostKeyCallback override). This catches two production-only regressions the
// loopback-TCP-seam round-trip tests mask: (1) an unsplittable stdioConn
// RemoteAddr (String()=="stdio") failing strict host-key verification for every
// ProxyCommand target, and (2) swapped stdioConn Read/Write/CloseWrite wiring
// breaking the version/KEX exchange.
func TestProxyCommandRealStdioConn(t *testing.T) {
	ctx := context.Background()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey()) // target: reached over its real TCP listener
	bHost, bPort := serverEndpoint(t, b)
	// The relay connects to the target's real listen addr; %h/%p are irrelevant
	// here (the relay reads PROXY_RELAY_ADDR), so use a fixed template.
	cmd := fmt.Sprintf("PROXY_RELAY_ADDR=%s exec %s", b.addr, shellSingleQuote(exe))
	fake := fakeResolver{
		"target": {User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: cmd},
	}
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)
	// No WithProxyCommandDialer: the real subprocess dialer + stdioConn are used,
	// and no host-key override: the real knownhosts callback verifies the target.
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure via real ProxyCommand stdioConn: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via real ProxyCommand stdioConn: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
}

// TestProxyJumpSharedHopDiamond (EC12) proves a hop shared by two sibling
// ProxyJump branches is NOT rejected as a false cycle: `target -> a,b` with
// `a -> bastion` and `b -> bastion` left-expands to [bastion,a,bastion,b] (the
// shared bastion legitimately dialed once per branch, matching OpenSSH's
// per-branch connection context) and the chain reaches the target. The old
// global visited-set aborted this with `proxyjump cycle at "bastion"`.
func TestProxyJumpSharedHopDiamond(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	bastion := newTestServer(t, clientSigner.PublicKey())
	a := newTestServer(t, clientSigner.PublicKey())
	bb := newTestServer(t, clientSigner.PublicKey())
	target := newTestServer(t, clientSigner.PublicKey())
	bastHost, bastPort := serverEndpoint(t, bastion)
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, bb)
	tHost, tPort := serverEndpoint(t, target)
	fake := fakeResolver{
		"target":  {User: "testuser", HostName: tHost, Port: tPort, ProxyJump: "a,b"},
		"a":       {User: "testuser", HostName: aHost, Port: aPort, ProxyJump: "bastion"},
		"b":       {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "bastion"},
		"bastion": {User: "testuser", HostName: bastHost, Port: bastPort},
	}
	kh := writeKnownHostsLines(t, bastion, a, bb, target)
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	// Left-expansion flattens the diamond; the shared bastion appears in BOTH
	// branches without being flagged as a cycle.
	chain, err := c.expandJumpChain(ctx)
	if err != nil {
		t.Fatalf("expandJumpChain of shared-hop diamond: %v", err)
	}
	gotPorts := make([]int, len(chain))
	for i, rh := range chain {
		gotPorts[i] = rh.Port
	}
	wantPorts := []int{bastPort, aPort, bastPort, bPort}
	if len(gotPorts) != len(wantPorts) {
		t.Fatalf("chain ports = %v, want %v (bastion,a,bastion,b)", gotPorts, wantPorts)
	}
	for i := range wantPorts {
		if gotPorts[i] != wantPorts[i] {
			t.Fatalf("chain ports = %v, want %v (bastion,a,bastion,b)", gotPorts, wantPorts)
		}
	}

	if _, err := c.Ensure(ctx); err != nil {
		t.Fatalf("Ensure via shared-hop diamond: %v", err)
	}
	stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
	if err != nil {
		t.Fatalf("Exec via shared-hop diamond: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
	}
}

// TestProxyJumpWrongInnerHopKey (EC12) proves STRICT per-hop verification is
// enforced at an INNER hop (position >= 2), not just the first hop: in the nested
// chain hopa(A) -> hopb(B) -> target(C), a WRONG known_hosts key for the INNER
// hop B (correct keys for A and C) aborts the WHOLE dial. A regression that
// verified only the first hop (InsecureIgnoreHostKey for prev != nil) would
// accept B's wrong key and reach C, so Ensure would wrongly succeed.
func TestProxyJumpWrongInnerHopKey(t *testing.T) {
	ctx := context.Background()
	clientPriv, clientSigner := generateKeyPair(t)
	a := newTestServer(t, clientSigner.PublicKey())  // first hop
	b := newTestServer(t, clientSigner.PublicKey())  // INNER hop (position 2)
	cc := newTestServer(t, clientSigner.PublicKey()) // target
	aHost, aPort := serverEndpoint(t, a)
	bHost, bPort := serverEndpoint(t, b)
	cHost, cPort := serverEndpoint(t, cc)
	fake := fakeResolver{
		"target": {User: "testuser", HostName: cHost, Port: cPort, ProxyJump: "hopb"},
		"hopb":   {User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "hopa"},
		"hopa":   {User: "testuser", HostName: aHost, Port: aPort},
	}
	// WRONG key for the INNER hop B; correct keys for first hop A and target C.
	_, wrongSigner := generateKeyPair(t)
	kh := writeKnownHosts(t, strings.Join([]string{
		a.knownHostsLine(),
		lineFor(b.addr, wrongSigner),
		cc.knownHostsLine(),
	}, "\n"))
	keyFile := writeIdentityFile(t, clientPriv)
	c := newProxyClient(t, fake, keyFile, kh)

	if _, err := c.Ensure(ctx); err == nil {
		t.Fatal("Ensure with wrong INNER hop key: want host-key error, got nil")
	} else if !strings.Contains(err.Error(), "host key verification failed") {
		t.Errorf("error = %v, want host-key verification failure", err)
	}
	assertNothingStored(t, c)
}
