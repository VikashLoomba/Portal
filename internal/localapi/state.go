// Package localapi is the local control-plane HTTP server: HTTP/1.1 + JSON over
// a Unix socket, mirroring the remote cmd socket's trust model (dir 0700, socket
// 0600, peer-uid check). It is the single implementation of portal's status
// aggregate and mutation endpoints; every frontend (the CLI today, desktop
// shells later) is a thin client of it. See DESIGN-local-core-api.md §4.
package localapi

import (
	"context"
	"io"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/service"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

// AgentSource is the narrow read view of the in-process agentclient.Client the
// status aggregate needs. Satisfied by *agentclient.Client; faked in tests.
type AgentSource interface {
	HelloAck() *protocol.HelloAck
	Snapshot() (seq uint64, ports []uint16, ok bool)
	LastDisconnectErr() string
}

// MasterProber probes transport liveness and identity (subset of
// transport.Transport). Health.Up is the liveness gate; Describe().Impl feeds
// the additive Status.Master.transport field.
type MasterProber interface {
	Health(ctx context.Context) (transport.Health, error)
	Describe() transport.Desc
}

// ForwardLister lists active local forwards (subset of transport.PortForwarder).
type ForwardLister interface {
	ForwardLines(ctx context.Context) ([]string, error)
}

// ServiceStater reports launchd service state (subset of service.Manager).
type ServiceStater interface {
	Status(ctx context.Context) (service.Status, error)
}

// ConfigStore is the file-backed allowlist + feature gates (subset of
// *config.Store; a real *config.Store over t.TempDir is fine in tests).
type ConfigStore interface {
	AllowedPorts() ([]int, error)
	Allow(ports []int) ([]int, error)
	Unallow(ports []int) error
	FeatureEnabled(feature string) bool
	SetFeature(feature string, on bool) error
}

// ExecStreamer is the live byte-stream command capability. The argv contract is
// Transport.Stream's shell-join contract; callers must pass argv through
// verbatim without adding another shell.
type ExecStreamer interface {
	Stream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error)
	Describe() transport.Desc
}

// SetupRunner is one fresh host setup run. Phase implementations own their
// running, line, and terminal event emissions through the sink passed to
// Deps.NewSetup.
type SetupRunner interface {
	Validate(ctx context.Context, host string, force bool) (proceed bool)
	Configure(ctx context.Context, host string) error
	DeployRemote(ctx context.Context, host string)
	Verify(ctx context.Context, host string) *doctor.Report
	Close(ctx context.Context)
}

// Deps is the dependency set for a Server. The interface fields are narrow so
// tests fake them without constructing an App; Doctor/PushAllow/Kick are
// closures wired by run.go so localapi never imports package app. FeatureNames
// defaults to [clip-image, clip-text, notify, exec, cred] when empty.
type Deps struct {
	Version       api.VersionInfo
	Host          func() (string, error)
	Agent         AgentSource
	Master        MasterProber
	Ports         ForwardLister
	Service       ServiceStater
	Config        ConfigStore
	Hub           *hub.Hub
	ExecStream    ExecStreamer
	Audit         *audit.Log
	PushAllow     func([]int) error
	Kick          func()
	ReconcileGen  func() uint64
	Doctor        func(context.Context) *doctor.Report
	NewSetup      func(sink func(api.SetupEvent)) SetupRunner
	Activate      func(context.Context, string) error
	NormalizeHost func(string) string
	FeatureNames  []string
}
