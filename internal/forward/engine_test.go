package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/clock"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// fakeTransport implements BOTH transport.Transport and transport.PortForwarder
// for the engine tests: the engine takes the two capabilities as separate
// constructor args, but one struct satisfying both lets a test observe
// Forward/Cancel and the master pid on a single value. ListForwards is the
// engine's current-truth source and is keyed by the LIVE master pid so the
// stateless-rebuild test can flip pid and see the fresh master's empty set.
type fakeTransport struct {
	host        string
	pid         int
	rebuilt     bool           // returned by Ensure
	addCalls    [][2]int       // (local, remote) for Forward
	cancelCalls [][2]int       // for Cancel
	failOn      map[int]string // port → error substring; surfaces upward from Forward
	exitCalled  bool
	// forwardsByPID is the ground-truth forwarded set per master pid, returned
	// by ListForwards for the live pid.
	forwardsByPID map[int][]int
	// reconciled is a SYNCHRONIZED reconcile signal: ListForwards runs exactly
	// once per Reconcile pass, so a non-blocking notify here emits one value per
	// pass. Tests that want to observe reconciles set it to a buffered channel; a
	// nil channel is never ready, so tests that leave it nil see no behavior
	// change. Reading the plain slices/maps while Run is live would be a data
	// race — use this.
	reconciled chan struct{}
}

// --- transport.Transport ---

func (f *fakeTransport) Ensure(context.Context) (bool, error) { return f.rebuilt, nil }
func (f *fakeTransport) Health(context.Context) (transport.Health, error) {
	if f.pid <= 0 {
		return transport.Health{Up: false}, nil
	}
	return transport.Health{Up: true, Pid: f.pid, Detail: fmt.Sprintf("pid=%d", f.pid)}, nil
}
func (f *fakeTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return "", "", nil
}
func (f *fakeTransport) Stream(_ context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}
func (f *fakeTransport) Close(context.Context) (bool, error) {
	f.exitCalled = true
	return true, nil
}
func (f *fakeTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: f.host, Endpoint: "/tmp/sock-fake"}
}

// --- transport.PortForwarder ---

func (f *fakeTransport) Forward(ctx context.Context, l, r int) error {
	f.addCalls = append(f.addCalls, [2]int{l, r})
	if f.failOn != nil {
		if _, bad := f.failOn[l]; bad {
			return errors.New("ssh request failed")
		}
	}
	return nil
}
func (f *fakeTransport) Cancel(ctx context.Context, l, r int) error {
	f.cancelCalls = append(f.cancelCalls, [2]int{l, r})
	return nil
}
func (f *fakeTransport) ListForwards(context.Context) ([]int, error) {
	select {
	case f.reconciled <- struct{}{}:
	default:
	}
	return f.forwardsByPID[f.pid], nil
}
func (f *fakeTransport) ForwardLines(context.Context) ([]string, error) { return nil, nil }

var (
	_ transport.Transport     = (*fakeTransport)(nil)
	_ transport.PortForwarder = (*fakeTransport)(nil)
)

// fakePortLister implements the engine's narrow LocalPorts surface.
type fakePortLister struct {
	holders      map[int]int
	processNames map[int]string
}

func (f *fakePortLister) LocalHolder(_ context.Context, port int) (int, error) {
	return f.holders[port], nil
}
func (f *fakePortLister) ProcessName(_ context.Context, pid int) string {
	return f.processNames[pid]
}

// fakeDiscoverer implements discover.RemoteDiscoverer.
type fakeDiscoverer struct {
	desired []int
	err     error
}

func (f *fakeDiscoverer) DesiredPorts(_ context.Context, _, _ []int) ([]int, error) {
	return f.desired, f.err
}

func newTestEngine(t *fakeTransport, p *fakePortLister, d *fakeDiscoverer) (*Engine, *MemLogger) {
	log := &MemLogger{}
	cfg := config.New("/tmp/never-touched-by-test")
	clk := clock.Real{}
	// t satisfies both transport.Transport and transport.PortForwarder.
	return New(t, t, p, d, cfg, clk, log, time.Second, []int{22, 25}, []int{}), log
}

// TestReconcile_Diff is the keystone test: given desired = {8081, 8082, 8083, 8084},
// current (from lsof on master pid 111) = {8081, 9111}, holder of 8082 = pid 222,
// and forward(8083) fails — assert add/cancel/skip/log lines exactly match the bash
// behavior.
func TestReconcile_Diff(t *testing.T) {
	tr := &fakeTransport{
		host:          "clementine",
		pid:           111,
		failOn:        map[int]string{8083: "request failed"},
		forwardsByPID: map[int][]int{111: {8081, 9111}},
	}
	pl := &fakePortLister{
		holders:      map[int]int{8082: 222}, // foreign holder
		processNames: map[int]string{222: "node"},
	}
	d := &fakeDiscoverer{desired: []int{8081, 8082, 8083, 8084}}

	e, log := newTestEngine(tr, pl, d)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// 8082 is held by a foreign pid → engine MUST skip Forward.
	// 8083 is attempted but stderr says "request failed" → ERROR logged.
	// 8084 is free and missing → MUST be forwarded.
	wantAdds := [][2]int{{8083, 8083}, {8084, 8084}}
	for _, w := range wantAdds {
		found := false
		for _, c := range tr.addCalls {
			if c == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing Forward(%d,%d); calls=%v", w[0], w[1], tr.addCalls)
		}
	}
	// 8081 already forwarded — must NOT add.
	for _, c := range tr.addCalls {
		if c[0] == 8081 {
			t.Errorf("8081 should not be re-added; calls=%v", tr.addCalls)
		}
	}
	// 9111 forwarded but not desired — must Cancel.
	if len(tr.cancelCalls) != 1 || tr.cancelCalls[0] != [2]int{9111, 9111} {
		t.Errorf("cancel calls = %v, want [[9111 9111]]", tr.cancelCalls)
	}
	// Foreign holder logged as SKIP, but 8082 STILL got Forward called because
	// fake allows it; in practice the test asserts that engine consults
	// LocalHolder before Forward. Reconcile logic: holder != pid → log + skip.
	// Wait — we set 222 ≠ 111 so engine SKIPS 8082, no Forward.
	for _, c := range tr.addCalls {
		if c[0] == 8082 {
			t.Errorf("8082 should be SKIPPED (foreign holder); calls=%v", tr.addCalls)
		}
	}
	if !log.Has("SKIP port 8082") {
		t.Errorf("expected SKIP log for 8082; got:\n%v", log.Lines)
	}
	if !log.Has("ERROR adding forward 8083") {
		t.Errorf("expected ERROR log for 8083; got:\n%v", log.Lines)
	}
	if !log.Has("forwarded localhost:8084") {
		t.Errorf("expected forward log for 8084; got:\n%v", log.Lines)
	}
	if !log.Has("removed forward 9111") {
		t.Errorf("expected remove log for 9111; got:\n%v", log.Lines)
	}
}

// TestReconcile_StatelessAfterMasterRebuild — change the master pid mid-test.
// The engine must re-derive 'current' from the NEW pid via PortLister, not
// cache the old set. After the rebuild, all desired ports are missing on the
// fresh master and must be re-forwarded.
func TestReconcile_StatelessAfterMasterRebuild(t *testing.T) {
	tr := &fakeTransport{
		host: "clementine",
		pid:  111,
		forwardsByPID: map[int][]int{
			111: {8081}, // old master had 8081 forwarded
			222: {},     // fresh master has nothing
		},
	}
	pl := &fakePortLister{}
	d := &fakeDiscoverer{desired: []int{8081}}

	e, _ := newTestEngine(tr, pl, d)
	_ = e.Reconcile(context.Background())
	// First pass: 8081 already forwarded on pid 111 → no Forward call.
	if len(tr.addCalls) != 0 {
		t.Errorf("first pass should not add; calls=%v", tr.addCalls)
	}

	// Master rebuild: pid changes.
	tr.pid = 222
	_ = e.Reconcile(context.Background())
	// Second pass: pid 222 has no forwards → 8081 must be re-added.
	if len(tr.addCalls) != 1 || tr.addCalls[0] != [2]int{8081, 8081} {
		t.Errorf("second pass should re-add 8081 on new master; calls=%v", tr.addCalls)
	}
}

// TestReconcile_DiscoveryFailureKeepsForwards — if DesiredPorts errors, no
// Cancel calls are issued (transient blip must NOT drop everything).
func TestReconcile_DiscoveryFailureKeepsForwards(t *testing.T) {
	tr := &fakeTransport{host: "clementine", pid: 111, forwardsByPID: map[int][]int{111: {8081, 9111}}}
	pl := &fakePortLister{}
	d := &fakeDiscoverer{err: errors.New("transient")}

	e, log := newTestEngine(tr, pl, d)
	_ = e.Reconcile(context.Background())
	if len(tr.cancelCalls) != 0 {
		t.Errorf("discovery failure must not Cancel; got %v", tr.cancelCalls)
	}
	if len(tr.addCalls) != 0 {
		t.Errorf("discovery failure must not Forward; got %v", tr.addCalls)
	}
	if !log.Has("WARN: port discovery failed") {
		t.Errorf("expected WARN log; got %v", log.Lines)
	}
}

// TestReconcile_MasterDownReturnsError — if EnsureMaster returns pid=0, the
// engine must not blindly call Discover/PortLister; it logs a warning and
// returns (next tick will retry).
func TestReconcile_MasterDownReturnsError(t *testing.T) {
	tr := &fakeTransport{host: "clementine", pid: 0}
	pl := &fakePortLister{}
	d := &fakeDiscoverer{desired: []int{8081}}

	e, log := newTestEngine(tr, pl, d)
	err := e.Reconcile(context.Background())
	if err == nil {
		t.Errorf("expected error when master down")
	}
	if len(tr.addCalls) != 0 {
		t.Errorf("master down must not Forward; got %v", tr.addCalls)
	}
	if !log.Has("could not establish master") {
		t.Errorf("expected master-down log; got %v", log.Lines)
	}
}

// TestRun_ContextCancelLeavesMasterAlone — Run must return on ctx.Done()
// without calling Transport.Exit, matching the bash trap behavior.
func TestRun_ContextCancelLeavesMasterAlone(t *testing.T) {
	tr := &fakeTransport{host: "clementine", pid: 111, forwardsByPID: map[int][]int{111: {}}}
	pl := &fakePortLister{}
	d := &fakeDiscoverer{desired: []int{}}

	e, _ := newTestEngine(tr, pl, d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if err := e.Run(ctx); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if tr.exitCalled {
		t.Errorf("Run must not call Transport.Exit on signal")
	}
}

// TestRun_KickTriggersReconcile proves Engine.Kick() drives a reconcile pass
// (EC7). It observes reconciles via the fake's SYNCHRONIZED reconciled channel
// — never by polling the fakes' unsynchronized slices/maps while Run is live
// (that would be a data race). A non-nil, never-fed AgentEvents channel forces
// the event-driven loop; the default 60s SafetyInterval is far past the 1s
// window, so the second signal is unambiguously the Kick's, not the backstop.
func TestRun_KickTriggersReconcile(t *testing.T) {
	tr := &fakeTransport{
		host:          "clementine",
		pid:           111,
		forwardsByPID: map[int][]int{111: {}},
		reconciled:    make(chan struct{}, 8),
	}
	pl := &fakePortLister{}
	d := &fakeDiscoverer{desired: []int{8081}}

	e, _ := newTestEngine(tr, pl, d)
	// Non-nil never-fed channel forces runEventDriven (event-driven select).
	e.AgentEvents = make(chan EngineEvent)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = e.Run(ctx)
	}()

	// The immediate startup reconcile.
	select {
	case <-tr.reconciled:
	case <-time.After(1 * time.Second):
		cancel()
		wg.Wait()
		t.Fatal("no startup reconcile observed")
	}

	e.Kick()

	// The kicked reconcile — comfortably past the 50ms debounce and far under
	// the 60s safety ticker, so this signal is the Kick's.
	select {
	case <-tr.reconciled:
	case <-time.After(1 * time.Second):
		cancel()
		wg.Wait()
		t.Fatal("Kick did not trigger a reconcile within 1s")
	}

	cancel()
	wg.Wait()

	// Only now, with no concurrent writer left, may we read the plain slices.
	// The kicked pass re-issued Forward(8081) since the fake master had none.
	found := false
	for _, c := range tr.addCalls {
		if c == [2]int{8081, 8081} {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Forward(8081,8081) across reconcile passes; addCalls=%v", tr.addCalls)
	}
}

// TestReconcile_MasterEstablishedLog pins the byte-exact "master established
// (pid=N)" log line (T9): when Ensure reports a fresh (re)build and Health
// reports the master up, the engine renders the line from Health.Pid. No prior
// test asserted this line — it is the machine-verifiable guard that the system
// transport keeps carrying the pid into the daemon log after the migration.
func TestReconcile_MasterEstablishedLog(t *testing.T) {
	tr := &fakeTransport{
		host:          "clementine",
		pid:           4242,
		rebuilt:       true, // Ensure reports THIS call performed the rebuild
		forwardsByPID: map[int][]int{4242: {}},
	}
	pl := &fakePortLister{}
	d := &fakeDiscoverer{desired: []int{}}

	e, log := newTestEngine(tr, pl, d)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !log.Has("master established (pid=4242)") {
		t.Errorf("expected 'master established (pid=4242)' log; got:\n%v", log.Lines)
	}
}
