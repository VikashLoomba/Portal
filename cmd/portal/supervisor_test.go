package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/clock"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/discover"
	"github.com/VikashLoomba/Portal/internal/forward"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/client"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/run"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/sshnative"
)

type stackTestBootstrap struct{}

func (stackTestBootstrap) EnsureUploaded(context.Context) (string, error) { return "/agent", nil }
func (stackTestBootstrap) SetBootID(string)                               {}
func (stackTestBootstrap) EmbeddedSHA() string                            { return "test" }

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
}

func (l *callLog) reset() {
	l.mu.Lock()
	l.calls = nil
	l.mu.Unlock()
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type recordingStackTransport struct {
	host            string
	impl            transport.Impl
	log             *callLog
	closeStarted    chan struct{}
	closeGate       <-chan struct{}
	closeIgnoresCtx bool
	closeFirstOnly  bool
	closeNotStopped bool
	closeErr        error
	closeCalls      atomic.Int32
	workCheck       func()
	closeOnce       sync.Once
	healthOnce      sync.Once
	healthDown      atomic.Bool
	healthStarted   chan struct{}
	healthCanceled  chan struct{}
	healthRelease   <-chan struct{}
	forwardStarted  chan struct{}
	forwardCanceled chan struct{}
	forwardRelease  <-chan struct{}
}

func (t *recordingStackTransport) record(name string) {
	if t.log != nil {
		t.log.add(t.host + "." + name)
	}
	if name != "close" && t.workCheck != nil {
		t.workCheck()
	}
}

func (t *recordingStackTransport) Ensure(context.Context) (bool, error) {
	t.record("ensure")
	return false, nil
}

func (t *recordingStackTransport) Health(ctx context.Context) (transport.Health, error) {
	if t.healthStarted != nil {
		first := false
		t.healthOnce.Do(func() { first = true })
		if first {
			close(t.healthStarted)
			<-ctx.Done()
			close(t.healthCanceled)
			if t.healthRelease != nil {
				<-t.healthRelease
			}
			return transport.Health{}, ctx.Err()
		}
	}
	if t.healthDown.Load() {
		return transport.Health{Up: false}, nil
	}
	return transport.Health{Up: true, Detail: "test"}, nil
}

func (t *recordingStackTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	t.record("exec")
	return "", "", nil
}

func (t *recordingStackTransport) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	t.record("stream")
	return discardWriteCloser{}, io.NopCloser(strings.NewReader("")), io.NopCloser(strings.NewReader("")), func() error {
		<-ctx.Done()
		return ctx.Err()
	}, nil
}

func (t *recordingStackTransport) Close(ctx context.Context) (bool, error) {
	t.record("close")
	closeCall := t.closeCalls.Add(1)
	t.closeOnce.Do(func() {
		if t.closeStarted != nil {
			close(t.closeStarted)
		}
	})
	if t.closeGate != nil && (!t.closeFirstOnly || closeCall == 1) {
		if t.closeIgnoresCtx {
			<-t.closeGate
		} else {
			select {
			case <-t.closeGate:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
	}
	return t.closeErr == nil && !t.closeNotStopped, t.closeErr
}

func (t *recordingStackTransport) Describe() transport.Desc {
	impl := t.impl
	if impl == "" {
		impl = transport.ImplSystemSSH
	}
	return transport.Desc{Impl: impl, Host: t.host, Endpoint: t.host}
}

func (t *recordingStackTransport) Forward(context.Context, int, int) error { return nil }
func (t *recordingStackTransport) Cancel(context.Context, int, int) error  { return nil }
func (t *recordingStackTransport) ListForwards(context.Context) ([]int, error) {
	return []int{}, nil
}

func (t *recordingStackTransport) ForwardLines(ctx context.Context) ([]string, error) {
	if t.forwardStarted != nil {
		close(t.forwardStarted)
		<-ctx.Done()
		close(t.forwardCanceled)
		if t.forwardRelease != nil {
			<-t.forwardRelease
		}
		return nil, ctx.Err()
	}
	return []string{}, nil
}

func (t *recordingStackTransport) StreamPty(ctx context.Context, _ transport.PtyRequest, _ ...string) (transport.PtySession, error) {
	t.record("pty")
	return &recordingPtySession{ctx: ctx, done: make(chan struct{})}, nil
}

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

type recordingPtySession struct {
	ctx  context.Context
	done chan struct{}
	once sync.Once
}

func (*recordingPtySession) Read([]byte) (int, error)    { return 0, io.EOF }
func (*recordingPtySession) Write(p []byte) (int, error) { return len(p), nil }
func (*recordingPtySession) Resize(uint16, uint16) error { return nil }
func (s *recordingPtySession) Wait() error               { <-s.ctx.Done(); return s.ctx.Err() }
func (s *recordingPtySession) Close() error              { s.once.Do(func() { close(s.done) }); return nil }

var (
	_ transport.Transport     = (*recordingStackTransport)(nil)
	_ transport.PortForwarder = (*recordingStackTransport)(nil)
	_ transport.PtyStreamer   = (*recordingStackTransport)(nil)
)

func newSupervisorBase(t *testing.T, host string) *app.App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.New(dir)
	if host != "" {
		if err := cfg.WriteHost(host); err != nil {
			t.Fatal(err)
		}
	}
	return &app.App{
		Paths:  app.Paths{ConfigDir: dir, Sock: filepath.Join(dir, "cm.sock"), APISock: filepath.Join(dir, "api.sock")},
		Cfg:    cfg,
		Runner: &run.Fake{},
		Clk:    clock.Real{},
		Log:    &forward.MemLogger{},
		Audit:  audit.New(dir),
		Hub:    hub.New(),
	}
}

func newSupervisorStack(base *app.App, host string, tr *recordingStackTransport) *app.Stack {
	ac := agentclient.New(agentclient.Config{
		Transport:    tr,
		Bootstrap:    stackTestBootstrap{},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		StderrSink:   io.Discard,
		Hub:          base.Hub,
		ReconnectMin: time.Hour,
		ReconnectMax: time.Hour,
	})
	rd := discover.NewAgent(ac)
	engine := forward.New(tr, tr, &appFakePorts{}, rd, base.Cfg, base.Clk, base.Log,
		app.Interval, app.DenyPorts, app.SkipLocal)
	engine.AgentEvents = make(chan forward.EngineEvent)
	return &app.Stack{
		Host: host, Paths: base.Paths, Transport: tr, PF: tr, AgentClient: ac,
		Discover: rd, Engine: engine, OpenURLCh: make(chan string, 1),
	}
}

func waitForCall(t *testing.T, log *callLog, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, call := range log.snapshot() {
			if call == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("calls = %v, missing %q", log.snapshot(), want)
}

func TestSupervisorActivateOrderingAndHubSignal(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	log := &callLog{}
	oldTr := &recordingStackTransport{host: "A", log: log}
	newTr := &recordingStackTransport{host: "B", log: log}
	oldStack := newSupervisorStack(base, "A", oldTr)
	newStack := newSupervisorStack(base, "B", newTr)
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return oldStack, nil
		}
		return newStack, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	waitForCall(t, log, "A.ensure")
	log.reset()
	if err := os.WriteFile(base.Paths.Sock, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	newTr.workCheck = func() {
		if _, err := os.Stat(base.Paths.Sock); !os.IsNotExist(err) {
			t.Errorf("new-stack work began before shared socket removal: %v", err)
		}
	}
	events, unsubscribe := base.Hub.Subscribe(hub.Coalesced)
	defer unsubscribe()

	if err := s.Activate(context.Background(), "B"); err != nil {
		t.Fatal(err)
	}
	waitForCall(t, log, "B.ensure")
	calls := log.snapshot()
	closeAt, newAt := -1, -1
	for i, call := range calls {
		if call == "A.close" {
			closeAt = i
		}
		if strings.HasPrefix(call, "B.") && newAt < 0 {
			newAt = i
		}
	}
	if closeAt < 0 || newAt < 0 || closeAt >= newAt {
		t.Fatalf("call order = %v, want A.close before all B work", calls)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("existing hub subscriber did not receive activation signal")
	}
	if host, _ := s.host(); host != "B" {
		t.Fatalf("active host = %q, want B", host)
	}
}

func TestSupervisorSameHostActivationIsNoOp(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	log := &callLog{}
	tr := &recordingStackTransport{host: "A", log: log}
	stack := newSupervisorStack(base, "A", tr)
	constructions := 0
	s.newStack = func(context.Context, string) (*app.Stack, error) {
		constructions++
		return stack, nil
	}
	shutdowns := 0
	s.shutdownStack = func(context.Context, *app.Stack) error {
		shutdowns++
		return nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	waitForCall(t, log, "A.ensure")
	log.reset()
	old := s.current()

	if err := s.Activate(context.Background(), "A"); err != nil {
		t.Fatalf("same-host Activate: %v", err)
	}
	if constructions != 1 {
		t.Fatalf("stack constructions = %d, want initial construction only", constructions)
	}
	if shutdowns != 0 || tr.closeCalls.Load() != 0 {
		t.Fatalf("same-host activation drained stack: shutdowns=%d closes=%d", shutdowns, tr.closeCalls.Load())
	}
	if s.current() != old {
		t.Fatal("same-host activation swapped the live stack")
	}
	if calls := log.snapshot(); len(calls) != 0 {
		t.Fatalf("same-host activation started stack work: %v", calls)
	}
}

func TestSupervisorConstructFailureKeepsOldStack(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	log := &callLog{}
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A", log: log})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return nil, errors.New("construct failed")
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	log.reset()
	if err := s.Activate(context.Background(), "B"); err == nil {
		t.Fatal("Activate succeeded, want construction error")
	}
	if s.current().stack != old {
		t.Fatal("construction failure replaced the live stack")
	}
	if host, _ := s.host(); host != "A" {
		t.Fatalf("active host = %q, want A", host)
	}
	for _, call := range log.snapshot() {
		if call == "A.close" {
			t.Fatal("construction failure drained the old stack")
		}
	}
}

func TestSupervisorDrainRejectsWorkAndPreservesPtyCapability(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	gate := make(chan struct{})
	closeStarted := make(chan struct{})
	oldTr := &recordingStackTransport{host: "A", closeStarted: closeStarted, closeGate: gate}
	newTr := &recordingStackTransport{host: "B"}
	old := newSupervisorStack(base, "A", oldTr)
	next := newSupervisorStack(base, "B", newTr)
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	captured, ok := s.serving()
	if !ok {
		t.Fatal("old stack was not serving before activation")
	}

	done := make(chan error, 1)
	go func() { done <- s.Activate(context.Background(), "B") }()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("activation did not reach old transport Close")
	}
	if host, _ := s.host(); host != "" {
		t.Fatalf("host during drain = %q, want empty", host)
	}
	execAdapter := supervisorExec{s}
	if _, ok := any(execAdapter).(transport.PtyStreamer); !ok {
		t.Fatal("exec adapter lost the optional PTY capability")
	}
	if _, _, _, _, err := execAdapter.Stream(context.Background(), "true"); !errors.Is(err, errNotConfigured) {
		t.Fatalf("Stream during drain error = %v, want not-configured sentinel", err)
	}
	if _, _, err := captured.bindContext(context.Background()); !errors.Is(err, errNotConfigured) {
		t.Fatalf("captured old stack bind error = %v, want not-configured sentinel", err)
	}
	if rep := s.doctor(context.Background()); rep.OK() {
		t.Fatalf("doctor during drain = %+v, want not-configured failure", rep)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sess, err := execAdapter.StreamPty(context.Background(), transport.PtyRequest{}, "true")
	if err != nil {
		t.Fatalf("StreamPty through active adapter: %v", err)
	}
	if sess == nil {
		t.Fatal("StreamPty through active adapter returned a nil session")
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close StreamPty session: %v", err)
	}
}

func TestSupervisorCancelKillsInflightExec(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A"})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	_, _, _, wait, err := (supervisorExec{s}).Stream(context.Background(), "sleep")
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- wait() }()
	if err := s.Activate(context.Background(), "B"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old exec wait error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old exec was not killed by stack cancellation")
	}
}

func TestSupervisorDrainJoinsControlSocketOperations(t *testing.T) {
	for _, operation := range []string{"health", "forward lines"} {
		t.Run(operation, func(t *testing.T) {
			base := newSupervisorBase(t, "A")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := newSupervisor(ctx, base, io.Discard)
			started := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			oldTr := &recordingStackTransport{host: "A"}
			if operation == "health" {
				oldTr.healthStarted = started
				oldTr.healthCanceled = canceled
				oldTr.healthRelease = release
			} else {
				oldTr.forwardStarted = started
				oldTr.forwardCanceled = canceled
				oldTr.forwardRelease = release
			}
			old := newSupervisorStack(base, "A", oldTr)
			next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
			s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
				if host == "A" {
					return old, nil
				}
				return next, nil
			}
			s.ref.Store(s.newLiveStack(old))
			defer func() { cancel(); s.waitCurrent() }()

			operationDone := make(chan error, 1)
			if operation == "health" {
				go func() {
					_, err := (supervisorMaster{s}).Health(context.Background())
					operationDone <- err
				}()
			} else {
				go func() {
					_, err := (supervisorPorts{s}).ForwardLines(context.Background())
					operationDone <- err
				}()
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatalf("%s did not start", operation)
			}

			activated := make(chan error, 1)
			go func() { activated <- s.Activate(context.Background(), "B") }()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatalf("%s did not inherit stack cancellation", operation)
			}
			select {
			case err := <-activated:
				t.Fatalf("activation returned before %s exited: %v", operation, err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-operationDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("%s error = %v, want context canceled", operation, err)
			}
			if err := <-activated; err != nil {
				t.Fatalf("Activate after %s exit: %v", operation, err)
			}
		})
	}
}

func TestBindStackContextStartsCanceledWithDeadStack(t *testing.T) {
	stackCtx, stopStack := context.WithCancel(context.Background())
	stopStack()
	ctx, cancel := bindStackContext(context.Background(), stackCtx)
	defer cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("bound context error = %v, want synchronous cancellation", ctx.Err())
	}
}

func TestSupervisorAllowlistPushWaitsForActivationStartup(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	gate := make(chan struct{})
	closeStarted := make(chan struct{})
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A", closeStarted: closeStarted, closeGate: gate})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	activated := make(chan error, 1)
	go func() { activated <- s.Activate(context.Background(), "B") }()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("activation did not enter drain")
	}
	pushed := make(chan error, 1)
	go func() { pushed <- s.pushAllow([]int{8080}) }()
	select {
	case err := <-pushed:
		t.Fatalf("allowlist push returned during activation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
	if err := <-pushed; err != nil {
		t.Fatalf("allowlist push after activation: %v", err)
	}
}

func TestSupervisorBlockedAllowlistPushDoesNotBlockActivationMutex(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	s.drainTimeout = 40 * time.Millisecond
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A"})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	pushStarted := make(chan struct{})
	releasePush := make(chan struct{})
	s.pushAllowStack = func(*app.Stack, []int) error {
		close(pushStarted)
		<-releasePush
		return nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	pushed := make(chan error, 1)
	go func() { pushed <- s.pushAllow([]int{8080}) }()
	select {
	case <-pushStarted:
	case <-time.After(time.Second):
		t.Fatal("allowlist push did not start")
	}
	started := time.Now()
	err := s.Activate(context.Background(), "B")
	if !errors.Is(err, context.DeadlineExceeded) {
		close(releasePush)
		t.Fatalf("Activate error = %v, want bounded drain deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 120*time.Millisecond {
		close(releasePush)
		t.Fatalf("blocked allowlist push held activation mutex for %s", elapsed)
	}
	close(releasePush)
	if err := <-pushed; err != nil {
		t.Fatalf("pushAllow: %v", err)
	}
}

func TestSupervisorTeardownFailureDoesNotStartReplacement(t *testing.T) {
	for _, tt := range []struct {
		name            string
		closeErr        error
		closeNotStopped bool
		socketDir       bool
	}{
		{name: "transport close", closeErr: errors.New("close failed")},
		{name: "transport stop unconfirmed", closeNotStopped: true},
		{name: "socket removal", socketDir: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := newSupervisorBase(t, "A")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := newSupervisor(ctx, base, io.Discard)
			log := &callLog{}
			old := newSupervisorStack(base, "A", &recordingStackTransport{
				host: "A", log: log, closeErr: tt.closeErr, closeNotStopped: tt.closeNotStopped,
			})
			next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B", log: log})
			s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
				if host == "A" {
					return old, nil
				}
				return next, nil
			}
			if err := s.startInitial(ctx, "A"); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); s.waitCurrent() }()
			waitForCall(t, log, "A.ensure")
			log.reset()
			if tt.socketDir {
				if err := os.MkdirAll(filepath.Join(base.Paths.Sock, "child"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			if err := s.Activate(context.Background(), "B"); err == nil {
				t.Fatal("Activate succeeded despite teardown failure")
			}
			if s.current().stack != old {
				t.Fatal("teardown failure published the replacement stack")
			}
			for _, call := range log.snapshot() {
				if strings.HasPrefix(call, "B.") {
					t.Fatalf("replacement work started after teardown failure: %v", log.snapshot())
				}
			}
		})
	}
}

func TestSupervisorUnconfirmedMasterStopBlocksActivationRetry(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	oldTr := &recordingStackTransport{host: "A", closeNotStopped: true}
	old := newSupervisorStack(base, "A", oldTr)
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	if err := s.Activate(context.Background(), "B"); err == nil {
		t.Fatal("first Activate succeeded without a confirmed master stop")
	}
	oldTr.healthDown.Store(true)
	if err := s.Activate(context.Background(), "B"); err == nil {
		t.Fatal("retry Activate succeeded after the control socket lost the master")
	}
	if s.current().stack != old {
		t.Fatal("activation retry published the replacement stack")
	}
}

func TestSupervisorUnconfirmedMasterStopBlocksActivationWhenHealthIsDown(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	oldTr := &recordingStackTransport{host: "A", closeNotStopped: true}
	oldTr.healthDown.Store(true)
	old := newSupervisorStack(base, "A", oldTr)
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	if err := s.Activate(context.Background(), "B"); err == nil {
		t.Fatal("Activate succeeded after a down probe without a confirmed master stop")
	}
	if s.current().stack != old {
		t.Fatal("unconfirmed master stop published the replacement stack")
	}
}

func TestSupervisorAllowsFalseCloseForNativeTransport(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	oldTr := &recordingStackTransport{host: "A", impl: transport.ImplNativeSSH, closeNotStopped: true}
	old := newSupervisorStack(base, "A", oldTr)
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	if err := s.Activate(context.Background(), "B"); err != nil {
		t.Fatalf("Activate with no native client: %v", err)
	}
	if s.current().stack != next {
		t.Fatal("activation did not publish the replacement stack")
	}
}

func TestSupervisorDrainJoinIsBounded(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	s.drainTimeout = 40 * time.Millisecond
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A"})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	stuck := make(chan struct{})
	ls := s.current()
	ls.wg.Add(1)
	go func() {
		defer ls.wg.Done()
		<-stuck
	}()
	started := time.Now()
	err := s.Activate(context.Background(), "B")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Activate error = %v, want drain deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 120*time.Millisecond {
		t.Fatalf("activation used more than one 40ms drain budget: %s", elapsed)
	}
	if s.current().stack != old {
		t.Fatal("timed-out drain published the replacement stack")
	}
	close(stuck)
}

func TestSupervisorShutdownIsBounded(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	s.drainTimeout = 40 * time.Millisecond
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A"})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	shutdownStarted := make(chan struct{})
	releaseShutdown := make(chan struct{})
	s.shutdownStack = func(context.Context, *app.Stack) error {
		close(shutdownStarted)
		<-releaseShutdown
		return nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	started := time.Now()
	err := s.Activate(context.Background(), "B")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Activate error = %v, want drain deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 120*time.Millisecond {
		t.Fatalf("blocked shutdown exceeded one 40ms drain budget: %s", elapsed)
	}
	select {
	case <-shutdownStarted:
	default:
		t.Fatal("old-stack shutdown was not attempted")
	}
	if s.current().stack != old {
		t.Fatal("timed-out shutdown published the replacement stack")
	}
	close(releaseShutdown)
}

func TestSupervisorQuiescesOldStackBeforeTransportClose(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	closeStarted := make(chan struct{})
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A", closeStarted: closeStarted})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	workerCanceled := make(chan struct{})
	releaseWorker := make(chan struct{})
	ls := s.current()
	ls.wg.Add(1)
	go func() {
		defer ls.wg.Done()
		<-ls.ctx.Done()
		close(workerCanceled)
		<-releaseWorker
	}()

	activated := make(chan error, 1)
	go func() { activated <- s.Activate(context.Background(), "B") }()
	select {
	case <-workerCanceled:
	case <-time.After(time.Second):
		t.Fatal("old-stack worker was not canceled")
	}
	select {
	case <-closeStarted:
		t.Fatal("transport closed before old-stack work quiesced")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseWorker)
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorTransportCloseIsBoundedWhenContextIgnored(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	s.drainTimeout = 40 * time.Millisecond
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	old := newSupervisorStack(base, "A", &recordingStackTransport{
		host: "A", closeStarted: closeStarted, closeGate: releaseClose, closeIgnoresCtx: true,
	})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	started := time.Now()
	err := s.Activate(context.Background(), "B")
	if !errors.Is(err, context.DeadlineExceeded) {
		close(releaseClose)
		t.Fatalf("Activate error = %v, want drain deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 120*time.Millisecond {
		close(releaseClose)
		t.Fatalf("context-ignoring transport close blocked activation for %s", elapsed)
	}
	select {
	case <-closeStarted:
	default:
		close(releaseClose)
		t.Fatal("transport close was not attempted")
	}
	close(releaseClose)
}

func TestSupervisorTimedOutCloseBlocksActivationRetry(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	s.drainTimeout = 40 * time.Millisecond
	releaseFirstClose := make(chan struct{})
	oldTr := &recordingStackTransport{
		host: "A", closeGate: releaseFirstClose, closeIgnoresCtx: true, closeFirstOnly: true,
	}
	old := newSupervisorStack(base, "A", oldTr)
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()

	if err := s.Activate(context.Background(), "B"); !errors.Is(err, context.DeadlineExceeded) {
		close(releaseFirstClose)
		t.Fatalf("first Activate error = %v, want drain deadline", err)
	}
	s.drainTimeout = time.Second
	retried := make(chan error, 1)
	go func() { retried <- s.Activate(context.Background(), "B") }()
	select {
	case err := <-retried:
		close(releaseFirstClose)
		t.Fatalf("activation retry returned before timed-out close exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := oldTr.closeCalls.Load(); got != 1 {
		close(releaseFirstClose)
		t.Fatalf("transport close calls = %d before first close exited, want 1", got)
	}
	if s.current().stack != old {
		close(releaseFirstClose)
		t.Fatal("activation retry published replacement while timed-out close was running")
	}
	close(releaseFirstClose)
	if err := <-retried; err != nil {
		t.Fatalf("activation retry after close exit: %v", err)
	}
	if got := oldTr.closeCalls.Load(); got != 2 {
		t.Fatalf("transport close calls = %d after retry, want 2", got)
	}
	if s.current().stack != next {
		t.Fatal("activation retry did not publish replacement after close exit")
	}
}

func TestSupervisorCloseAndJoinShareDrainDeadline(t *testing.T) {
	base := newSupervisorBase(t, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newSupervisor(ctx, base, io.Discard)
	s.drainTimeout = 80 * time.Millisecond
	closeGate := make(chan struct{})
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A", closeGate: closeGate})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); s.waitCurrent() }()
	stuck := make(chan struct{})
	ls := s.current()
	ls.wg.Add(1)
	go func() {
		defer ls.wg.Done()
		<-stuck
	}()

	started := time.Now()
	err := s.Activate(context.Background(), "B")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Activate error = %v, want drain deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 140*time.Millisecond {
		t.Fatalf("close plus join exceeded one 80ms budget: %s", elapsed)
	}
	close(closeGate)
	close(stuck)
}

func TestSupervisorNewStackOutlivesActivationRequest(t *testing.T) {
	base := newSupervisorBase(t, "A")
	daemonCtx, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	s := newSupervisor(daemonCtx, base, io.Discard)
	old := newSupervisorStack(base, "A", &recordingStackTransport{host: "A"})
	next := newSupervisorStack(base, "B", &recordingStackTransport{host: "B"})
	s.newStack = func(_ context.Context, host string) (*app.Stack, error) {
		if host == "A" {
			return old, nil
		}
		return next, nil
	}
	if err := s.startInitial(daemonCtx, "A"); err != nil {
		t.Fatal(err)
	}
	defer func() { stopDaemon(); s.waitCurrent() }()
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	if err := s.Activate(reqCtx, "B"); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	select {
	case <-s.current().ctx.Done():
		t.Fatal("activation request cancellation stopped the new live stack")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSupervisorActivationConstructionContext(t *testing.T) {
	base := newSupervisorBase(t, "A")
	if err := base.Cfg.SetTransport("native"); err != nil {
		t.Fatal(err)
	}
	ctx, cancelDaemon := context.WithCancel(context.Background())
	defer cancelDaemon()
	s := newSupervisor(ctx, base, io.Discard)
	old := &liveStack{stack: newSupervisorStack(base, "A", &recordingStackTransport{host: "A"})}
	old.ctx, old.cancel = context.WithCancel(ctx)
	s.ref.Store(old)
	s.newStack = func(ctx context.Context, host string) (*app.Stack, error) {
		resolver := sshnative.WithConfigResolver(func(ctx context.Context, _ string) (sshnative.ResolvedHost, error) {
			<-ctx.Done()
			return sshnative.ResolvedHost{}, ctx.Err()
		})
		return app.NewStack(ctx, base.Paths, base.Cfg, base.Hub, host, base.Runner,
			base.Clk, base.Log, io.Discard, resolver)
	}
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Activate(reqCtx, "B"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Activate error = %v, want context canceled", err)
	}
	if s.current() != old {
		t.Fatal("canceled construction changed the current stack")
	}
}

func TestRunDaemonServesUnconfiguredAndBootConstructionFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		host      string
		construct error
	}{
		{name: "no host"},
		{name: "persisted host construction failure", host: "bad", construct: errors.New("bad native host")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "portal-supervisor-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			base := newSupervisorBase(t, tc.host)
			base.Paths.APISock = filepath.Join(dir, "api.sock")
			ctx, cancel := context.WithCancel(context.Background())
			s := newSupervisor(ctx, base, io.Discard)
			if tc.construct != nil {
				s.newStack = func(context.Context, string) (*app.Stack, error) { return nil, tc.construct }
			}
			done := make(chan error, 1)
			go func() { done <- runDaemon(ctx, cancel, base, s) }()
			lc := client.New(base.Paths.APISock)
			deadline := time.Now().Add(3 * time.Second)
			for !lc.Available(context.Background()) {
				if time.Now().After(deadline) {
					t.Fatal("unconfigured daemon did not serve API")
				}
				time.Sleep(time.Millisecond)
			}
			st, err := lc.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Host != "" || st.Master.Up || st.Agent != nil {
				t.Fatalf("unconfigured status = %+v", st)
			}
			for _, path := range []string{"/v1/doctor", "/v1/exec?arg=true"} {
				status, code := unixRequestError(t, base.Paths.APISock, http.MethodPost, path)
				if status != http.StatusServiceUnavailable || code != "not_configured" {
					t.Fatalf("POST %s = %d/%q, want 503/not_configured", path, status, code)
				}
			}
			status, code := unixRequestError(t, base.Paths.APISock, http.MethodGet, "/v1/ports")
			if status != http.StatusServiceUnavailable || code != "not_configured" {
				t.Fatalf("GET /v1/ports = %d/%q, want 503/not_configured", status, code)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("runDaemon returned %v", err)
			}
		})
	}
}

func unixRequestError(t *testing.T, socket, method, path string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	req, err := http.NewRequest(method, "http://unix"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body api.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body.Error.Code
}
