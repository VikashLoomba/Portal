package protocol

import "github.com/fxamacker/cbor/v2"

// Msg is the generic service frame (v4). Payload is the service's own CBOR
// struct — the v3 payload types (OpenURL/ClipRequest/ClipResponse/Notify),
// their tags unchanged — marshalled via MarshalPayload and carried opaquely.
// Seq is per-(service, direction) monotonic within one agent process, stamped
// by the registry for LOG CORRELATION ONLY; it is never the port-event
// staleness Seq (that counter is s.seq and Msg must never advance it).
type Msg struct {
	Service string          `cbor:"svc"`
	Kind    string          `cbor:"k"`
	Seq     uint64          `cbor:"seq,omitempty"`
	Payload cbor.RawMessage `cbor:"p,omitempty"`
}

// Port describes a remote loopback listener. Family is 4 or 6; Addr is
// "127.0.0.1" or "::1" in our use (the agent filters out non-loopback
// listeners before they reach the wire). InodeNS is the kernel socket
// inode — opaque to the client, used only for diagnostics.
type Port struct {
	Port    uint16 `cbor:"port"`
	Family  uint8  `cbor:"fam"`
	Addr    string `cbor:"addr"`
	InodeNS uint32 `cbor:"ns"`
}

// Hello — client → agent. First frame on the connection.
type Hello struct {
	ProtoVersion   uint32 `cbor:"pv"`
	ClientGitSHA   string `cbor:"sha"`
	ClientPID      int    `cbor:"pid"`
	PollIntervalMs uint32 `cbor:"poll_ms"` // 0 = agent default (75)
	WantDestroyMC  bool   `cbor:"destroy_mc"`
	// Services advertises the client's registered handlers as service→version
	// (DESIGN S4 symmetric advertisement). A handler whose service the agent
	// lacks — or whose version differs — stays dormant with one warning.
	Services map[string]uint32 `cbor:"services,omitempty"`
}

// HelloAck — agent → client. Sent after validating Hello.
type HelloAck struct {
	ProtoVersion uint32 `cbor:"pv"`
	AgentGitSHA  string `cbor:"sha"`
	AgentPID     int    `cbor:"pid"`
	Kernel       string `cbor:"kern"`
	BootID       string `cbor:"boot"`
	EphemMin     uint16 `cbor:"emin"`
	EphemMax     uint16 `cbor:"emax"`
	NowUnixNano  int64  `cbor:"now"`
	// Services advertises the agent's registered services as service→version
	// (DESIGN S4 symmetric advertisement). Symmetric with Hello.Services.
	Services map[string]uint32 `cbor:"services,omitempty"`
}

// Subscribe — client → agent. Allow/deny lists. Sent after HelloAck and on
// every allow-file change. ResubscribeID is monotonic per client process;
// the agent ignores any rsid <= last-processed (race-safe on retries).
type Subscribe struct {
	Deny             []uint16 `cbor:"deny"`
	Allow            []uint16 `cbor:"allow"`
	ExcludeEphemeral bool     `cbor:"exc_eph"`
	ResubscribeID    uint64   `cbor:"rsid"`
}

// SubscribeAck — agent → client. Confirms filter swap.
type SubscribeAck struct {
	ResubscribeID uint64 `cbor:"rsid"`
}

// Snapshot — agent → client. Authoritative desired-set as of Seq. Sent
// immediately after every SubscribeAck. Engine treats Snapshot as a RESET.
type Snapshot struct {
	Seq         uint64 `cbor:"seq"`
	GeneratedAt int64  `cbor:"ts"`
	Ports       []Port `cbor:"ports"`
}

// PortAdded — agent → client. Seq strictly > last Snapshot.Seq.
type PortAdded struct {
	Seq  uint64 `cbor:"seq"`
	Port Port   `cbor:"p"`
	At   int64  `cbor:"ts"`
}

// PortRemoved — agent → client. Source: 1 = dump-diff, 2 = destroy-multicast.
type PortRemoved struct {
	Seq    uint64 `cbor:"seq"`
	Port   uint16 `cbor:"port"`
	Family uint8  `cbor:"fam"`
	At     int64  `cbor:"ts"`
	Source uint8  `cbor:"src"`
}

// Heartbeat — agent → client. Sent every 5s when no other agent→client
// frame has gone in that window. The client uses these to detect a hung
// agent (10s without any frame → reconnect). Nonce is echoed from Ping
// so the client can correlate responses to requests.
type Heartbeat struct {
	Seq        uint64 `cbor:"seq"`
	UptimeNano int64  `cbor:"up"`
	Now        int64  `cbor:"now"`
	Nonce      uint64 `cbor:"n,omitempty"` // echoed from Ping when non-zero
}

// Ping — client → agent. Agent responds with Heartbeat echoing the Nonce.
type Ping struct {
	Nonce uint64 `cbor:"n"`
}

// ReqSnap — client → agent. Forces a fresh full Snapshot.
type ReqSnap struct{}

// Shutdown — client → agent. Polite teardown request; agent answers Bye then exits.
type Shutdown struct {
	Reason string `cbor:"reason,omitempty"`
}

// Bye — agent → client. Final frame before agent exits.
type Bye struct {
	Reason string `cbor:"reason,omitempty"`
}

// AgentError — agent → client. Fatal errors carry Fatal=true; the agent
// exits with non-zero immediately after sending such a frame.
type AgentError struct {
	Code  uint16 `cbor:"code"`
	Msg   string `cbor:"msg"`
	Fatal bool   `cbor:"fatal"`
}

// Error codes — exhaustive.
const (
	CodeProtocolMismatch uint16 = 1 // Hello.ProtoVersion != ours
	CodeBadSubscribe     uint16 = 2 // Subscribe before Hello, malformed, etc.
	CodeWatcherFailed    uint16 = 3 // netlink dump returned ErrUnsupported, etc.
	CodeUnauthorized     uint16 = 4 // (reserved for future per-port ACL)
	CodeInternalPanic    uint16 = 5 // backpressure overflow, encoder failure
)

// PortRemovedSource constants.
const (
	SourceDumpDiff     uint8 = 1
	SourceDestroyMulti uint8 = 2
)

// OpenURL — agent → client. Sent when something on the remote box called
// `portald open <url>` (typically via the ~/.local/bin/xdg-open wrapper).
// The Mac client calls `open <url>` to open it in the default browser.
// Only sent while a client is subscribed; silently dropped otherwise.
// As of v4 this travels inside Msg.Payload rather than as an Envelope field.
type OpenURL struct {
	URL string `cbor:"url"`
	Seq uint64 `cbor:"seq"`
}

// ClipRequest — agent → client. A remote shim hit the cmd socket asking the
// Mac to read its clipboard. Sent only while a client is subscribed; the
// agent registers a waiter keyed by Nonce and the Serve loop writes this
// frame interleaved with heartbeats. Nonce+Epoch correlate the response so a
// stale ClipResponse arriving down a *new* pipe after reconnect (where the
// agent's clipSeq reset to 0) is dropped on the epoch check, never
// mis-delivered. These counters are SEPARATE from the port-event Seq — a
// ClipRequest must never advance the agent's port-event staleness counter.
//
// Kind ∈ {"targets","image","text"}; Format is "png" for images, empty otherwise.
// As of v4 this travels inside Msg.Payload rather than as an Envelope field.
type ClipRequest struct {
	Nonce  uint64 `cbor:"n"`
	Epoch  uint64 `cbor:"e"` // agent process identity; echoed back in ClipResponse
	Kind   string `cbor:"kind"`
	Format string `cbor:"fmt,omitempty"`
}

// ClipResponse — client → agent. Answers a ClipRequest by (Nonce,Epoch).
// Image and text bytes are NEVER inline: the bytes cross out-of-band over the
// ControlMaster (clipupload) to a content-addressed side-channel file, and
// this frame carries only the SHA. The agent reconstructs the single legal
// path from the SHA itself (closes the arbitrary-file-read vector), so this
// frame is always a few hundred bytes — the 1 MiB MaxFrameBytes cap is never
// at risk from clipboard content.
//
//	targets: Has indicates the requested content is available on the Mac, and
//	         Kind ∈ {"image","text"} tells the agent WHICH target lines to
//	         advertise. The Mac decides image-vs-text (HasImage first, else
//	         HasText) so the shim greps see the kind actually on the clipboard.
//	image:   OK=true with SHA only (NO path — agent reconstructs it).
//	text:    OK=true with SHA of a side-channel text file (NOT inline).
//
// As of v4 this travels inside Msg.Payload rather than as an Envelope field.
type ClipResponse struct {
	Nonce uint64 `cbor:"n"`
	Epoch uint64 `cbor:"e"`
	OK    bool   `cbor:"ok"`
	Has   bool   `cbor:"has,omitempty"`
	// Kind is the clipboard content kind the Mac decided for a `targets`
	// probe: "image" or "text" (empty for image/text fetches). The agent's
	// writeClipReply maps it to the byte-exact target line(s) the shim's grep
	// expects. Carrying the kind (not the literal target lines) keeps the
	// tool-specific formatting (xclip's UTF8_STRING/TEXT/STRING vs wl-paste's
	// text/plain) on the shim side where the tool identity is known.
	Kind string `cbor:"k,omitempty"`
	SHA  string `cbor:"sha,omitempty"`
	Err  string `cbor:"err,omitempty"`
}

// Notify — agent → client (v3). A remote event (a Claude Code hook firing
// `portald notify --hook`, or a generic `portald notify --title … --body …`)
// is relayed up the pipe and raised as a native macOS notification on the Mac.
// Fire-and-forget: there is NO response frame, mirroring OpenURL. Sent only
// while a client is subscribed; silently dropped otherwise.
//
// Trust model (mirrors cc-clip): an event arriving via the STRUCTURED hook
// entrypoint (a real Claude Code hook) is Verified=true; an event from an
// arbitrary/generic `portald notify` invocation is Verified=false, which the
// Mac renders with an "[unverified] " title prefix. The transport itself (the
// authenticated ssh ControlMaster + the 0600 owner-only cmd socket) is the
// trust boundary — there is no bearer token (DESIGN §7.2 token-equivalence).
//
// Urgency tiers (from the ported ClassifyHookPayload): 0 = completion/calm,
// 1 = attention/idle, 2 = critical/tool-approval. The Mac maps these to an
// optional notification sound when Sound is empty.
//
// As of v4 this travels inside Msg.Payload rather than as an Envelope field.
type Notify struct {
	Title    string `cbor:"title"`
	Body     string `cbor:"body,omitempty"`
	Subtitle string `cbor:"sub,omitempty"`
	Urgency  uint8  `cbor:"urg,omitempty"`
	Verified bool   `cbor:"verified,omitempty"`
	Source   string `cbor:"src,omitempty"`   // e.g. "claude_hook" or "generic"
	Sound    string `cbor:"sound,omitempty"` // macOS sound name; "" = urgency default
	Seq      uint64 `cbor:"seq,omitempty"`
}
