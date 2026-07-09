package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/forward"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/internal/service"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/client"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

// --- fake daemon dependencies (localapi.Deps narrow interfaces) ---

// fakeAgentSource is a canned localapi.AgentSource. Subscribe advances rsid so
// the allow-push tests (EC3) can assert a Subscribe reached the agent without
// waiting for a reconcile. It is concurrency-safe: the localapi server reads
// HelloAck/Snapshot from handler goroutines while a test may read rsid.
type fakeAgentSource struct {
	mu        sync.Mutex
	ack       *protocol.HelloAck
	snapPorts []uint16
	snapOK    bool
	rsid      int
}

func (f *fakeAgentSource) HelloAck() *protocol.HelloAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ack
}

func (f *fakeAgentSource) Snapshot() (uint64, []uint16, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return 0, append([]uint16(nil), f.snapPorts...), f.snapOK
}

func (f *fakeAgentSource) LastDisconnectErr() string { return "" }

// Subscribe records that a new filter reached the agent by bumping rsid.
func (f *fakeAgentSource) Subscribe(_ []uint16, _ []uint16, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rsid++
	return nil
}

func (f *fakeAgentSource) RSID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rsid
}

var _ localapi.AgentSource = (*fakeAgentSource)(nil)

// fakeMasterProber is a canned localapi.MasterProber (Health + Describe).
type fakeMasterProber struct{ pid int }

func (f *fakeMasterProber) Health(context.Context) (transport.Health, error) {
	if f.pid <= 0 {
		return transport.Health{Up: false}, nil
	}
	return transport.Health{Up: true, Pid: f.pid, Detail: fmt.Sprintf("pid=%d", f.pid)}, nil
}
func (f *fakeMasterProber) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/cm-fake.sock"}
}

var _ localapi.MasterProber = (*fakeMasterProber)(nil)

// fakeForwardLister is a canned localapi.ForwardLister.
type fakeForwardLister struct{ lines []string }

func (f *fakeForwardLister) ForwardLines(context.Context) ([]string, error) {
	return append([]string(nil), f.lines...), nil
}

var _ localapi.ForwardLister = (*fakeForwardLister)(nil)

// fakeServiceStater is a canned localapi.ServiceStater.
type fakeServiceStater struct{ st service.Status }

func (f *fakeServiceStater) Status(context.Context) (service.Status, error) { return f.st, nil }

var _ localapi.ServiceStater = (*fakeServiceStater)(nil)

// countingConfig wraps the shared config.Store and counts feature reads/writes
// that go THROUGH the daemon. The CLI's daemon-down fallback touches a.Cfg (the
// unwrapped *config.Store) directly, so it never advances these counters. That
// gap is the only observable that distinguishes a real GET/PUT /v1/features
// round-trip from a silent fall-through: without it the two paths share the same
// backing files and produce byte-identical output. Concurrency-safe: the localapi
// server touches it from handler goroutines while a test reads the counters.
type countingConfig struct {
	localapi.ConfigStore
	reads  atomic.Int64
	writes atomic.Int64
}

func (c *countingConfig) FeatureEnabled(feature string) bool {
	c.reads.Add(1)
	return c.ConfigStore.FeatureEnabled(feature)
}

func (c *countingConfig) SetFeature(feature string, on bool) error {
	c.writes.Add(1)
	return c.ConfigStore.SetFeature(feature, on)
}

// --- fake daemon ---

// fakeDaemon is a real localapi.Server on a temp /tmp unix socket, backed by the
// canned deps above and sharing the test's config.Store (so allow/feature file
// mutations stay consistent between the CLI and the server). It exposes the
// socket path, the hub, the agent source, a Kick counter, and a Stop().
type fakeDaemon struct {
	path   string
	hub    *hub.Hub
	agent  *fakeAgentSource
	master *fakeMasterProber
	fwd    *fakeForwardLister
	svc    *fakeServiceStater
	cfg    *countingConfig
	kicks  atomic.Int64
	// reconciles stands in for the real engine's completed-pass counter that
	// ReconcileGen exposes over GET /v1/status. Kick advances it, modelling the
	// production engine running a pass in response to POST /v1/reconcile, so the
	// daemon-up `once` poll (pollOnceReconciled) can observe convergence. Tests
	// may also bump it directly to drive the poll.
	reconciles atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}
}

// fakeOpt customizes a fakeDaemon before its server starts serving.
type fakeOpt func(*fakeDaemon)

func withHelloAck(ack *protocol.HelloAck) fakeOpt {
	return func(d *fakeDaemon) { d.agent.ack = ack }
}

func withSnapshot(ports []uint16, ok bool) fakeOpt {
	return func(d *fakeDaemon) { d.agent.snapPorts = ports; d.agent.snapOK = ok }
}

func withMasterPID(pid int) fakeOpt {
	return func(d *fakeDaemon) { d.master.pid = pid }
}

func withForwardLines(lines []string) fakeOpt {
	return func(d *fakeDaemon) { d.fwd.lines = lines }
}

func withService(st service.Status) fakeOpt {
	return func(d *fakeDaemon) { d.svc.st = st }
}

// startFakeDaemon builds and serves a fakeDaemon, waits for it to answer, and
// registers teardown. cfg is shared so the server and the CLI-under-test read
// the same allow/feature/host files.
func startFakeDaemon(t *testing.T, cfg *config.Store, opts ...fakeOpt) *fakeDaemon {
	t.Helper()
	// A /tmp dir keeps the socket path well under the ~104-char sun_path limit
	// that t.TempDir() can blow past on macOS.
	dir, err := os.MkdirTemp("/tmp", "portal-api-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	d := &fakeDaemon{
		path:   filepath.Join(dir, "api.sock"),
		hub:    hub.New(),
		agent:  &fakeAgentSource{},
		master: &fakeMasterProber{pid: 4242},
		fwd:    &fakeForwardLister{},
		svc:    &fakeServiceStater{},
		cfg:    &countingConfig{ConfigStore: cfg},
		done:   make(chan struct{}),
	}
	for _, o := range opts {
		o(d)
	}

	deps := localapi.Deps{
		Version: api.VersionInfo{Version: "test", GitSHA: "deadbeef", ProtoVersion: protocol.ProtoVersion},
		Host:    cfg.ReadHost,
		Agent:   d.agent,
		Master:  d.master,
		Ports:   d.fwd,
		Service: d.svc,
		Config:  d.cfg,
		Hub:     d.hub,
		PushAllow: func(allow []int) error {
			return d.agent.Subscribe(toU16(app.DenyPorts), toU16(allow), true)
		},
		Kick:         func() { d.kicks.Add(1); d.reconciles.Add(1) },
		ReconcileGen: func() uint64 { return uint64(d.reconciles.Load()) },
		Doctor: func(context.Context) *doctor.Report {
			return &doctor.Report{Host: "fakehost"}
		},
	}

	ln, err := localapi.Listen(d.path)
	if err != nil {
		t.Fatalf("localapi.Listen: %v", err)
	}
	srv := localapi.New(deps)

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go func() {
		defer close(d.done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(d.Stop)

	// Wait until the socket answers so tests are deterministic.
	lc := client.New(d.path)
	deadline := time.Now().Add(3 * time.Second)
	for !lc.Available(context.Background()) {
		if time.Now().After(deadline) {
			t.Fatal("fake daemon did not come up")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return d
}

// Stop cancels the server and waits for Serve to return (which unlinks the
// socket). It is idempotent.
func (d *fakeDaemon) Stop() {
	d.cancel()
	<-d.done
}

func (d *fakeDaemon) kickCount() int { return int(d.kicks.Load()) }

// featureReads/featureWrites report how many times GET/PUT /v1/features touched
// the gates THROUGH the daemon. The daemon-down fallback bypasses this wrapper,
// so a non-advancing counter proves a request never reached the daemon.
func (d *fakeDaemon) featureReads() int  { return int(d.cfg.reads.Load()) }
func (d *fakeDaemon) featureWrites() int { return int(d.cfg.writes.Load()) }

// --- fake App adapters (the daemon-DOWN fallback path) ---

// appFakeTransport is a minimal transport.Transport for the fallback view: it
// only needs to report master liveness (pid) and identity.
type appFakeTransport struct{ pid int }

func (f *appFakeTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (f *appFakeTransport) Health(context.Context) (transport.Health, error) {
	if f.pid <= 0 {
		return transport.Health{Up: false}, nil
	}
	return transport.Health{Up: true, Pid: f.pid, Detail: fmt.Sprintf("pid=%d", f.pid)}, nil
}
func (f *appFakeTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (f *appFakeTransport) Close(context.Context) (bool, error) { return false, nil }
func (f *appFakeTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/cm-fake.sock"}
}
func (f *appFakeTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

var _ transport.Transport = (*appFakeTransport)(nil)

// appFakeService is a canned service.Manager (only Status is exercised).
type appFakeService struct{ st service.Status }

func (f *appFakeService) Status(context.Context) (service.Status, error) { return f.st, nil }
func (f *appFakeService) Install(context.Context) error                  { return nil }
func (f *appFakeService) Uninstall(context.Context) error                { return nil }
func (f *appFakeService) Reload(context.Context) error                   { return nil }
func (f *appFakeService) Start(context.Context) error                    { return nil }
func (f *appFakeService) Stop(context.Context) error                     { return nil }
func (f *appFakeService) Restart(context.Context) error                  { return nil }
func (f *appFakeService) IsLoaded(context.Context) (bool, error)         { return f.st.Loaded, nil }

var _ service.Manager = (*appFakeService)(nil)

// appFakePorts satisfies BOTH forward.LocalPorts (App.Ports) and
// transport.PortForwarder (App.PF): the fallback view reads forwards via
// App.PF.ForwardLines while the engine's conflict path reads local holders via
// App.Ports. One struct backs both so the fallback render sees a single set.
type appFakePorts struct {
	lines []string
	ports []int
}

func (f *appFakePorts) Forward(context.Context, int, int) error { return nil }
func (f *appFakePorts) Cancel(context.Context, int, int) error  { return nil }
func (f *appFakePorts) ListForwards(context.Context) ([]int, error) {
	return append([]int(nil), f.ports...), nil
}
func (f *appFakePorts) ForwardLines(context.Context) ([]string, error) {
	return append([]string(nil), f.lines...), nil
}
func (f *appFakePorts) LocalHolder(context.Context, int) (int, error) { return 0, nil }
func (f *appFakePorts) ProcessName(context.Context, int) string       { return "" }

var (
	_ forward.LocalPorts      = (*appFakePorts)(nil)
	_ transport.PortForwarder = (*appFakePorts)(nil)
)

// appFakeDiscover is a canned discover.RemoteDiscoverer.
type appFakeDiscover struct{ ports []int }

func (f *appFakeDiscover) DesiredPorts(context.Context, []int, []int) ([]int, error) {
	return append([]int(nil), f.ports...), nil
}

// newDaemonTestApp builds an App wired with the fallback fakes and pointed at
// sockPath. AgentClient is nil (a short-lived CLI has no agent handshake), so
// the fallback view never prints an agent line. The transport/service/ports/
// discover fakes carry representative values so the fallback render is
// observable.
func newDaemonTestApp(t *testing.T, sockPath string, cfg *config.Store) *app.App {
	t.Helper()
	ports := &appFakePorts{lines: []string{"127.0.0.1:5173"}, ports: []int{5173}}
	return &app.App{
		Paths: app.Paths{
			APISock: sockPath,
			Label:   "com.test.portal",
			Sock:    "/tmp/cm-fake.sock",
		},
		Cfg:         cfg,
		Transport:   &appFakeTransport{pid: 7777},
		PF:          ports,
		Service:     &appFakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
		Ports:       ports,
		Discover:    &appFakeDiscover{ports: []int{5173, 6006}},
		AgentClient: nil,
	}
}

// newTestConfig returns a config.Store over a fresh temp dir with host written.
func newTestConfig(t *testing.T, host string) *config.Store {
	t.Helper()
	cfg := config.New(t.TempDir())
	if host != "" {
		if err := cfg.WriteHost(host); err != nil {
			t.Fatalf("WriteHost: %v", err)
		}
	}
	return cfg
}
