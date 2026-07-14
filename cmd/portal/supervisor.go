package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

const stackDrainTimeout = 2 * time.Second

var errNotConfigured = errors.New("portal: no active host stack configured")

type liveStack struct {
	stack  *app.Stack
	ctx    context.Context
	cancel context.CancelFunc
	wg     lifecycleGroup

	mu                 sync.Mutex
	draining           bool
	masterStopRequired bool
}

// lifecycleGroup exposes completion without creating an uncancelable waiter
// goroutine for every bounded drain attempt.
type lifecycleGroup struct {
	mu     sync.Mutex
	active int
	done   chan struct{}
}

func (g *lifecycleGroup) Add(delta int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := g.active + delta
	if next < 0 {
		panic("portal: negative lifecycle group count")
	}
	if g.active == 0 && next > 0 {
		g.done = make(chan struct{})
	}
	wasActive := g.active > 0
	g.active = next
	if wasActive && next == 0 {
		close(g.done)
	}
}

func (g *lifecycleGroup) Done() { g.Add(-1) }

func (g *lifecycleGroup) DoneChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done == nil {
		g.done = make(chan struct{})
		close(g.done)
	}
	return g.done
}

type supervisor struct {
	daemonCtx context.Context
	base      *app.App
	ref       atomic.Pointer[liveStack]
	mu        sync.Mutex

	newStack       func(context.Context, string) (*app.Stack, error)
	shutdownStack  func(context.Context, *app.Stack) error
	pushAllowStack func(*app.Stack, []int) error
	drainTimeout   time.Duration
}

func newSupervisor(ctx context.Context, base *app.App, sshStderr io.Writer) *supervisor {
	s := &supervisor{daemonCtx: ctx, base: base, drainTimeout: stackDrainTimeout}
	s.newStack = func(ctx context.Context, host string) (*app.Stack, error) {
		return app.NewStack(ctx, base.Paths, base.Cfg, base.Hub, host,
			base.Runner, base.Clk, base.Log, sshStderr)
	}
	s.shutdownStack = func(ctx context.Context, stack *app.Stack) error {
		return stack.Shutdown(ctx)
	}
	s.pushAllowStack = func(stack *app.Stack, allow []int) error {
		return stack.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)
	}
	return s
}

func (s *supervisor) current() *liveStack { return s.ref.Load() }

func (s *supervisor) serving() (*liveStack, bool) {
	ls := s.current()
	if ls == nil {
		return nil, false
	}
	ls.mu.Lock()
	draining := ls.draining
	ls.mu.Unlock()
	if draining {
		return nil, false
	}
	return ls, true
}

func (s *supervisor) startInitial(ctx context.Context, host string) error {
	ns, err := s.newStack(ctx, host)
	if err != nil {
		return err
	}
	ls := s.newLiveStack(ns)
	s.ref.Store(ls)
	s.start(ls)
	return nil
}

func (s *supervisor) newLiveStack(stack *app.Stack) *liveStack {
	ctx, cancel := context.WithCancel(s.daemonCtx)
	ls := &liveStack{stack: stack, ctx: ctx, cancel: cancel}
	if stack.Transport.Describe().Impl == transport.ImplSystemSSH {
		ls.masterStopRequired = true
	}
	return ls
}

func (s *supervisor) start(ls *liveStack) {
	allow, _ := s.base.Cfg.AllowedPorts()
	_ = ls.stack.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)

	sa := *s.base
	sa.Transport = ls.stack.Transport
	sa.PF = ls.stack.PF
	sa.AgentClient = ls.stack.AgentClient
	sa.Bootstrap = ls.stack.Bootstrap
	sa.Discover = ls.stack.Discover

	launch := func(run func()) {
		ls.wg.Add(1)
		go func() {
			defer ls.wg.Done()
			run()
		}()
	}
	launch(func() {
		if err := ls.stack.AgentClient.Run(ls.ctx); err != nil && ls.ctx.Err() == nil {
			s.base.Log.Logf("agent loop stopped: %v", err)
		}
	})
	launch(func() {
		if err := ls.stack.Engine.Run(ls.ctx); err != nil && ls.ctx.Err() == nil {
			s.base.Log.Logf("forward loop stopped: %v", err)
		}
	})
	launch(func() { ls.stack.RunAgentEventPump(ls.ctx) })
	launch(func() { runOpenURLHandler(ls.ctx, ls.stack.OpenURLCh, &sa) })
	launch(func() { runClipHandler(ls.ctx, ls.stack.AgentClient.ClipEvents(), &sa, &ls.wg) })
	launch(func() { runCredHandler(ls.ctx, ls.stack.AgentClient.CredEvents(), &sa, &ls.wg) })
	launch(func() { runNotifyHandler(ls.ctx, ls.stack.AgentClient.NotifyEvents(), &sa) })
}

// Activate commits teardown only after the replacement stack is constructed.
func (s *supervisor) Activate(ctx context.Context, host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.current()
	if old != nil && old.stack.Host == host {
		old.mu.Lock()
		draining := old.draining
		old.mu.Unlock()
		if !draining {
			return nil
		}
	}

	ns, err := s.newStack(ctx, host)
	if err != nil {
		return err
	}
	if old != nil {
		old.mu.Lock()
		old.draining = true
		old.mu.Unlock()
		if err := s.drain(ctx, old); err != nil {
			return err
		}
	}

	ls := s.newLiveStack(ns)
	s.ref.Store(ls)
	s.start(ls)
	if s.base.Hub != nil {
		s.base.Hub.Publish(hub.Event{Class: hub.Coalesced})
	}
	return nil
}

func (s *supervisor) drain(ctx context.Context, old *liveStack) error {
	timeout := s.drainTimeout
	if timeout <= 0 {
		timeout = stackDrainTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	teardownCtx, teardownCancel := context.WithTimeout(ctx, timeout)
	defer teardownCancel()

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.shutdownStack(teardownCtx, old.stack) }()
	var shutdownErr error
	select {
	case shutdownErr = <-shutdownDone:
	case <-teardownCtx.Done():
		shutdownErr = teardownCtx.Err()
	}
	old.cancel()
	quiesceErr := s.wait(teardownCtx, old)
	closeDone := make(chan error, 1)
	old.wg.Add(1)
	go func() {
		defer old.wg.Done()
		closeDone <- closeStackTransport(teardownCtx, old)
	}()
	var closeErr error
	select {
	case closeErr = <-closeDone:
	case <-teardownCtx.Done():
		closeErr = teardownCtx.Err()
	}
	removeErr := old.stack.RemoveSocket()
	waitErr := s.wait(teardownCtx, old)
	return errors.Join(
		wrapDrainError("shutdown old agent", shutdownErr),
		wrapDrainError("quiesce old stack", quiesceErr),
		wrapDrainError("close old transport", closeErr),
		wrapDrainError("remove old control socket", removeErr),
		wrapDrainError("wait for old stack", waitErr),
	)
}

func closeStackTransport(ctx context.Context, old *liveStack) error {
	stack := old.stack
	systemSSH := stack.Transport.Describe().Impl == transport.ImplSystemSSH
	var healthErr error
	stopRequired := false
	if systemSSH {
		old.mu.Lock()
		stopRequired = old.masterStopRequired
		old.mu.Unlock()
	}
	if systemSSH && !stopRequired {
		health, err := stack.Transport.Health(ctx)
		healthErr = err
		stopRequired = health.Up || err != nil
		if stopRequired {
			old.mu.Lock()
			old.masterStopRequired = true
			old.mu.Unlock()
		}
	}
	stopped, closeErr := stack.CloseTransport(ctx)
	if stopped && systemSSH {
		old.mu.Lock()
		old.masterStopRequired = false
		old.mu.Unlock()
	}
	if !stopped && stopRequired {
		return errors.Join(closeErr, healthErr, errors.New("transport did not confirm shutdown"))
	}
	return closeErr
}

func wrapDrainError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func (s *supervisor) wait(ctx context.Context, ls *liveStack) error {
	select {
	case <-ls.wg.DoneChan():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *supervisor) waitCurrent() {
	if ls := s.current(); ls != nil {
		timeout := s.drainTimeout
		if timeout <= 0 {
			timeout = stackDrainTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = s.wait(ctx, ls)
	}
}

func bindStackContext(caller, stack context.Context) (context.Context, context.CancelFunc) {
	if stack == nil {
		stack = context.Background()
	}
	// The stack must be the direct parent so cancellation is visible before a
	// stale request can reach a replacement master's shared ControlPath.
	ctx, cancel := context.WithCancel(stack)
	if caller == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(caller, cancel)
	if caller.Err() != nil {
		cancel()
	}
	return stackBoundContext{Context: ctx, caller: caller}, func() {
		stop()
		cancel()
	}
}

type stackBoundContext struct {
	context.Context
	caller context.Context
}

func (c stackBoundContext) Deadline() (time.Time, bool) {
	stackDeadline, stackOK := c.Context.Deadline()
	callerDeadline, callerOK := c.caller.Deadline()
	if !stackOK || callerOK && callerDeadline.Before(stackDeadline) {
		return callerDeadline, callerOK
	}
	return stackDeadline, true
}

func (c stackBoundContext) Value(key any) any {
	if value := c.caller.Value(key); value != nil {
		return value
	}
	return c.Context.Value(key)
}

func (ls *liveStack) bindContext(caller context.Context) (context.Context, context.CancelFunc, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.draining || ls.ctx.Err() != nil {
		return nil, nil, errNotConfigured
	}
	ls.wg.Add(1)
	ctx, cancel := bindStackContext(caller, ls.ctx)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			ls.wg.Done()
		})
	}, nil
}

type pinnedStack struct {
	base *app.App
	ls   *liveStack
	ctx  context.Context
}

func (s *supervisor) pin(ctx context.Context) (*pinnedStack, func()) {
	ls, ok := s.serving()
	if !ok {
		return nil, func() {}
	}
	pctx, release, err := ls.bindContext(ctx)
	if err != nil {
		return nil, func() {}
	}
	return &pinnedStack{base: s.base, ls: ls, ctx: pctx}, release
}

func (p *pinnedStack) operationContext(caller context.Context) (context.Context, context.CancelFunc) {
	return bindStackContext(caller, p.ctx)
}

func (p *pinnedStack) host() string { return p.ls.stack.Host }

func (p *pinnedStack) HelloAck() *protocol.HelloAck {
	return p.ls.stack.AgentClient.HelloAck()
}

func (p *pinnedStack) Snapshot() (uint64, []uint16, bool) {
	return p.ls.stack.AgentClient.Snapshot()
}

func (p *pinnedStack) LastDisconnectErr() string {
	return p.ls.stack.AgentClient.LastDisconnectErr()
}

func (p *pinnedStack) Health(ctx context.Context) (transport.Health, error) {
	sctx, cancel := p.operationContext(ctx)
	defer cancel()
	return p.ls.stack.Transport.Health(sctx)
}

func (p *pinnedStack) Describe() transport.Desc {
	return p.ls.stack.Transport.Describe()
}

func (p *pinnedStack) ForwardLines(ctx context.Context) ([]string, error) {
	sctx, cancel := p.operationContext(ctx)
	defer cancel()
	return p.ls.stack.PF.ForwardLines(sctx)
}

func (p *pinnedStack) Stream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	sctx, cancel := p.operationContext(ctx)
	stdin, stdout, stderr, wait, err := p.ls.stack.Transport.Stream(sctx, argv...)
	if err != nil {
		cancel()
		return nil, nil, nil, nil, err
	}
	if wait == nil {
		cancel()
		return stdin, stdout, stderr, nil, nil
	}
	var once sync.Once
	return stdin, stdout, stderr, func() error {
		defer once.Do(cancel)
		return wait()
	}, nil
}

func (p *pinnedStack) StreamPty(ctx context.Context, req transport.PtyRequest, argv ...string) (transport.PtySession, error) {
	streamer, ok := p.ls.stack.Transport.(transport.PtyStreamer)
	if !ok {
		return nil, errors.New("portal: active transport does not support PTY")
	}
	sctx, cancel := p.operationContext(ctx)
	sess, err := streamer.StreamPty(sctx, req, argv...)
	if err != nil {
		cancel()
		return nil, err
	}
	if sess == nil {
		cancel()
		return nil, nil
	}
	return &supervisorPtySession{PtySession: sess, cancel: cancel}, nil
}

func (p *pinnedStack) reconciles() uint64 {
	return p.ls.stack.Engine.Reconciles()
}

func (p *pinnedStack) doctor(ctx context.Context) *doctor.Report {
	sctx, cancel := p.operationContext(ctx)
	defer cancel()
	sa := *p.base
	sa.Transport = p.ls.stack.Transport
	tr, err := doctorTransport(sctx, &sa, p.ls.stack.Host)
	if err != nil {
		rep := &doctor.Report{Host: p.ls.stack.Host}
		rep.Add("transport", doctor.Fail, "transport unavailable: "+err.Error())
		return rep
	}
	return runDoctor(sctx, p.ls.stack.Host, tr)
}

type supervisorAgent struct{ s *supervisor }

func (a supervisorAgent) HelloAck() *protocol.HelloAck {
	if ls, ok := a.s.serving(); ok {
		return ls.stack.AgentClient.HelloAck()
	}
	return nil
}

func (a supervisorAgent) Snapshot() (uint64, []uint16, bool) {
	if ls, ok := a.s.serving(); ok {
		return ls.stack.AgentClient.Snapshot()
	}
	return 0, nil, false
}

func (a supervisorAgent) LastDisconnectErr() string {
	if ls, ok := a.s.serving(); ok {
		return ls.stack.AgentClient.LastDisconnectErr()
	}
	return ""
}

type supervisorMaster struct{ s *supervisor }

func (m supervisorMaster) Health(ctx context.Context) (transport.Health, error) {
	ls, ok := m.s.serving()
	if ok {
		sctx, cancel, err := ls.bindContext(ctx)
		if err != nil {
			return transport.Health{Up: false, Detail: errNotConfigured.Error()}, nil
		}
		defer cancel()
		return ls.stack.Transport.Health(sctx)
	}
	return transport.Health{Up: false, Detail: errNotConfigured.Error()}, nil
}

func (m supervisorMaster) Describe() transport.Desc {
	if ls, ok := m.s.serving(); ok {
		return ls.stack.Transport.Describe()
	}
	return transport.Desc{Impl: transport.ImplUnavailable, Endpoint: errNotConfigured.Error()}
}

type supervisorPorts struct{ s *supervisor }

func (p supervisorPorts) ForwardLines(ctx context.Context) ([]string, error) {
	ls, ok := p.s.serving()
	if ok {
		sctx, cancel, err := ls.bindContext(ctx)
		if err != nil {
			return []string{}, nil
		}
		defer cancel()
		return ls.stack.PF.ForwardLines(sctx)
	}
	return []string{}, nil
}

type supervisorExec struct{ s *supervisor }

var _ transport.PtyStreamer = supervisorExec{}

func (e supervisorExec) Describe() transport.Desc {
	if ls, ok := e.s.serving(); ok {
		return ls.stack.Transport.Describe()
	}
	return transport.Desc{Impl: transport.ImplUnavailable, Endpoint: errNotConfigured.Error()}
}

func (e supervisorExec) Stream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	ls, ok := e.s.serving()
	if !ok {
		return nil, nil, nil, nil, errNotConfigured
	}
	sctx, cancel, err := ls.bindContext(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stdin, stdout, stderr, wait, err := ls.stack.Transport.Stream(sctx, argv...)
	if err != nil {
		cancel()
		return nil, nil, nil, nil, err
	}
	if wait == nil {
		cancel()
		return stdin, stdout, stderr, nil, nil
	}
	var once sync.Once
	return stdin, stdout, stderr, func() error {
		defer once.Do(cancel)
		return wait()
	}, nil
}

func (e supervisorExec) StreamPty(ctx context.Context, req transport.PtyRequest, argv ...string) (transport.PtySession, error) {
	ls, ok := e.s.serving()
	if !ok {
		return nil, errNotConfigured
	}
	streamer, ok := ls.stack.Transport.(transport.PtyStreamer)
	if !ok {
		return nil, errors.New("portal: active transport does not support PTY")
	}
	sctx, cancel, err := ls.bindContext(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := streamer.StreamPty(sctx, req, argv...)
	if err != nil {
		cancel()
		return nil, err
	}
	if sess == nil {
		cancel()
		return nil, nil
	}
	return &supervisorPtySession{PtySession: sess, cancel: cancel}, nil
}

type supervisorPtySession struct {
	transport.PtySession
	cancel context.CancelFunc
	once   sync.Once
}

func (s *supervisorPtySession) Wait() error {
	defer s.once.Do(s.cancel)
	return s.PtySession.Wait()
}

func (s *supervisorPtySession) Close() error {
	defer s.once.Do(s.cancel)
	return s.PtySession.Close()
}

func (s *supervisor) host() (string, error) {
	if ls, ok := s.serving(); ok {
		return ls.stack.Host, nil
	}
	return "", nil
}

func (s *supervisor) pushAllow(allow []int) error {
	s.mu.Lock()
	ls, ok := s.serving()
	if !ok {
		s.mu.Unlock()
		return errNotConfigured
	}
	_, release, err := ls.bindContext(context.Background())
	s.mu.Unlock()
	if err != nil {
		return err
	}
	defer release()
	return s.pushAllowStack(ls.stack, allow)
}

func (s *supervisor) kick() {
	if ls, ok := s.serving(); ok {
		ls.stack.Engine.Kick()
	}
}

func (s *supervisor) reconciles() uint64 {
	if ls, ok := s.serving(); ok {
		return ls.stack.Engine.Reconciles()
	}
	return 0
}

func (s *supervisor) doctor(ctx context.Context) *doctor.Report {
	ls, ok := s.serving()
	if !ok {
		rep := &doctor.Report{}
		rep.Add("transport", doctor.Fail, errNotConfigured.Error())
		return rep
	}
	sctx, cancel, err := ls.bindContext(ctx)
	if err != nil {
		rep := &doctor.Report{}
		rep.Add("transport", doctor.Fail, errNotConfigured.Error())
		return rep
	}
	defer cancel()
	sa := *s.base
	sa.Transport = ls.stack.Transport
	tr, err := doctorTransport(sctx, &sa, ls.stack.Host)
	if err != nil {
		rep := &doctor.Report{Host: ls.stack.Host}
		rep.Add("transport", doctor.Fail, "transport unavailable: "+err.Error())
		return rep
	}
	return runDoctor(sctx, ls.stack.Host, tr)
}
