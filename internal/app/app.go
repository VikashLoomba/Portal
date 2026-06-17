package app

import (
	"fmt"
	"os"
	"os/user"
	"strconv"

	"github.com/vikashl/portal/internal/clock"
	"github.com/vikashl/portal/internal/config"
	"github.com/vikashl/portal/internal/discover"
	"github.com/vikashl/portal/internal/forward"
	"github.com/vikashl/portal/internal/proc"
	"github.com/vikashl/portal/internal/run"
	"github.com/vikashl/portal/internal/service"
	"github.com/vikashl/portal/internal/sshctl"
)

// App is the dependency container. NewProd wires real adapters; tests build
// it directly with fakes.
type App struct {
	Paths    Paths
	Cfg      *config.Store
	Runner   run.Runner
	Clk      clock.Clock
	Log      forward.Logger
	Transport sshctl.Transport
	Ports    proc.PortLister
	Discover discover.RemoteDiscoverer
	Service  service.Manager
}

// NewProd builds an App for normal use: reads HOME, derives Paths, opens the
// config store, and constructs production adapters. The ssh Transport is
// built around the host stored in the config file (empty until `install`).
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
	log := forward.StdoutLogger()
	transport := sshctl.New(paths.Sock, host, SSHOpts, runner)
	// Tee ssh stderr to our stderr so launchd's StandardErrorPath captures
	// host-key churn / mux warnings — bash relies on stderr inheritance.
	transport.StderrSink = os.Stderr
	ports := proc.New(LsofPath, runner)
	rd := discover.New(transport)
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

	return &App{
		Paths: paths, Cfg: cfg, Runner: runner, Clk: clk, Log: log,
		Transport: transport, Ports: ports, Discover: rd, Service: svc,
	}, nil
}

// Engine constructs a fresh forward.Engine using the App's wiring. Lives
// here (not on App) so commands that don't need the loop don't pay for it.
func (a *App) Engine() *forward.Engine {
	return forward.New(a.Transport, a.Ports, a.Discover, a.Cfg, a.Clk, a.Log,
		Interval, DenyPorts, SkipLocal)
}
