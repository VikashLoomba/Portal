// Package protocol is the shared wire format spoken by the Mac client
// (`portal`) and the Linux agent (`portald`) over a single ssh-multiplexed
// exec pipe. Frames are length-and-magic-prefixed CBOR (see codec.go); the
// payload is a one-key Envelope tagged-union (exactly one field is non-nil
// per frame). See messages.go for every wire type.
package protocol

// ProtoVersion is bumped only on incompatible schema changes. Both binaries
// ship from one tree, so a mismatch can only mean a stale agent upload — the
// bootstrap layer detects this and re-uploads.
//
// v2 added the clipboard-read request/response pair (ClipRequest /
// ClipResponse). The fields are additive — an old decoder ignores the unknown
// CBOR keys — so the bump is not for wire compatibility but for *honest*
// version negotiation: if a re-upload were ever blocked (e.g. read-only
// ~/.cache), a loud mismatch beats a silent clip-feature no-op.
//
// v3 added the notification relay (Notify, agent → client). Same honest-
// negotiation rationale: a remote hook firing `portald notify` into a stale
// agent that predates the Notify capability must surface as a loud version
// mismatch (triggering re-upload) rather than a silently dropped notification.
const ProtoVersion uint32 = 3

// MaxFrameBytes is the hard cap on a single frame's payload size. Decoder
// rejects oversized frames before allocating, so a hostile peer can't OOM us.
const MaxFrameBytes = 1 << 20 // 1 MiB

// FrameMagic precedes every length prefix. Two-byte sentinel; on mismatch
// the reader closes the connection (no in-band recovery — reconnect is fast).
var FrameMagic = [2]byte{'P', 'F'}

// Envelope is a 1-key CBOR map. Exactly one field is non-nil per frame —
// the receiver looks up the populated pointer to dispatch.
type Envelope struct {
	// client → agent
	Hello     *Hello     `cbor:"hello,omitempty"`
	Subscribe *Subscribe `cbor:"subscribe,omitempty"`
	Ping      *Ping      `cbor:"ping,omitempty"`
	ReqSnap   *ReqSnap   `cbor:"req_snap,omitempty"`
	Shutdown  *Shutdown  `cbor:"shutdown,omitempty"`

	// agent → client
	HelloAck     *HelloAck     `cbor:"hello_ack,omitempty"`
	SubscribeAck *SubscribeAck `cbor:"subscribe_ack,omitempty"`
	Snapshot     *Snapshot     `cbor:"snapshot,omitempty"`
	PortAdded    *PortAdded    `cbor:"port_added,omitempty"`
	PortRemoved  *PortRemoved  `cbor:"port_removed,omitempty"`
	Heartbeat    *Heartbeat    `cbor:"heartbeat,omitempty"`
	AgentError   *AgentError   `cbor:"agent_error,omitempty"`
	Bye          *Bye          `cbor:"bye,omitempty"`
	OpenURL      *OpenURL      `cbor:"open_url,omitempty"`

	// clipboard-read (v2): request flows agent → client, response client → agent.
	ClipRequest  *ClipRequest  `cbor:"clip_req,omitempty"`  // agent → client
	ClipResponse *ClipResponse `cbor:"clip_resp,omitempty"` // client → agent

	// notification relay (v3): fire-and-forget agent → client. A remote event
	// (a Claude Code hook, or a generic `portald notify`) is relayed up the pipe
	// and raised as a native macOS notification on the Mac. No response frame.
	Notify *Notify `cbor:"notify,omitempty"` // agent → client
}
