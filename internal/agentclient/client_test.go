package agentclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vikashl/portal/internal/agent"
	"github.com/vikashl/portal/internal/agent/watcher"
	"github.com/vikashl/portal/internal/bootstrap"
	"github.com/vikashl/portal/internal/protocol"
	"github.com/vikashl/portal/internal/sshctl"
)

// fakeStreamTransport implements sshctl.Transport for the client test. It
// pairs the client's ExecStream call with an in-process agent.Server using
// io.Pipe rather than launching a real ssh process.
type fakeStreamTransport struct {
	w *watcher.Fake
}

func (f *fakeStreamTransport) Host() string                                  { return "fakehost" }
func (f *fakeStreamTransport) Sock() string                                  { return "/tmp/fake-sock" }
func (f *fakeStreamTransport) MasterPID(context.Context) (int, error)        { return 1, nil }
func (f *fakeStreamTransport) EnsureMaster(context.Context) (int, bool, error) { return 1, false, nil }
func (f *fakeStreamTransport) Forward(context.Context, int, int) error        { return nil }
func (f *fakeStreamTransport) Cancel(context.Context, int, int) error         { return nil }
func (f *fakeStreamTransport) Exit(context.Context) (bool, error)             { return false, nil }
func (f *fakeStreamTransport) Exec(_ context.Context, _ string, _ ...string) (string, error) {
	return "", nil
}
func (f *fakeStreamTransport) ExecBytes(_ context.Context, _ []byte, _ ...string) (string, string, error) {
	return "", "", nil
}

// ExecStream wires the agent.Server to the returned (stdin, stdout)
// pipes via io.Pipe pairs. The agent runs in a goroutine and exits when
// stdin closes.
func (f *fakeStreamTransport) ExecStream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
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

func (s *shaOverridingTransport) Host() string                                  { return s.base.Host() }
func (s *shaOverridingTransport) Sock() string                                  { return s.base.Sock() }
func (s *shaOverridingTransport) MasterPID(c context.Context) (int, error)     { return s.base.MasterPID(c) }
func (s *shaOverridingTransport) EnsureMaster(c context.Context) (int, bool, error) { return s.base.EnsureMaster(c) }
func (s *shaOverridingTransport) Forward(c context.Context, l, r int) error    { return s.base.Forward(c, l, r) }
func (s *shaOverridingTransport) Cancel(c context.Context, l, r int) error     { return s.base.Cancel(c, l, r) }
func (s *shaOverridingTransport) Exit(c context.Context) (bool, error)         { return s.base.Exit(c) }
func (s *shaOverridingTransport) Exec(c context.Context, in string, av ...string) (string, error) {
	return s.base.Exec(c, in, av...)
}
func (s *shaOverridingTransport) ExecBytes(c context.Context, b []byte, av ...string) (string, string, error) {
	return s.base.ExecBytes(c, b, av...)
}
func (s *shaOverridingTransport) ExecStream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
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

func (p *probeOKTransport) Host() string                                  { return "p" }
func (p *probeOKTransport) Sock() string                                  { return "/tmp/p" }
func (p *probeOKTransport) MasterPID(context.Context) (int, error)        { return 1, nil }
func (p *probeOKTransport) EnsureMaster(context.Context) (int, bool, error) { return 1, false, nil }
func (p *probeOKTransport) Forward(context.Context, int, int) error        { return nil }
func (p *probeOKTransport) Cancel(context.Context, int, int) error         { return nil }
func (p *probeOKTransport) Exit(context.Context) (bool, error)             { return false, nil }
func (p *probeOKTransport) Exec(_ context.Context, _ string, _ ...string) (string, error) {
	return fmt.Sprintf("%d\n", len(bootstrap.EmbeddedAgent())), nil
}
func (p *probeOKTransport) ExecBytes(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (p *probeOKTransport) ExecStream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

// silence unused import.
var _ = os.Getpid
var _ = protocol.ProtoVersion
var _ sshctl.Transport = (*shaOverridingTransport)(nil)
