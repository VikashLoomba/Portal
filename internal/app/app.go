package app

import (
	"fmt"
	"io"
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
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshnative"
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
	// App.Ports serves the engine's LOCAL-port conflict queries; the master-
	// forward truth behind PortForwarder List/Lines is wired inside NewTransport.
	ports := proc.New(LsofPath, runner)
	// NewProd routes ssh stderr to os.Stderr so launchd's StandardErrorPath
	// captures host-key churn / mux warnings — the DESIGN-split-daemon invariant
	// (bash relied on stderr inheritance). An invalid transport config is a LOUD
	// startup failure here, never a silent fallback.
	tr, pf, err := NewTransport(paths, host, runner, cfg, os.Stderr)
	if err != nil {
		return nil, err
	}
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
	bs := bootstrap.New(tr, slogger)
	h := hub.New()
	ac := agentclient.New(agentclient.Config{
		Transport:  tr,
		Bootstrap:  bs,
		Log:        slogger,
		StderrSink: os.Stderr,
		Hub:        h,
	})
	rd := discover.NewAgent(ac)

	return &App{
		Paths: paths, Cfg: cfg, Runner: runner, Clk: clk, Log: logf,
		Audit:     audit.New(paths.ConfigDir),
		Transport: tr, PF: pf, Ports: ports, Discover: rd, Service: svc,
		Bootstrap: bs, AgentClient: ac, Hub: h,
	}, nil
}

// NewTransport is the ONE selection-aware transport factory (T8) and the ONLY
// place transports are constructed. It reads cfg.Transport() and builds the
// selected implementation:
//
//   - "system" (the default when the transport file is absent): sshctl.New over
//     the ControlMaster socket, wired with the lsof-backed master-forward source
//     and the caller-supplied ssh-stderr sink.
//   - "native": sshnative.New against the configured user@host[:port], using the
//     real ~/.ssh defaults (no Options).
//
// An invalid config value returns the error unchanged — a loud startup failure,
// NEVER a silent fallback to system.
//
// sshStderr is the ssh-stderr sink for the SYSTEM transport only (nil = quiet,
// no tee). NewProd passes os.Stderr so ssh warnings reach launchd's log (the
// DESIGN-split-daemon invariant); every DOCTOR-path caller passes nil so ssh
// stderr is never tee'd into the report. The native transport has no ambient
// ssh-stderr stream to tee (each session captures its own stderr), so sshStderr
// does NOT apply to it.
//
// The concrete *sshctl.SSH and *sshnative.Client each satisfy BOTH
// transport.Transport and transport.PortForwarder at compile time, so the
// returned pair needs no runtime assertion.
func NewTransport(paths Paths, host string, runner run.Runner, cfg *config.Store, sshStderr io.Writer) (transport.Transport, transport.PortForwarder, error) {
	sel, err := cfg.Transport()
	if err != nil {
		return nil, nil, err
	}
	switch sel {
	case "native":
		c, err := sshnative.New(host)
		if err != nil {
			return nil, nil, err
		}
		return c, c, nil
	default: // "system"
		s := sshctl.New(paths.Sock, host, SSHOpts, runner)
		s.Forwards = proc.New(LsofPath, runner)
		s.StderrSink = sshStderr // nil-safe: sshctl guards a nil sink.
		return s, s, nil
	}
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
