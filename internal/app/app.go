package app

import (
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strconv"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agentclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/audit"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/bootstrap"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clock"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/discover"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/forward"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/proc"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/run"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/service"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// App is the dependency container. NewProd wires real adapters; tests build
// it directly with fakes.
type App struct {
	Paths     Paths
	Cfg       *config.Store
	Runner    run.Runner
	Clk       clock.Clock
	Log       forward.Logger
	Audit     *audit.Log
	Transport transport.Transport
	// PF is the port-forwarding capability, acquired by type assertion at
	// wiring time (the daemon requires it). run.go/inspect.go reach
	// Forward/Cancel/ListForwards/ForwardLines ONLY through PF — those are
	// never on the core Transport interface.
	PF       transport.PortForwarder
	Ports    forward.LocalPorts
	Discover discover.RemoteDiscoverer
	Service  service.Manager

	// Split-daemon additions:
	Bootstrap   *bootstrap.Manager
	AgentClient *agentclient.Client

	// Hub is the read-only fan-out tee that agentclient publishes state and
	// notify events into; internal/localapi's events stream subscribes to it.
	// nil is tolerated everywhere (tests build App directly with fakes).
	Hub *hub.Hub
}

// NewProd builds an App for normal use: reads HOME, derives Paths, opens
// the config store, constructs production adapters, and wires the
// agentclient + bootstrap manager so the engine is event-driven against
// the remote portald agent.
func NewProd() (*App, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("UserHomeDir: %w", err)
	}
	uid := os.Getuid()
	if u, err := user.Current(); err == nil {
		if n, err := strconv.Atoi(u.Uid); err == nil {
			uid = n
		}
	}
	paths := DerivePaths(home, uid)
	cfg := config.New(paths.ConfigDir)
	host, _ := cfg.ReadHost()

	runner := run.OSRunner{}
	clk := clock.Real{}
	logf := forward.StdoutLogger()
	// ONE *proc.Lsof serves both App.Ports (local-port conflict queries) and
	// s.Forwards (the master-forward truth behind PortForwarder List/Lines).
	ports := proc.New(LsofPath, runner)
	s := sshctl.New(paths.Sock, host, SSHOpts, runner)
	// Tee ssh stderr to our stderr so launchd's StandardErrorPath captures
	// host-key churn / mux warnings — bash relies on stderr inheritance.
	// (The u5 selection-aware factory will pass the sink through explicitly;
	// this unit keeps NewProd's direct construction with the sink set inline.)
	s.StderrSink = os.Stderr
	s.Forwards = ports
	svc := service.New(service.Spec{
		Label:   paths.Label,
		BinPath: paths.BinPath,
		Args:    []string{"run"},
		LogPath: paths.Log,
		Plist:   paths.Plist,
		Domain:  paths.Domain,
		EnvPATH: PlistPATH,
		Home:    paths.Home,
	}, runner, clk)

	// Agent layer: bootstrap manager + client. The client's Events()
	// channel is consumed by forward.Engine; AgentDiscoverer reads its
	// cached Snapshot for desired-port lookups.
	slogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	bs := bootstrap.New(s, slogger)
	h := hub.New()
	ac := agentclient.New(agentclient.Config{
		Transport:  s,
		Bootstrap:  bs,
		Log:        slogger,
		StderrSink: os.Stderr,
		Hub:        h,
	})
	rd := discover.NewAgent(ac)

	// The daemon requires port forwarding; assert the capability loudly at
	// wiring time rather than failing deep in the engine.
	var tr transport.Transport = s
	pf, ok := tr.(transport.PortForwarder)
	if !ok {
		return nil, fmt.Errorf("transport %s does not implement PortForwarder", tr.Describe().Impl)
	}

	return &App{
		Paths: paths, Cfg: cfg, Runner: runner, Clk: clk, Log: logf,
		Audit:     audit.New(paths.ConfigDir),
		Transport: tr, PF: pf, Ports: ports, Discover: rd, Service: svc,
		Bootstrap: bs, AgentClient: ac, Hub: h,
	}, nil
}

// Engine constructs a fresh forward.Engine using the App's wiring. The
// engine is event-driven via AgentClient.Events(). Callers that want to
// handle OpenURL events should set the returned engine's OpenURLSink before
// calling Run — or use NewEngineWithOpenURL for convenience.
func (a *App) Engine() *forward.Engine {
	e := forward.New(a.Transport, a.PF, a.Ports, a.Discover, a.Cfg, a.Clk, a.Log,
		Interval, DenyPorts, SkipLocal)
	if a.AgentClient != nil {
		e.AgentEvents = adaptAgentEvents(a.AgentClient.Events())
	}
	return e
}

// NewEngineWithOpenURL is like Engine but also returns a channel that
// receives URLs from EvOpenURL events (e.g. xdg-open on the remote).
// The channel is buffered; the engine drops URLs if the consumer is slow.
func (a *App) NewEngineWithOpenURL() (*forward.Engine, <-chan string) {
	e := a.Engine()
	ch := make(chan string, 16)
	e.OpenURLSink = ch
	return e, ch
}

// adaptAgentEvents copies fields from agentclient.EngineEvent into the
// engine-local forward.EngineEvent shape (the engine doesn't import
// agentclient to avoid a layering cycle).
func adaptAgentEvents(in <-chan agentclient.EngineEvent) <-chan forward.EngineEvent {
	out := make(chan forward.EngineEvent, cap(in)+8)
	go func() {
		defer close(out)
		for ev := range in {
			out <- forward.EngineEvent{
				Kind:    forward.EngineEventKind(ev.Kind),
				Err:     ev.Err,
				Added:   ev.Added,
				Removed: ev.Removed,
				URL:     ev.URL,
			}
		}
	}()
	return out
}
