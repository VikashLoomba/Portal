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
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

// VersionInfo is the payload of GET /v1/version: portal version, embedded git
// SHA, and the wire ProtoVersion (D6).
type VersionInfo struct {
	Version      string `json:"version"`
	GitSHA       string `json:"gitSha"`
	ProtoVersion uint32 `json:"protoVersion"`
}

// AgentStatus is the connected agent's handshake identity, sourced from
// protocol.HelloAck. ConnectedFor is deferred (§4.4 is shape-not-final).
type AgentStatus struct {
	Pid    int    `json:"pid"`
	SHA    string `json:"sha"`
	Kernel string `json:"kernel"`
	BootID string `json:"bootId"`
}

// PortStatus is one remote loopback listener from the cached wire Snapshot.
type PortStatus struct {
	Port int `json:"port"`
}

// ForwardStatus is one active local forward — the verbatim lsof NAME line
// (ground truth; renderers parse it, this layer does not).
type ForwardStatus struct {
	Name string `json:"name"`
}

// ServiceStatus mirrors service.Status: launchd loaded flag + raw state lines.
type ServiceStatus struct {
	Loaded     bool     `json:"loaded"`
	StateLines []string `json:"stateLines"`
}

// MasterStatus reports the transport liveness. Up is the liveness signal; Pid
// is impl-specific ground truth (the system ssh transport fills the
// ControlMaster pid; the native/localexec transports leave it 0 — documented).
// Transport is the active Describe().Impl; Detail is the human liveness string
// (the system ssh transport uses "pid=N"). Transport/Detail are additive and do
// not perturb the byte-compat rendered status on the default (system) path.
type MasterStatus struct {
	Up        bool   `json:"up"`
	Pid       int    `json:"pid"`
	Transport string `json:"transport"`
	Detail    string `json:"detail"`
}

// Health carries daemon-internal liveness/QoS counters (§4.4).
type Health struct {
	LastDisconnectErr  string `json:"lastDisconnectErr,omitempty"`
	DroppedNotifyCount uint64 `json:"droppedNotifyCount"`
	EventsSubscribers  int    `json:"eventsSubscribers"`
	// ReconcileCount is the engine's monotonic completed-pass counter. A client
	// (e.g. `once`) reads it before POST /v1/reconcile, then polls until it
	// advances to know the async, debounced Kick has actually run a pass —
	// Master.Up is already true on the daemon-up branch and says nothing about
	// convergence (§5).
	ReconcileCount uint64 `json:"reconcileCount"`
}

// Status is the full aggregate returned by GET /v1/status (and the first line
// of the events stream). Everything runStatus prints today is derivable from it.
type Status struct {
	Version  VersionInfo     `json:"version"`
	Host     string          `json:"host"`
	Service  ServiceStatus   `json:"service"`
	Master   MasterStatus    `json:"master"`
	Agent    *AgentStatus    `json:"agent"`
	Ports    []PortStatus    `json:"ports"`
	Forwards []ForwardStatus `json:"forwards"`
	Allowed  []int           `json:"allowed"`
	Features map[string]bool `json:"features"`
	Health   Health          `json:"health"`
}

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

// Deps is the dependency set for a Server. The interface fields are narrow so
// tests fake them without constructing an App; Doctor/PushAllow/Kick are
// closures wired by run.go so localapi never imports package app. FeatureNames
// defaults to [clip-image, clip-text, notify, exec] when empty.
type Deps struct {
	Version      VersionInfo
	Host         func() (string, error)
	Agent        AgentSource
	Master       MasterProber
	Ports        ForwardLister
	Service      ServiceStater
	Config       ConfigStore
	Hub          *hub.Hub
	ExecStream   ExecStreamer
	Audit        *audit.Log
	PushAllow    func([]int) error
	Kick         func()
	ReconcileGen func() uint64
	Doctor       func(context.Context) *doctor.Report
	FeatureNames []string
}
