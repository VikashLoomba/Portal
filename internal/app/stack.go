package app

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/VikashLoomba/Portal/internal/bootstrap"
	"github.com/VikashLoomba/Portal/internal/clock"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/discover"
	"github.com/VikashLoomba/Portal/internal/forward"
	"github.com/VikashLoomba/Portal/internal/proc"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/run"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/sshnative"
)

// Stack is the unstarted host-bound portion of the daemon.
type Stack struct {
	Host        string
	Paths       Paths
	Transport   transport.Transport
	PF          transport.PortForwarder
	Bootstrap   *bootstrap.Manager
	AgentClient *agentclient.Client
	Discover    discover.RemoteDiscoverer
	Engine      *forward.Engine
	OpenURLCh   chan string

	agentIn  <-chan agentclient.EngineEvent
	agentOut chan<- forward.EngineEvent
}

// NewStack wires one host without starting any goroutines or remote work.
func NewStack(ctx context.Context, paths Paths, cfg *config.Store, h *hub.Hub, host string,
	runner run.Runner, clk clock.Clock, log forward.Logger, sshStderr io.Writer,
	nativeOpts ...sshnative.Option) (*Stack, error) {

	// The system-transport branch performs no I/O, so without this guard a
	// setup request canceled before activation would construct "successfully"
	// and proceed into teardown of the live stack.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tr, pf, err := NewTransportContext(ctx, paths, host, runner, cfg, sshStderr, nativeOpts...)
	if err != nil {
		return nil, err
	}
	logSink := sshStderr
	if logSink == nil {
		logSink = os.Stderr
	}
	slogger := slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	bs := bootstrap.New(tr, slogger)
	ac := agentclient.New(agentclient.Config{
		Transport:  tr,
		Bootstrap:  bs,
		Log:        slogger,
		StderrSink: sshStderr,
		Hub:        h,
		ClipShim:   clipShimAdapter{},
	})
	rd := discover.NewAgent(ac)
	e := forward.New(tr, pf, proc.New(LsofPath, runner), rd, cfg, clk, log,
		Interval, DenyPorts, SkipLocal)
	out := make(chan forward.EngineEvent, cap(ac.Events())+8)
	e.AgentEvents = out
	openURLCh := make(chan string, 16)
	e.OpenURLSink = openURLCh

	return &Stack{
		Host: host, Paths: paths, Transport: tr, PF: pf, Bootstrap: bs,
		AgentClient: ac, Discover: rd, Engine: e, OpenURLCh: openURLCh,
		agentIn: ac.Events(), agentOut: out,
	}, nil
}

// RunAgentEventPump adapts agent events under the stack lifetime.
func (s *Stack) RunAgentEventPump(ctx context.Context) {
	if s.agentIn == nil || s.agentOut == nil {
		return
	}
	pumpAgentEvents(ctx, s.agentIn, s.agentOut)
}

// Shutdown asks the remote agent to exit before the stack context is canceled.
func (s *Stack) Shutdown(ctx context.Context) error {
	if s.AgentClient == nil {
		return nil
	}
	return s.AgentClient.Shutdown(ctx, "activate")
}

// CloseTransport tears down the host-bound transport.
func (s *Stack) CloseTransport(ctx context.Context) (bool, error) {
	if s.Transport == nil {
		return false, nil
	}
	return s.Transport.Close(ctx)
}

// RemoveSocket releases the shared ControlPath before another stack starts.
func (s *Stack) RemoveSocket() error {
	if s.Paths.Sock == "" {
		return nil
	}
	err := os.Remove(s.Paths.Sock)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
