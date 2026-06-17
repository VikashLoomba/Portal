// Package service abstracts platform-specific service managers. Today only
// the darwin/launchd impl exists; a future systemd backend would add a Linux
// build-tagged file alongside.
package service

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by stub implementations on platforms with no
// service manager wired up yet.
var ErrUnsupported = errors.New("service manager not supported on this OS")

// Spec is everything the service manager needs to know to install and run
// the daemon. Fields are populated from app.Paths.
type Spec struct {
	Label    string   // local.portal.autoforward
	BinPath  string   // ~/.local/bin/portal
	Args     []string // ["run"]
	LogPath  string   // ~/Library/Logs/portal.log
	Plist    string   // ~/Library/LaunchAgents/<label>.plist
	Domain   string   // gui/<uid>
	EnvPATH  string
	Home     string
}

// Status is a snapshot for the `status` command.
type Status struct {
	Loaded bool
	// StateLines are the raw "state/pid/runs/last exit code" lines from
	// `launchctl print` (each indented by two spaces, matching the bash
	// formatting). Empty when not loaded.
	StateLines []string
}

// Manager is the OS-agnostic surface.
type Manager interface {
	Install(ctx context.Context) error
	Uninstall(ctx context.Context) error
	Reload(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	IsLoaded(ctx context.Context) (bool, error)
	Status(ctx context.Context) (Status, error)
}
