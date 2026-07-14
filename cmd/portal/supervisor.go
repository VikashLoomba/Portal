package main

import (
	"context"
	"errors"
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
	wg     sync.WaitGroup

	mu       sync.Mutex
	draining bool
}

type supervisor struct {
	daemonCtx context.Context
	base      *app.App
	ref       atomic.Pointer[liveStack]
	mu        sync.Mutex

	newStack     func(context.Context, string) (*app.Stack, error)
	drainTimeout time.Duration
}

func newSupervisor(ctx context.Context, base *app.App, sshStderr io.Writer) *supervisor {
	s := &supervisor{daemonCtx: ctx, base: base, drainTimeout: stackDrainTimeout}
	s.newStack = func(ctx context.Context, host string) (*app.Stack, error) {
		return app.NewStack(ctx, base.Paths, base.Cfg, base.Hub, host,
			base.Runner, base.Clk, base.Log, sshStderr)
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
	return &liveStack{stack: stack, ctx: ctx, cancel: cancel}
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
		s.drain(old)
	}

	ls := s.newLiveStack(ns)
	s.ref.Store(ls)
	s.start(ls)
	if s.base.Hub != nil {
		s.base.Hub.Publish(hub.Event{Class: hub.Coalesced})
	}
	return nil
}

func (s *supervisor) drain(old *liveStack) {
	timeout := s.drainTimeout
	if timeout <= 0 {
		timeout = stackDrainTimeout
	}
	teardownCtx, teardownCancel := context.WithTimeout(context.Background(), timeout)
	defer teardownCancel()

	_ = old.stack.Shutdown(teardownCtx)
	old.cancel()
	_, _ = old.stack.CloseTransport(teardownCtx)
	_ = old.stack.RemoveSocket()
	s.wait(old, timeout)
}

func (s *supervisor) wait(ls *liveStack, timeout time.Duration) {
	if timeout <= 0 {
		timeout = stackDrainTimeout
	}
	done := make(chan struct{})
	go func() {
		ls.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (s *supervisor) waitCurrent() {
	if ls := s.current(); ls != nil {
		s.wait(ls, s.drainTimeout)
	}
}

func bindStackContext(caller, stack context.Context) (context.Context, context.CancelFunc) {
	if caller == nil {
		caller = context.Background()
	}
	ctx, cancel := context.WithCancel(caller)
	go func() {
		select {
		case <-stack.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
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
	if ls, ok := m.s.serving(); ok {
		return ls.stack.Transport.Health(ctx)
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
	if ls, ok := p.s.serving(); ok {
		return ls.stack.PF.ForwardLines(ctx)
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
	sctx, cancel := bindStackContext(ctx, ls.ctx)
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
	sctx, cancel := bindStackContext(ctx, ls.ctx)
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
	if ls, ok := s.serving(); ok {
		return ls.stack.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)
	}
	return errNotConfigured
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
	sctx, cancel := bindStackContext(ctx, ls.ctx)
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
