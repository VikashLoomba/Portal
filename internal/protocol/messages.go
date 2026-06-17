package protocol

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
	PollIntervalMs uint32 `cbor:"poll_ms"`   // 0 = agent default (75)
	WantDestroyMC  bool   `cbor:"destroy_mc"`
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
type OpenURL struct {
	URL string `cbor:"url"`
	Seq uint64 `cbor:"seq"`
}
