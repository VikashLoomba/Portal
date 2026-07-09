package api

import "github.com/VikashLoomba/Portal/pkg/hub"

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

// Event is the typed envelope for one ndjson line of GET /v1/events. Exactly
// one shape is populated per Type: snapshot/state carry Status; notify carries
// Notify; tick carries neither.
type Event struct {
	Type   string      `json:"type"`
	Status *Status     `json:"status,omitempty"`
	Notify *hub.Notify `json:"notify,omitempty"`
}
