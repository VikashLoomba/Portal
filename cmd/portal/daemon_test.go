package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/doctor"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localapi"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/proc"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/service"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
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

// fakeMasterProber is a canned localapi.MasterProber.
type fakeMasterProber struct{ pid int }

func (f *fakeMasterProber) MasterPID(context.Context) (int, error) { return f.pid, nil }

var _ localapi.MasterProber = (*fakeMasterProber)(nil)

// fakeForwardLister is a canned localapi.ForwardLister.
type fakeForwardLister struct{ lines []string }

func (f *fakeForwardLister) MasterForwardLines(context.Context, int) ([]string, error) {
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
		Version: localapi.VersionInfo{Version: "test", GitSHA: "deadbeef", ProtoVersion: protocol.ProtoVersion},
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
		Kick: func() { d.kicks.Add(1) },
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
	lc := localclient.New(d.path)
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

// appFakeTransport is a minimal sshctl.Transport for the fallback view: it only
// needs to report a master pid.
type appFakeTransport struct{ pid int }

func (f *appFakeTransport) MasterPID(context.Context) (int, error) { return f.pid, nil }
func (f *appFakeTransport) EnsureMaster(context.Context) (int, bool, error) {
	return f.pid, false, nil
}
func (f *appFakeTransport) Forward(context.Context, int, int) error { return nil }
func (f *appFakeTransport) Cancel(context.Context, int, int) error  { return nil }
func (f *appFakeTransport) Exit(context.Context) (bool, error)      { return false, nil }
func (f *appFakeTransport) Host() string                            { return "fakehost" }
func (f *appFakeTransport) Sock() string                            { return "/tmp/cm-fake.sock" }
func (f *appFakeTransport) Exec(context.Context, string, ...string) (string, error) {
	return "", nil
}
func (f *appFakeTransport) ExecBytes(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (f *appFakeTransport) ExecStream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

var _ sshctl.Transport = (*appFakeTransport)(nil)

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

// appFakePorts is a canned proc.PortLister.
type appFakePorts struct {
	lines []string
	ports []int
}

func (f *appFakePorts) MasterForwards(context.Context, int) ([]int, error) {
	return append([]int(nil), f.ports...), nil
}
func (f *appFakePorts) MasterForwardLines(context.Context, int) ([]string, error) {
	return append([]string(nil), f.lines...), nil
}
func (f *appFakePorts) LocalHolder(context.Context, int) (int, error) { return 0, nil }
func (f *appFakePorts) ProcessName(context.Context, int) string       { return "" }

var _ proc.PortLister = (*appFakePorts)(nil)

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
	return &app.App{
		Paths: app.Paths{
			APISock: sockPath,
			Label:   "com.test.portal",
			Sock:    "/tmp/cm-fake.sock",
		},
		Cfg:         cfg,
		Transport:   &appFakeTransport{pid: 7777},
		Service:     &appFakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
		Ports:       &appFakePorts{lines: []string{"127.0.0.1:5173"}, ports: []int{5173}},
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
