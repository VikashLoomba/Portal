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
	// KindOpenURL fires when the agent receives a request to open a URL
	// on the client (e.g. via the xdg-open wrapper on the remote box).
	KindOpenURL
	// KindClipRequest fires when a remote shim asks the Mac to read its
	// clipboard (via the cmd socket → ClipRequest frame). UNLIKE the other
	// kinds this is NOT routed through the shared, drop-on-full events
	// channel — runClipHandler reads it from a dedicated channel so a burst
	// of port events can never evict a pending paste (DESIGN §5). It is
	// declared here only so the kind value is reserved and the event shape is
	// shared; the demuxer sends it on ClipEvents(), not Events().
	KindClipRequest
	// KindNotify fires when the agent relays a notification (a Claude Code hook
	// or a generic `portald notify`) up the pipe. LIKE KindClipRequest it is
	// routed on a DEDICATED channel (NotifyEvents()), not the shared drop-on-full
	// events channel, so a port-event burst can't evict a pending notification.
	// runNotifyHandler on the Mac drains it and raises a native notification.
	KindNotify
	// KindCredRequest fires when a remote command asks the Mac to approve and
	// supply a credential. Like clip and notify, it uses its own dedicated
	// channel so shared engine traffic cannot evict a pending human prompt.
	// runCredHandler answers it through Client.SendCredResponse.
	KindCredRequest
)

// EngineEvent is the unit of communication from agentclient → engine.
type EngineEvent struct {
	Kind    EngineEventKind
	Err     error    // populated on KindDisconnected
	Added   []uint16 // populated on KindDelta
	Removed []uint16 // populated on KindDelta
	URL     string   // populated on KindOpenURL
	// Clip carries the fields of a KindClipRequest event. nil otherwise. The
	// handler answers by calling Client.SendClipResponse with the echoed
	// Nonce/Epoch (DESIGN §4.4).
	Clip *ClipEvent
	// Notify carries the fields of a KindNotify event. nil otherwise. Notify is
	// fire-and-forget (no response frame), so the handler just raises the
	// native notification.
	Notify *NotifyEvent
	// Cred carries the fields of a KindCredRequest event. nil otherwise. The
	// handler answers by calling Client.SendCredResponse with the echoed
	// Nonce/Epoch.
	Cred *CredEvent
}

// NotifyEvent is the payload of a KindNotify. It mirrors protocol.Notify; the
// Mac's runNotifyHandler raises a native notification from it, prefixing
// "[unverified] " on the title when Verified is false.
type NotifyEvent struct {
	Title    string
	Body     string
	Subtitle string
	Urgency  uint8
	Verified bool
	Source   string
	Sound    string
	Seq      uint64
}

// ClipEvent is the payload of a KindClipRequest. Nonce/Epoch are echoed back
// verbatim in the ClipResponse so the agent can correlate it; Kind is one of
// "targets"|"image"|"text" and Format is "png" for images, "" otherwise.
type ClipEvent struct {
	Nonce  uint64
	Epoch  uint64
	Kind   string
	Format string
}

// CredEvent is the payload of a KindCredRequest. Nonce/Epoch are echoed in the
// CredResponse; Label identifies the credential, Requester identifies the box
// process, and Mode/Target describe its delivery destination.
type CredEvent struct {
	Nonce     uint64
	Epoch     uint64
	Label     string
	Requester string
	Mode      string
	Target    string
}
