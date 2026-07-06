package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/agent"
	"github.com/VikashLoomba/Portal/internal/agent/watcher"
	"github.com/VikashLoomba/Portal/internal/agentclient"
	"github.com/VikashLoomba/Portal/internal/bootstrap"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/doctor"
	"github.com/VikashLoomba/Portal/internal/hub"
	"github.com/VikashLoomba/Portal/internal/protocol"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// integStreamTransport is the transport.Transport for the full-stack integration
// harness: each Stream wires a REAL agent.Server over io.Pipe pairs (as
// agentclient/client_test.go does) instead of launching ssh. A drop() cancels
// the current agent's context so agentclient's supervisor reconnects — letting
// the events test observe a coalesced state line after a simulated reconnect.
type integStreamTransport struct {
	// snapshot seeds a FRESH watcher.Fake per connection: a single Fake shared
	// across two live agents (the dropped one + the reconnected one) races, so
	// each ExecStream gets its own watcher carrying the same listener set.
	snapshot []watcher.Listen
	sha      string

	mu       sync.Mutex
	dropCur  func() // cancels the current agent's Serve; nil before first connect
	connects int    // number of ExecStream calls (i.e. connections)
}

func (t *integStreamTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (t *integStreamTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 4242, Detail: "pid=4242"}, nil
}
func (t *integStreamTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (t *integStreamTransport) Close(context.Context) (bool, error) { return false, nil }
func (t *integStreamTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/fake-sock"}
}

// Stream wires a fresh agent.Server to the returned pipes. The agent runs
// under a per-connection cancelable context recorded in dropCur so a test can
// force a session drop.
func (t *integStreamTransport) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go stderrW.Close() // stderr is unused in this harness

	w := watcher.NewFake()
	w.SetSnapshot(t.snapshot)
	actx, acancel := context.WithCancel(ctx)
	srv := agent.New(agent.Config{
		In:                c2aR,
		Out:               a2cW,
		Watcher:           w,
		AgentSHA:          t.sha,
		EphemMin:          32768,
		EphemMax:          60999,
		HeartbeatInterval: time.Hour,
	})
	go func() {
		_ = srv.Serve(actx)
		_ = a2cW.Close() // client's demux read → EOF → runOnce returns → reconnect
	}()

	t.mu.Lock()
	t.connects++
	t.dropCur = acancel
	t.mu.Unlock()

	wait := func() error { c2aR.Close(); return nil }
	return c2aW, a2cR, stderrR, wait, nil
}

// drop cancels the current agent's context, forcing the client to reconnect.
func (t *integStreamTransport) drop() {
	t.mu.Lock()
	d := t.dropCur
	t.mu.Unlock()
	if d != nil {
		d()
	}
}

// connectCount returns how many ExecStream connections have been made.
func (t *integStreamTransport) connectCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connects
}

var _ transport.Transport = (*integStreamTransport)(nil)

// integProbeTransport answers the bootstrap stat-probe with the embedded agent's
// byte size so EnsureUploaded short-circuits (no upload) — mirroring the
// client_test.go probeOKTransport. It never streams; the real agent is wired by
// integStreamTransport.
type integProbeTransport struct{}

func (integProbeTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (integProbeTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}
func (integProbeTransport) Exec(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	if len(argv) > 0 && argv[0] == "uname" {
		return "Linux x86_64\n", "", nil
	}
	if len(stdin) > 0 {
		// The upload path (byte stdin) — never reached because the probe below
		// short-circuits, but honor the shape.
		return "", "", nil
	}
	// Answer the content-hash probe with the exact "<size> <sha256hex>" line
	// EnsureUploaded expects so it short-circuits (no upload, no reconnect delay).
	sum := sha256.Sum256(bootstrap.EmbeddedAgent())
	return fmt.Sprintf("%d %s", len(bootstrap.EmbeddedAgent()), hex.EncodeToString(sum[:])), "", nil
}
func (integProbeTransport) Close(context.Context) (bool, error) { return false, nil }
func (integProbeTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "probe", Endpoint: "/tmp/probe"}
}
func (integProbeTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

// integDaemon bundles the live pieces of a full in-process daemon: a real
// agentclient.Client (with its Hub teed) driving a real agent over pipes, plus a
// real localapi.Server on a real unix socket. Everything is torn down via
// t.Cleanup.
type integDaemon struct {
	tr     *integStreamTransport
	hub    *hub.Hub
	client *agentclient.Client
	cfg    *config.Store
	path   string
	sha    string
}

// integDeny is the deny set both the initial Subscribe and the PushAllow closure
// use, mirroring run.go (which passes app.DenyPorts). 22 stands in for it.
var integDeny = []uint16{22}

// startIntegDaemon builds the full stack: watcher.Fake seeded with snapshot, a
// real agent over pipes, a real agentclient.Client with the hub teed, a real
// config.Store, and a localapi.Server served on a real socket. It skips when the
// embedded SHA is empty (as client_test.go does) since the client's SHA-match
// check needs a real `make agent` SHA.
func startIntegDaemon(t *testing.T, snapshot []watcher.Listen, tick time.Duration) *integDaemon {
	t.Helper()
	sha := bootstrap.EmbeddedSHA()
	if sha == "" {
		t.Skip("embedded SHA empty; run `make agent` first")
	}

	tr := &integStreamTransport{snapshot: snapshot, sha: sha}
	h := hub.New()
	c := agentclient.New(agentclient.Config{
		Transport:        tr,
		Bootstrap:        bootstrap.New(integProbeTransport{}, nil),
		Hub:              h,
		HeartbeatTimeout: 30 * time.Second,
		ReconnectMin:     20 * time.Millisecond,
		ReconnectMax:     50 * time.Millisecond,
	})
	cfg := config.New(t.TempDir())

	// Push the initial filter before Run (replayed on connect), mirroring run.go.
	if err := c.Subscribe(integDeny, nil, true); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	clientDone := make(chan struct{})
	go func() { defer close(clientDone); _ = c.Run(ctx) }()

	deps := Deps{
		Version: VersionInfo{Version: "9.9", GitSHA: sha, ProtoVersion: protocol.ProtoVersion},
		Host:    func() (string, error) { return "testhost", nil },
		Agent:   c,
		Master:  tr,
		Config:  cfg,
		Hub:     h,
		PushAllow: func(allow []int) error {
			return c.Subscribe(integDeny, toU16Test(allow), true)
		},
		Kick:   func() {},
		Doctor: func(context.Context) *doctor.Report { return &doctor.Report{Host: "testhost"} },
	}
	srv := New(deps)
	if tick > 0 {
		srv.TickInterval = tick
	}

	path := filepath.Join(shortTempDir(t), "api.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()
		<-serveDone
		<-clientDone
	})
	waitVersion(t, path)
	// Wait for the first cached Snapshot before handing the daemon back. The
	// agent emits its Snapshot only AFTER watcher.Watcher.Start has returned, so
	// this guarantees a subsequent teardown (ctx cancel) can never race the
	// watcher.Fake's Start-return against its own ctx-cancel cleanup goroutine.
	waitSnapshot(t, c)
	return &integDaemon{tr: tr, hub: h, client: c, cfg: cfg, path: path, sha: sha}
}

// toU16Test narrows []int → []uint16 for the PushAllow closure (the run.go
// closure uses app.toU16; localapi tests can't import cmd/portal).
func toU16Test(in []int) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		if v > 0 && v <= 65535 {
			out = append(out, uint16(v))
		}
	}
	return out
}

// waitHandshake blocks until the agent's HelloAck lands or the deadline passes.
func waitHandshake(t *testing.T, c *agentclient.Client) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.HelloAck() != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent handshake never completed")
}

// waitSnapshot blocks until the client has a cached Snapshot (ok, seq>0), which
// implies the agent's watcher Start has returned.
func waitSnapshot(t *testing.T, c *agentclient.Client) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := c.Snapshot(); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent never produced a cached snapshot")
}

// getStatus fetches and decodes GET /v1/status over the socket.
func getStatus(t *testing.T, path string) Status {
	t.Helper()
	resp, err := unixClient(path).Get("http://unix/v1/status")
	if err != nil {
		t.Fatalf("GET /v1/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/status = %d, want 200", resp.StatusCode)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode Status: %v", err)
	}
	return st
}

// TestIntegration_StatusAgentIdentity (EC2): after the handshake completes,
// GET /v1/status over the real socket reports the fake agent's pid and SHA.
func TestIntegration_StatusAgentIdentity(t *testing.T) {
	d := startIntegDaemon(t, []watcher.Listen{{Port: 8081, Family: 4, Addr: "127.0.0.1"}}, 0)
	waitHandshake(t, d.client)

	st := getStatus(t, d.path)
	if st.Agent == nil {
		t.Fatal("status Agent is nil after handshake")
	}
	if st.Agent.Pid != os.Getpid() {
		t.Errorf("Agent.Pid = %d, want %d (agent runs in-process)", st.Agent.Pid, os.Getpid())
	}
	if st.Agent.SHA != d.sha {
		t.Errorf("Agent.SHA = %q, want %q", st.Agent.SHA, d.sha)
	}
	if !st.Master.Up {
		t.Error("Master.Up = false, want true (fake MasterPID is live)")
	}
	if !st.Features[config.FeatureExec] {
		t.Error("status Features[exec] = false/missing, want default enabled")
	}
}

// TestIntegration_EventsReconnectNotifyTick (EC3): the events stream is
// snapshot-first; a simulated agent reconnect yields a coalesced state line; a
// notification teed through the wired hub yields a notify line; a shortened tick
// interval yields a tick line.
func TestIntegration_EventsReconnectNotifyTick(t *testing.T) {
	d := startIntegDaemon(t, []watcher.Listen{{Port: 8081, Family: 4, Addr: "127.0.0.1"}}, 150*time.Millisecond)
	waitHandshake(t, d.client)

	resp, lr, cancel := openStream(t, d.path)
	defer cancel()
	defer resp.Body.Close()

	// Snapshot is always the first line.
	if first := lr.next(t, 2*time.Second); first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}

	// Force a session drop; the supervisor reconnects and re-handshakes, which
	// publishes a Coalesced signal → a "state" line arrives after the reconnect.
	before := d.tr.connectCount()
	d.tr.drop()
	recDeadline := time.Now().Add(3 * time.Second)
	for d.tr.connectCount() <= before && time.Now().Before(recDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := d.tr.connectCount(); got <= before {
		t.Fatalf("connectCount = %d, want > %d (agent must reconnect)", got, before)
	}
	_ = lr.waitType(t, "state", 3*time.Second)

	// A notification teed through the wired hub yields a "notify" line carrying
	// the exact fields.
	d.hub.Publish(hub.Event{Class: hub.Queued, Notify: &hub.Notify{Title: "hello", Verified: true, Source: "hook"}})
	n := lr.waitType(t, "notify", 3*time.Second)
	if n.Notify == nil || n.Notify.Title != "hello" || !n.Notify.Verified {
		t.Fatalf("notify line = %+v, want Title=hello Verified=true", n.Notify)
	}

	// The shortened TickInterval yields a tick line for hung-daemon detection.
	_ = lr.waitType(t, "tick", 3*time.Second)
}

// TestIntegration_AllowRoundTrip: PUT /v1/allow/{port} returns 200 with the new
// allowlist AND the agent observes the fresh Subscribe — the previously
// ephemeral-excluded port now appears in Status.Ports without waiting for a
// reconcile.
func TestIntegration_AllowRoundTrip(t *testing.T) {
	const ephemeral = 40085 // in the agent's [32768,60999] ephemeral range
	d := startIntegDaemon(t, []watcher.Listen{{Port: ephemeral, Family: 4, Addr: "127.0.0.1"}}, 0)
	waitHandshake(t, d.client)

	// Initially the ephemeral port is excluded (not in the allow set).
	if hasPort(getStatus(t, d.path).Ports, ephemeral) {
		t.Fatalf("port %d present before allow — expected ephemeral exclusion", ephemeral)
	}

	// PUT /v1/allow/40085 → 200 with the new allowlist.
	resp, err := unixClient(d.path).Do(mustReq(t, http.MethodPut, "http://unix/v1/allow/40085"))
	if err != nil {
		t.Fatalf("PUT /v1/allow/40085: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("PUT /v1/allow/40085 = %d, want 200", resp.StatusCode)
	}
	var al allowlistResponse
	if err := json.NewDecoder(resp.Body).Decode(&al); err != nil {
		resp.Body.Close()
		t.Fatalf("decode allowlist: %v", err)
	}
	resp.Body.Close()
	if !containsInt(al.Allowed, ephemeral) {
		t.Fatalf("allowlist %v does not contain %d", al.Allowed, ephemeral)
	}

	// The fresh Subscribe reaches the agent, which re-snapshots; the port now
	// shows in Status.Ports well within the reconcile safety net (no 60s wait).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hasPort(getStatus(t, d.path).Ports, ephemeral) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d never appeared in Status.Ports after allow push", ephemeral)
}

// TestIntegration_SingleInstanceLive (D7): a second Listen against the live
// socket fails — another daemon owns it.
func TestIntegration_SingleInstanceLive(t *testing.T) {
	d := startIntegDaemon(t, nil, 0)
	if _, err := Listen(d.path); err == nil {
		t.Fatal("second Listen against a live socket succeeded, want error")
	}
}

func hasPort(ports []PortStatus, port int) bool {
	for _, p := range ports {
		if p.Port == port {
			return true
		}
	}
	return false
}

func mustReq(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}
