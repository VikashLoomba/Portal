package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/agentclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/bootstrap"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clock"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/forward"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
)

// onceStreamTransport wires each ExecStream to a REAL agent.Server over io.Pipe
// pairs (the agentclient/client_test.go pattern), counting connections so a test
// can assert whether `once` spun the AgentClient up at all. The connect count
// stays 0 on the daemon-up path (which must NOT start a second AgentClient) and
// goes positive on the daemon-down fallback (which does).
type onceStreamTransport struct {
	w        *watcher.Fake
	sha      string
	connects atomic.Int64
}

func (t *onceStreamTransport) Host() string                                    { return "fakehost" }
func (t *onceStreamTransport) Sock() string                                    { return "/tmp/fake-sock" }
func (t *onceStreamTransport) MasterPID(context.Context) (int, error)          { return 1, nil }
func (t *onceStreamTransport) EnsureMaster(context.Context) (int, bool, error) { return 1, false, nil }
func (t *onceStreamTransport) Forward(context.Context, int, int) error         { return nil }
func (t *onceStreamTransport) Cancel(context.Context, int, int) error          { return nil }
func (t *onceStreamTransport) Exit(context.Context) (bool, error)              { return false, nil }
func (t *onceStreamTransport) Exec(context.Context, string, ...string) (string, error) {
	return "", nil
}
func (t *onceStreamTransport) ExecBytes(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}

func (t *onceStreamTransport) ExecStream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	t.connects.Add(1)
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go stderrW.Close() // stderr is unused in this harness

	srv := agent.New(agent.Config{
		In:                c2aR,
		Out:               a2cW,
		Watcher:           t.w,
		AgentSHA:          t.sha,
		HeartbeatInterval: time.Hour,
	})
	go func() {
		_ = srv.Serve(ctx)
		_ = a2cW.Close()
	}()
	wait := func() error { c2aR.Close(); return nil }
	return c2aW, a2cR, stderrR, wait, nil
}

func (t *onceStreamTransport) connectCount() int { return int(t.connects.Load()) }

var _ sshctl.Transport = (*onceStreamTransport)(nil)

// onceProbeTransport answers the bootstrap stat-probe with the embedded agent's
// byte size so EnsureUploaded short-circuits (no upload). Mirrors client_test.go
// probeOKTransport; it never streams — the real agent is wired by
// onceStreamTransport.
type onceProbeTransport struct{}

func (onceProbeTransport) Host() string                                    { return "p" }
func (onceProbeTransport) Sock() string                                    { return "/tmp/p" }
func (onceProbeTransport) MasterPID(context.Context) (int, error)          { return 1, nil }
func (onceProbeTransport) EnsureMaster(context.Context) (int, bool, error) { return 1, false, nil }
func (onceProbeTransport) Forward(context.Context, int, int) error         { return nil }
func (onceProbeTransport) Cancel(context.Context, int, int) error          { return nil }
func (onceProbeTransport) Exit(context.Context) (bool, error)              { return false, nil }
func (onceProbeTransport) Exec(context.Context, string, ...string) (string, error) {
	return strconv.Itoa(len(bootstrap.EmbeddedAgent())) + "\n", nil
}
func (onceProbeTransport) ExecBytes(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (onceProbeTransport) ExecStream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

// newOnceAgentClient builds a real agentclient.Client wired to the agent-over-
// pipes harness with a short reconnect floor so Run connects and snapshots
// within milliseconds. sha must equal bootstrap.EmbeddedSHA() or the client
// rejects the handshake (SHA-mismatch guard).
func newOnceAgentClient(t *testing.T, sha string) (*agentclient.Client, *onceStreamTransport) {
	t.Helper()
	w := watcher.NewFake()
	w.SetSnapshot([]watcher.Listen{{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	tr := &onceStreamTransport{w: w, sha: sha}
	ac := agentclient.New(agentclient.Config{
		Transport:        tr,
		Bootstrap:        bootstrap.New(onceProbeTransport{}, nil),
		HeartbeatTimeout: 5 * time.Second,
		ReconnectMin:     5 * time.Millisecond,
		ReconnectMax:     20 * time.Millisecond,
	})
	return ac, tr
}

// runOnceCmd drives newOnceCmd(a) through cobra with capture buffers so
// cmd.OutOrStdout()/ErrOrStderr() are &out/&errw — status is observable via out
// on both branches (the reviewer fix routes both through runStatusTo).
func runOnceCmd(ctx context.Context, a *app.App, out, errw *bytes.Buffer) error {
	cmd := newOnceCmd(a)
	cmd.SetOut(out)
	cmd.SetErr(errw)
	cmd.SetArgs(nil)
	return cmd.ExecuteContext(ctx)
}

// EC (once, daemon up): `once` POSTs /v1/reconcile (Kick advances) and renders
// status from the socket (agent line present) WITHOUT spinning a second
// AgentClient — the AgentClient's transport must see zero connects.
func TestOnce_DaemonUp_KicksAndRendersFromSocket(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg,
		withHelloAck(&protocol.HelloAck{
			AgentPID:    4321,
			AgentGitSHA: "0123456789abcdef",
			Kernel:      "Linux-test",
		}),
		withMasterPID(4242),
	)
	a := newDaemonTestApp(t, d.path, cfg)
	// A real AgentClient the up-path must NOT run: the SHA is irrelevant because
	// ExecStream is never reached; the connect counter proves it stayed idle.
	ac, tr := newOnceAgentClient(t, "irrelevant-up-path")
	a.AgentClient = ac

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runOnceCmd(ctx, a, &out, &errw); err != nil {
		t.Fatalf("once (daemon up): %v", err)
	}

	if got := d.kickCount(); got < 1 {
		t.Errorf("daemon Kick count = %d, want >= 1 (reconcile must reach the daemon)", got)
	}
	if got := tr.connectCount(); got != 0 {
		t.Errorf("AgentClient connect count = %d, want 0 (no second client against the same box)", got)
	}
	if want := "agent: pid=4321 sha=0123456789ab kernel=Linux-test\n"; !strings.Contains(out.String(), want) {
		t.Errorf("status missing socket-sourced agent line %q\n--- got ---\n%s", want, out.String())
	}
	if errw.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errw.String())
	}
}

// EC (once, daemon down): with no socket, `once` falls back to the short-lived
// AgentClient (connect count > 0), waitForSnapshot succeeds fast, and a status
// IS printed to the command out buffer (capturable now that the fallback renders
// via runStatusTo(cmd.OutOrStdout()) — previously it wrote os.Stdout). No
// socket-related error spam on stderr.
func TestOnce_DaemonDown_FallbackRendersToOut(t *testing.T) {
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}
	cfg := newTestConfig(t, "devbox")
	// Point APISock at a nonexistent path so Available() is a fast dial failure.
	a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)
	a.Clk = clock.Real{}
	a.Log = &forward.MemLogger{}
	ac, tr := newOnceAgentClient(t, sha)
	a.AgentClient = ac

	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runOnceCmd(ctx, a, &out, &errw); err != nil {
		t.Fatalf("once (daemon down): %v", err)
	}

	if got := tr.connectCount(); got < 1 {
		t.Errorf("AgentClient connect count = %d, want >= 1 (fallback spins the short-lived client)", got)
	}
	if !strings.Contains(out.String(), "dev box: devbox\n") {
		t.Errorf("fallback status not rendered to out buffer\n--- got ---\n%s", out.String())
	}
	// The fallback path must not spam stderr with socket errors. waitForSnapshot
	// may in principle warn, but with a fast agent it should not; assert the only
	// tolerated content is absent noise.
	if strings.Contains(errw.String(), "localapi") || strings.Contains(errw.String(), "socket") {
		t.Errorf("unexpected socket error spam on stderr: %q", errw.String())
	}
}
