// Package discover defines the contract for "what ports does the remote
// want forwarded?". The production implementation is AgentDiscoverer
// (agent.go), which reads from a cached agentclient Snapshot driven by
// push events from the remote portald agent.
//
// Historically a `discover.SS` struct ran an embedded ss/awk script over
// SSH every 10s. That implementation was removed in the split-daemon
// migration — the agent watches listen sockets via NETLINK_SOCK_DIAG and
// pushes events with sub-100ms latency, so polling is no longer needed.
package discover

import "context"

// RemoteDiscoverer returns the desired forward-set: which loopback dev
// ports are listening remotely after deny/ephemeral exclusions and
// allowlist overrides. Implementations may push state changes
// asynchronously; the engine reconciles whenever an event arrives.
type RemoteDiscoverer interface {
	DesiredPorts(ctx context.Context, deny, allow []int) ([]int, error)
}
