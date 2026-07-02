package discover

import (
	"context"
	"errors"

	"github.com/VikashLoomba/Portal/internal/agentclient"
)

// ErrAgentNotReady is returned by AgentDiscoverer.DesiredPorts before the
// first Snapshot has landed. The reconcile engine treats this the same way
// it treats a discovery error in the bash original: keep current forwards,
// retry next event.
var ErrAgentNotReady = errors.New("discover: agent snapshot not ready")

// AgentDiscoverer satisfies the existing RemoteDiscoverer interface but
// reads from the cached agent Snapshot rather than running ss-over-ssh.
// The deny+allow args drive Subscribe pushes whenever they change.
type AgentDiscoverer struct {
	C *agentclient.Client

	// Last filter we sent, so we can avoid redundant Subscribes.
	lastDeny  []int
	lastAllow []int
	hasSent   bool
}

// NewAgent builds the adapter.
func NewAgent(c *agentclient.Client) *AgentDiscoverer { return &AgentDiscoverer{C: c} }

// DesiredPorts returns the cached Snapshot's port list. If the deny/allow
// args have changed since the last call, it pushes a new Subscribe FIRST
// (the Snapshot reply will land asynchronously and produce a
// KindSnapshotReplaced event that drives the next Reconcile pass).
func (a *AgentDiscoverer) DesiredPorts(ctx context.Context, deny, allow []int) ([]int, error) {
	if a.shouldResubscribe(deny, allow) {
		dU := toU16(deny)
		aU := toU16(allow)
		if err := a.C.Subscribe(dU, aU, true); err != nil {
			return nil, err
		}
		a.lastDeny = append([]int(nil), deny...)
		a.lastAllow = append([]int(nil), allow...)
		a.hasSent = true
	}
	_, ports, ok := a.C.Snapshot()
	if !ok {
		return nil, ErrAgentNotReady
	}
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		out = append(out, int(p))
	}
	return out, nil
}

func (a *AgentDiscoverer) shouldResubscribe(deny, allow []int) bool {
	if !a.hasSent {
		return true
	}
	return !intsEqual(a.lastDeny, deny) || !intsEqual(a.lastAllow, allow)
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func toU16(in []int) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		if v <= 0 || v > 65535 {
			continue
		}
		out = append(out, uint16(v))
	}
	return out
}

var _ RemoteDiscoverer = (*AgentDiscoverer)(nil)
