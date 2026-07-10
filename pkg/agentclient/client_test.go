package agentclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/internal/bootstrap"
	"github.com/VikashLoomba/Portal/pkg/agent"
	"github.com/VikashLoomba/Portal/pkg/agent/watcher"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

// fakeStreamTransport implements transport.Transport for the client test. It
// pairs the client's Stream call with an in-process agent.Server using
// io.Pipe rather than launching a real ssh process.
type fakeStreamTransport struct {
	w *watcher.Fake
}

func (f *fakeStreamTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (f *fakeStreamTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}
func (f *fakeStreamTransport) Exec(_ context.Context, _ []byte, _ ...string) (string, string, error) {
	return "", "", nil
}
func (f *fakeStreamTransport) Close(context.Context) (bool, error) { return false, nil }
func (f *fakeStreamTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/fake-sock"}
}

// Stream wires the agent.Server to the returned (stdin, stdout)
// pipes via io.Pipe pairs. The agent runs in a goroutine and exits when
// stdin closes.
func (f *fakeStreamTransport) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go stderrW.Close() // we don't exercise stderr in this test

	srv := agent.New(agent.Config{
		In:                c2aR,
		Out:               a2cW,
		Watcher:           f.w,
		AgentSHA:          "fakebootstrapSHA",
		HeartbeatInterval: time.Hour,
	})
	go func() {
		_ = srv.Serve(ctx)
		_ = a2cW.Close()
	}()
	wait := func() error { c2aR.Close(); return nil }
	return c2aW, a2cR, stderrR, wait, nil
}

// fakeBootstrap is a no-op bootstrap that always returns "ok".
type fakeBootstrap struct{}

func (fakeBootstrap) EnsureUploaded(context.Context) (string, error) {
	return "/agent-fake", nil
}

// We need to hijack bootstrap.EmbeddedSHA so the SHA assertion in client
// passes. The simplest way is to compile a test-only build that replaces
// the package-level gitSHA variable. Since we can't do that cleanly here,
// we test against the real EmbeddedSHA() and pass it through to the agent
// — both sides see the same string.
func TestClient_HandshakeSnapshotDelta(t *testing.T) {
	// Pre-condition: bootstrap.EmbeddedSHA() returns the real SHA used by
	// `make agent`. Wire the agent to report the SAME SHA so the client's
	// mismatch check passes.
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}
	w := watcher.NewFake()
	w.SetSnapshot([]watcher.Listen{{Port: 8081, Family: 4, Addr: "127.0.0.1"}})

	tr := &shaOverridingTransport{base: &fakeStreamTransport{w: w}, sha: sha}

	bs := &realBootstrapShim{path: "/agent-" + sha}
	c := New(Config{
		Transport:        tr,
		Bootstrap:        nil, // placeholder; we install a fake below
		HeartbeatTimeout: 5 * time.Second,
	})
	c.cfg.Bootstrap = (*bootstrap.Manager)(nil) // disable real upload
	// Override the bootstrap pointer via reflection-free shim.
	c.cfg.Bootstrap = bs.asManager()
	_ = bs // keep alive

	// Set initial subscribe filter.
	if err := c.Subscribe([]uint16{22}, []uint16{40085}, true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.Run(ctx)
	}()

	// Wait for first snapshot.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if seq, ports, ok := c.Snapshot(); ok && seq > 0 {
			if len(ports) != 1 || ports[0] != 8081 {
				t.Errorf("snapshot: got %v, want [8081]", ports)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, ok := c.Snapshot(); !ok {
		t.Fatal("never received snapshot")
	}

	// Emit a delta: PortAdded.
	w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8082, Family: 4, Addr: "127.0.0.1"}})

	// Verify a Delta event arrives.
	saw := false
	for !saw {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for delta")
		case ev := <-c.Events():
			if ev.Kind == KindDelta {
				if len(ev.Added) == 1 && ev.Added[0] == 8082 {
					saw = true
				}
			}
		}
	}

	cancel()
	wg.Wait()
}

// shaOverridingTransport injects a known agent SHA into the underlying
// fakeStreamTransport's agent.Server config. We achieve this by rebuilding
// the agent.Server inside ExecStream with the desired SHA.
type shaOverridingTransport struct {
	base *fakeStreamTransport
	sha  string
}

func (s *shaOverridingTransport) Ensure(c context.Context) (bool, error) { return s.base.Ensure(c) }
func (s *shaOverridingTransport) Health(c context.Context) (transport.Health, error) {
	return s.base.Health(c)
}
func (s *shaOverridingTransport) Exec(c context.Context, in []byte, av ...string) (string, string, error) {
	return s.base.Exec(c, in, av...)
}
func (s *shaOverridingTransport) Close(c context.Context) (bool, error) { return s.base.Close(c) }
func (s *shaOverridingTransport) Describe() transport.Desc              { return s.base.Describe() }
func (s *shaOverridingTransport) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go stderrW.Close()
	srv := agent.New(agent.Config{
		In:                c2aR,
		Out:               a2cW,
		Watcher:           s.base.w,
		AgentSHA:          s.sha,
		HeartbeatInterval: time.Hour,
	})
	go func() {
		_ = srv.Serve(ctx)
		_ = a2cW.Close()
	}()
	wait := func() error { c2aR.Close(); return nil }
	return c2aW, a2cR, stderrR, wait, nil
}

// realBootstrapShim is a tiny no-op stand-in for *bootstrap.Manager. We
// can't construct a Manager with a fake Transport without exposing more
// of the bootstrap API than we want; instead we wrap a fake. The real
// concern is that EnsureUploaded returns a path — the client doesn't care
// about its content because our shim Transport ignores the argv anyway.
type realBootstrapShim struct{ path string }

// asManager returns an actual *bootstrap.Manager whose probe and upload
// will succeed against the fakeStreamTransport. We construct one with a
// nil logger; bootstrap.New is OK with that.
func (r *realBootstrapShim) asManager() *bootstrap.Manager {
	// Use a no-op transport whose Exec returns the matching size so the
	// stat-probe succeeds and no upload happens.
	bs := bootstrap.New(&probeOKTransport{path: r.path}, nil)
	return bs
}

// probeOKTransport's Exec returns the expected byte size so EnsureUploaded
// short-circuits on the probe.
type probeOKTransport struct{ path string }

func (p *probeOKTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (p *probeOKTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}
func (p *probeOKTransport) Exec(_ context.Context, _ []byte, argv ...string) (string, string, error) {
	if len(argv) > 0 && argv[0] == "uname" {
		return "Linux x86_64\n", "", nil
	}
	return fmt.Sprintf("%d\n", len(bootstrap.EmbeddedAgent())), "", nil
}
func (p *probeOKTransport) Close(context.Context) (bool, error) { return false, nil }
func (p *probeOKTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "p", Endpoint: "/tmp/p"}
}
func (p *probeOKTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

// silence unused import.
var _ = os.Getpid
var _ = protocol.ProtoVersion
var _ transport.Transport = (*shaOverridingTransport)(nil)

// ---------------------------------------------------------------------------
// u6: full-stack v4 exit-criteria suite (real agent.Server + real Client over
// io.Pipe, driven through the agent's live cmd socket).
// ---------------------------------------------------------------------------

// shortSockPath returns a Unix-socket path under a SHORT temp dir. macOS caps
// sun_path at 104 bytes, and t.TempDir() embeds the (long) test name, which
// overflows the limit — so we mint our own short dir (mirrors the agent tests).
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ptle2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "cmd.sock")
}

// e2eTransport wires the Client to a REAL agent.Server over io.Pipe, giving the
// agent a live cmd socket so the test can drive open/notify/clip verbs, plus a
// configurable SHA, heartbeat interval, and slog sink. It embeds
// fakeStreamTransport for the trivial Transport methods and overrides Stream.
type e2eTransport struct {
	*fakeStreamTransport
	sha      string
	sockPath string
	agentLog *slog.Logger
	hb       time.Duration
}

// Stream OVERRIDES the embedded fakeStreamTransport.Stream with the live
// agent.Server wiring. It MUST stay named Stream (not ExecStream) — an
// ExecStream method here would be an orphan that no longer overrides the
// embedded Stream, silently reverting the e2e tests to the canned fake pipe.
func (e *e2eTransport) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go stderrW.Close()
	srv := agent.New(agent.Config{
		In: c2aR, Out: a2cW, Watcher: e.w, AgentSHA: e.sha,
		HeartbeatInterval: e.hb, CmdSockPath: e.sockPath, Log: e.agentLog,
	})
	go func() { _ = srv.Serve(ctx); _ = a2cW.Close() }()
	wait := func() error { c2aR.Close(); return nil }
	return c2aW, a2cR, stderrR, wait, nil
}

var _ transport.Transport = (*e2eTransport)(nil)

// e2eSession bundles a running Client bound to a real agent over the e2e
// transport, plus the agent's cmd-socket path and the shared fake watcher.
type e2eSession struct {
	c        *Client
	sockPath string
	w        *watcher.Fake
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
}

// newE2ESession builds a Client + real agent, runs Run in a goroutine, and blocks
// until the handshake+snapshot landed and the cmd socket is up. mutate (optional)
// tweaks the Client between New and Run — the in-package seam for swapping/reset-
// ting handlers before the session starts.
func newE2ESession(t *testing.T, clientLog, agentLog *slog.Logger, mutate func(*Client)) *e2eSession {
	t.Helper()
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	sockPath := shortSockPath(t)
	tr := &e2eTransport{
		fakeStreamTransport: &fakeStreamTransport{w: w},
		sha:                 sha,
		sockPath:            sockPath,
		agentLog:            agentLog,
		hb:                  100 * time.Millisecond,
	}
	if clientLog == nil {
		clientLog = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := New(Config{
		Transport:        tr,
		HeartbeatTimeout: 5 * time.Second,
		Log:              clientLog,
	})
	c.cfg.Bootstrap = (&realBootstrapShim{path: "/agent-" + sha}).asManager()
	if mutate != nil {
		mutate(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Run(ctx) }()

	sess := &e2eSession{c: c, sockPath: sockPath, w: w, cancel: cancel, wg: &wg}
	sess.waitConnected(t)
	sess.waitSock(t)
	return sess
}

func (s *e2eSession) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, ok := s.c.Snapshot(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("client never reached connected+snapshot state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *e2eSession) waitSock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if conn, err := net.DialTimeout("unix", s.sockPath, 100*time.Millisecond); err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("agent cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *e2eSession) close() {
	s.cancel()
	s.wg.Wait()
}

// ask dials the agent's cmd socket, writes line, and returns the raw reply.
func (s *e2eSession) ask(t *testing.T, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", s.sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial cmd sock: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write verb: %v", err)
	}
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

// serveClip drains the client's dedicated clip channel and answers every request
// with OK+sha via SendClipResponse — the in-test stand-in for runClipHandler.
func (s *e2eSession) serveClip(ctx context.Context, sha string) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-s.c.ClipEvents():
				if ev.Clip == nil {
					continue
				}
				_ = s.c.SendClipResponse(&protocol.ClipResponse{
					Nonce: ev.Clip.Nonce, Epoch: ev.Clip.Epoch, OK: true, SHA: sha,
				})
			}
		}
	}()
}

// serveCred drains the client's dedicated credential channel and approves each
// request with the supplied fake bytes through SendCredResponse.
func (s *e2eSession) serveCred(ctx context.Context, secret []byte) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-s.c.CredEvents():
				if ev.Cred == nil {
					continue
				}
				_ = s.c.SendCredResponse(&protocol.CredResponse{
					Nonce: ev.Cred.Nonce, Epoch: ev.Cred.Epoch, OK: true,
					Secret: append([]byte(nil), secret...),
				})
			}
		}
	}()
}

func credRequestLine(t *testing.T, req agent.CredShimReq) string {
	t.Helper()
	payload, err := protocol.MarshalPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return "cred\t" + base64.StdEncoding.EncodeToString(payload) + "\n"
}

// waitEvent reads the shared events channel until an event of kind arrives.
func (s *e2eSession) waitEvent(t *testing.T, kind EngineEventKind) EngineEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.c.Events():
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("did not observe event kind %d", kind)
			return EngineEvent{}
		}
	}
}

// waitNotify reads the dedicated notify channel.
func (s *e2eSession) waitNotify(t *testing.T) EngineEvent {
	t.Helper()
	select {
	case ev := <-s.c.NotifyEvents():
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no notify event")
		return EngineEvent{}
	}
}

func containsPort(ps []uint16, p uint16) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

// openurlHandler builds a client-side openurl HandlerSpec at the given version
// with the supplied Deliver — the seam u6 tests use to swap in a panicking or
// version-mismatched handler while keeping the real Decode.
func openurlHandler(version uint32, deliver func(EngineEvent)) HandlerSpec {
	return HandlerSpec{
		Service: "openurl", Version: version, MaxPayload: 8192,
		Decode: func(_ uint64, payload cbor.RawMessage) (EngineEvent, error) {
			ou, err := protocol.UnmarshalPayload[protocol.OpenURL](payload)
			if err != nil {
				return EngineEvent{}, err
			}
			return EngineEvent{Kind: KindOpenURL, URL: ou.URL}, nil
		},
		Deliver: deliver,
	}
}

// TestE2EAllFourServicesRoundTrip drives openurl, notify, clip, and cred end to
// end over a real agent+client pipe, both auto-registering all four services.
// Every feature crosses via Msg frames ONLY (the legacy Envelope fields are
// deleted — compiler-enforced, and asserted by the agent's deletion-invariant
// test). HelloAck.Services proves the reverse-direction advertisement; the cmd
// replies prove the agent recorded the client's services (forward).
func TestE2EAllFourServicesRoundTrip(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef"
	secret := []byte("s3kr3t-vector")
	sess := newE2ESession(t, nil, nil, nil)
	defer sess.close()

	ack := sess.c.HelloAck()
	if ack == nil {
		t.Fatal("no HelloAck")
	}
	for _, svc := range []string{"openurl", "notify", "clip", "cred"} {
		if v, ok := ack.Services[svc]; !ok || v != 1 {
			t.Fatalf("HelloAck.Services[%q] = %d (present %v), want 1", svc, v, ok)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.serveClip(ctx, sha)
	sess.serveCred(ctx, secret)

	// openurl.
	if got := sess.ask(t, "open\thttp://example.com/x\n"); got != "ok\n" {
		t.Fatalf("open = %q, want ok\\n", got)
	}
	if ev := sess.waitEvent(t, KindOpenURL); ev.URL != "http://example.com/x" {
		t.Fatalf("KindOpenURL URL = %q", ev.URL)
	}

	// notify.
	if got := sess.ask(t, "notify\t{\"title\":\"hi\",\"body\":\"b\",\"verified\":true}\n"); got != "ok\n" {
		t.Fatalf("notify = %q, want ok\\n", got)
	}
	if ev := sess.waitNotify(t); ev.Notify == nil || ev.Notify.Title != "hi" || !ev.Notify.Verified {
		t.Fatalf("KindNotify = %+v", ev.Notify)
	}

	// clip.
	if got := sess.ask(t, "clip\timage\tpng\n"); got != "ok\t"+sha+"\n" {
		t.Fatalf("clip = %q, want ok\\t<sha>\\n", got)
	}

	// cred.
	wantCred := "ok\t" + base64.StdEncoding.EncodeToString(secret) + "\n"
	if got := sess.ask(t, credRequestLine(t, agent.CredShimReq{
		Label: "database", Mode: "env", Target: "PW", Requester: "pid 42: sh",
	})); got != wantCred {
		t.Fatal("credential response did not carry the approved fake bytes")
	}
}

// TestZeroServiceConsumer (EC8) resets the Client's registry to EMPTY before Run
// (a consumer that registers no service handlers). The handshake still completes
// and Snapshot/PortAdded/PortRemoved flow normally; the agent answers its cmd
// verbs no-client/none for the unadvertised services (hasClient() true,
// clientHas() false).
func TestZeroServiceConsumer(t *testing.T) {
	sess := newE2ESession(t, nil, nil, func(c *Client) {
		c.registry = newRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	defer sess.close()

	if sess.c.HelloAck() == nil {
		t.Fatal("handshake did not complete for zero-service consumer")
	}

	sess.w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	if ev := sess.waitEvent(t, KindDelta); !containsPort(ev.Added, 8081) {
		t.Fatalf("KindDelta Added = %v, want 8081", ev.Added)
	}
	sess.w.Emit(watcher.Event{Kind: watcher.KindRemove, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	if ev := sess.waitEvent(t, KindDelta); !containsPort(ev.Removed, 8081) {
		t.Fatalf("KindDelta Removed = %v, want 8081", ev.Removed)
	}

	if got := sess.ask(t, "open\thttp://example.com\n"); got != "no-client\n" {
		t.Fatalf("open = %q, want no-client\\n", got)
	}
	if got := sess.ask(t, "clip\timage\tpng\n"); got != "none\n" {
		t.Fatalf("clip = %q, want none\\n", got)
	}
	if got := sess.ask(t, "notify\t{\"title\":\"hi\"}\n"); got != "no-client\n" {
		t.Fatalf("notify = %q, want no-client\\n", got)
	}
	if got := sess.ask(t, credRequestLine(t, agent.CredShimReq{
		Label: "database", Mode: "env", Target: "PW",
	})); got != "deny\tno-client\n" {
		t.Fatalf("cred = %q, want deny\\tno-client\\n", got)
	}
}

// TestPanicIsolationBothEnds (EC6) swaps the Client's openurl handler for one
// whose Deliver panics, then drives an open. The client dispatch recover drops
// only that frame; the session survives on BOTH ends — notify still round-trips
// (client end alive) and clip still round-trips (agent end alive), and the
// session never disconnects.
func TestPanicIsolationBothEnds(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccc"
	sess := newE2ESession(t, nil, nil, func(c *Client) {
		c.registry.register(openurlHandler(1, func(EngineEvent) {
			panic("deliberate client openurl panic")
		}))
	})
	defer sess.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.serveClip(ctx, sha)

	if got := sess.ask(t, "open\thttp://example.com/boom\n"); got != "ok\n" {
		t.Fatalf("open = %q, want ok\\n (agent side unaffected)", got)
	}

	if got := sess.ask(t, "notify\t{\"title\":\"after\"}\n"); got != "ok\n" {
		t.Fatalf("notify after panic = %q, want ok\\n", got)
	}
	if ev := sess.waitNotify(t); ev.Notify == nil || ev.Notify.Title != "after" {
		t.Fatalf("notify after panic not delivered: %+v", ev.Notify)
	}
	if got := sess.ask(t, "clip\timage\tpng\n"); got != "ok\t"+sha+"\n" {
		t.Fatalf("clip after panic = %q, want ok\\t<sha>\\n", got)
	}

	if e := sess.c.LastDisconnectErr(); e != "" {
		t.Fatalf("session disconnected during panic isolation: %q", e)
	}
}

// TestServiceVersionMismatchDisable (EC10) advertises the client's openurl at
// version 2 while the agent registered it at 1. Exact-equality means BOTH sides
// treat openurl as absent: exactly one warning each side, the open verb answers
// no-client, and the other two services keep working with a healthy session.
func TestServiceVersionMismatchDisable(t *testing.T) {
	const sha = "dddddddddddddddddddddddddddddddd"
	agentLog := &countHandler{}
	clientLog := &countHandler{}
	sess := newE2ESession(t, slog.New(clientLog), slog.New(agentLog), func(c *Client) {
		c.registry.register(openurlHandler(2, c.publish))
	})
	defer sess.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.serveClip(ctx, sha)

	if got := sess.ask(t, "open\thttp://example.com\n"); got != "no-client\n" {
		t.Fatalf("open (version-mismatched) = %q, want no-client\\n", got)
	}

	if got := sess.ask(t, "notify\t{\"title\":\"ok\"}\n"); got != "ok\n" {
		t.Fatalf("notify = %q, want ok\\n", got)
	}
	if ev := sess.waitNotify(t); ev.Notify == nil || ev.Notify.Title != "ok" {
		t.Fatalf("notify not delivered: %+v", ev.Notify)
	}
	if got := sess.ask(t, "clip\timage\tpng\n"); got != "ok\t"+sha+"\n" {
		t.Fatalf("clip = %q, want ok\\t<sha>\\n", got)
	}

	// Exactly one warning on EACH side (agent: setClientServices mismatch;
	// client: dormant-handler). Poll to let async handshake logging land, then pin
	// — read BEFORE close() so Run's "session ended" warn can't inflate the count.
	deadline := time.Now().Add(3 * time.Second)
	for (agentLog.count() < 1 || clientLog.count() < 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := agentLog.count(); got != 1 {
		t.Fatalf("agent warnings = %d, want exactly 1", got)
	}
	if got := clientLog.count(); got != 1 {
		t.Fatalf("client warnings = %d, want exactly 1", got)
	}
}

// TestClientNotify_SurfacesMsgSeq exercises the REAL client notify handler
// (registered in New) through registry.dispatch and asserts NotifyEvent.Seq is
// the registry-stamped Msg.Seq — NOT the payload field, which svc_notify never
// sets. This guards the v4 regression where the client read the always-0 payload
// Seq, collapsing every /v1/events notify line to seq:0.
func TestClientNotify_SurfacesMsgSeq(t *testing.T) {
	c := New(Config{})
	// Payload carries NO seq (mirrors svc_notify, which stamps only Msg.Seq).
	payload, err := protocol.MarshalPayload(protocol.Notify{Title: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []uint64{7, 8} {
		c.registry.dispatch(&protocol.Msg{Service: "notify", Kind: "event", Seq: want, Payload: payload})
		select {
		case ev := <-c.NotifyEvents():
			if ev.Notify == nil {
				t.Fatalf("notify not delivered for seq %d", want)
			}
			if ev.Notify.Seq != want {
				t.Fatalf("NotifyEvent.Seq = %d, want %d (registry-stamped Msg.Seq)", ev.Notify.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no notify event delivered for seq %d", want)
		}
	}
}

// TestClientCred_DecodePublish exercises the real built-in cred HandlerSpec and
// pins the complete CredRequest→dedicated-channel field mapping.
func TestClientCred_DecodePublish(t *testing.T) {
	c := New(Config{})
	payload, err := protocol.MarshalPayload(protocol.CredRequest{
		Nonce: 11, Epoch: 12, Label: "staging admin", Requester: "pid 42: sh",
		Mode: "env", Target: "PW",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.registry.dispatch(&protocol.Msg{Service: "cred", Kind: "req", Payload: payload})
	select {
	case ev := <-c.CredEvents():
		if ev.Kind != KindCredRequest || ev.Cred == nil {
			t.Fatal("credential request was not published on its dedicated channel")
		}
		got := ev.Cred
		if got.Nonce != 11 || got.Epoch != 12 || got.Label != "staging admin" ||
			got.Requester != "pid 42: sh" || got.Mode != "env" || got.Target != "PW" {
			t.Fatal("credential event fields did not match the decoded request")
		}
	case <-time.After(time.Second):
		t.Fatal("no credential event delivered")
	}
}

// TestClientCred_SendBeforeConnect pins the response facade's connection error
// contract, matching SendClipResponse.
func TestClientCred_SendBeforeConnect(t *testing.T) {
	c := New(Config{})
	err := c.SendCredResponse(&protocol.CredResponse{Nonce: 1, Epoch: 2, Err: "denied"})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("SendCredResponse before connect: want ErrNotConnected, got %v", err)
	}
}

// TestClientOpenURL_LongURLSurvivesPayloadCap delivers a URL near the agent's
// 4096-byte cmd-socket line, whose CBOR-framed payload exceeds 4096, and asserts
// the client openurl handler still admits it. Guards the regression where the
// client cap was pinned at the raw socket-read size (4096) and silently dropped
// any URL a caller could actually pass to `portald open`.
func TestClientOpenURL_LongURLSurvivesPayloadCap(t *testing.T) {
	c := New(Config{})
	url := "http://host/?q=" + strings.Repeat("a", 4070)
	payload, err := protocol.MarshalPayload(protocol.OpenURL{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 4096 {
		t.Fatalf("payload %d bytes is not over the old 4096 cap; test would not guard the regression", len(payload))
	}
	c.registry.dispatch(&protocol.Msg{Service: "openurl", Kind: "open", Payload: payload})
	select {
	case ev := <-c.Events():
		if ev.Kind != KindOpenURL || ev.URL != url {
			t.Fatalf("delivered %+v, want KindOpenURL carrying the long URL", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("long URL was dropped by the client payload cap")
	}
}
