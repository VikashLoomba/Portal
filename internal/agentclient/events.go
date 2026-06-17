// Package agentclient is the Mac-side counterpart of cmd/portald. It manages
// the long-lived ssh-exec pipe to the agent, performs the handshake, and
// emits high-level EngineEvents to the reconcile loop.
package agentclient

// EngineEventKind discriminates the lifecycle events the engine cares about.
type EngineEventKind uint8

const (
	// KindConnected fires once per successful (re)connect, after Hello+
	// Subscribe+initial Snapshot have all been processed. The engine can
	// safely call Reconcile.
	KindConnected EngineEventKind = 1 + iota
	// KindDisconnected fires when the agent stream errors or EOFs. The
	// engine must NOT cancel forwards (matches old "transient blip"
	// semantics); it just keeps waiting for KindConnected.
	KindDisconnected
	// KindSnapshotReplaced fires when a fresh Snapshot supersedes the
	// previous desired-set (new Subscribe, ReqSnap, etc.). The engine
	// reconciles to the new set.
	KindSnapshotReplaced
	// KindDelta fires after a coalesced burst of PortAdded/PortRemoved
	// events (50ms debounce). The engine reconciles.
	KindDelta
)

// EngineEvent is the unit of communication from agentclient → engine.
type EngineEvent struct {
	Kind    EngineEventKind
	Err     error    // populated on KindDisconnected
	Added   []uint16 // populated on KindDelta
	Removed []uint16 // populated on KindDelta
}
