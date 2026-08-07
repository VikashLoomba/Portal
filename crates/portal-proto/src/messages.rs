//! Wire message types. Field ORDER and serde renames are wire contract —
//! ciborium emits map keys in declaration order and the golden-vector tests
//! assert byte-exact re-encoding. Do not reorder fields.

use std::collections::BTreeMap;

use ciborium::Value;
use serde::{Deserialize, Serialize};
use serde_bytes::ByteBuf;

/// Generic service frame (v4). `payload` is the service's own CBOR struct
/// (ClipRequest/ClipResponse/Notify/CredRequest/... below) carried opaquely
/// and spliced inline as an opaque `ciborium::Value`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Msg {
    #[serde(rename = "svc")]
    pub service: String,
    #[serde(rename = "k")]
    pub kind: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub seq: Option<u64>,
    #[serde(rename = "p", skip_serializing_if = "Option::is_none")]
    pub payload: Option<Value>,
}

/// Marshal a typed service payload into the `Msg.payload` slot.
pub fn marshal_payload<T: Serialize>(v: &T) -> Result<Value, ciborium::value::Error> {
    Value::serialized(v)
}

/// Unmarshal a `Msg.payload` into a typed service payload.
pub fn unmarshal_payload<'a, T: Deserialize<'a>>(v: &Value) -> Result<T, ciborium::value::Error> {
    v.deserialized()
}

/// A remote loopback listener. `family` is 4 or 6; `inode_ns` is the kernel
/// socket inode (diagnostics only).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Port {
    pub port: u16,
    #[serde(rename = "fam")]
    pub family: u8,
    pub addr: String,
    #[serde(rename = "ns")]
    pub inode_ns: u32,
}

/// Hello — client → agent. First frame on the connection.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Hello {
    #[serde(rename = "pv")]
    pub proto_version: u32,
    #[serde(rename = "sha")]
    pub client_git_sha: String,
    #[serde(rename = "pid")]
    pub client_pid: i64,
    #[serde(rename = "poll_ms")]
    pub poll_interval_ms: u32,
    #[serde(rename = "destroy_mc")]
    pub want_destroy_mc: bool,
    /// service → version advertisement (symmetric with HelloAck).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub services: Option<BTreeMap<String, u32>>,
    /// v2-ADDITIVE: the Mac-side box name for this connection, so agent logs
    /// are box-attributed on multi-box/multi-Mac setups. Old decoders ignore
    /// the unknown key; `None` encodes nothing (golden vectors unaffected).
    #[serde(rename = "box", skip_serializing_if = "Option::is_none")]
    pub box_name: Option<String>,
}

/// HelloAck — agent → client. Sent after validating Hello.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct HelloAck {
    #[serde(rename = "pv")]
    pub proto_version: u32,
    #[serde(rename = "sha")]
    pub agent_git_sha: String,
    #[serde(rename = "pid")]
    pub agent_pid: i64,
    #[serde(rename = "kern")]
    pub kernel: String,
    #[serde(rename = "boot")]
    pub boot_id: String,
    #[serde(rename = "emin")]
    pub ephem_min: u16,
    #[serde(rename = "emax")]
    pub ephem_max: u16,
    #[serde(rename = "now")]
    pub now_unix_nano: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub services: Option<BTreeMap<String, u32>>,
}

/// Subscribe — client → agent. Allow/deny filter; re-sent on allow changes.
/// `resubscribe_id` is monotonic per client; the agent ignores stale ids.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Subscribe {
    pub deny: Vec<u16>,
    pub allow: Vec<u16>,
    #[serde(rename = "exc_eph")]
    pub exclude_ephemeral: bool,
    #[serde(rename = "rsid")]
    pub resubscribe_id: u64,
}

/// SubscribeAck — agent → client. Confirms filter swap.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SubscribeAck {
    #[serde(rename = "rsid")]
    pub resubscribe_id: u64,
}

/// Snapshot — agent → client. Authoritative desired-set as of `seq`; the
/// engine treats it as a RESET.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Snapshot {
    pub seq: u64,
    #[serde(rename = "ts")]
    pub generated_at: i64,
    pub ports: Vec<Port>,
}

/// PortAdded — agent → client. `seq` strictly > last Snapshot.seq.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PortAdded {
    pub seq: u64,
    #[serde(rename = "p")]
    pub port: Port,
    #[serde(rename = "ts")]
    pub at: i64,
}

/// PortRemoved — agent → client. `source`: see [`crate::removed_source`].
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PortRemoved {
    pub seq: u64,
    pub port: u16,
    #[serde(rename = "fam")]
    pub family: u8,
    #[serde(rename = "ts")]
    pub at: i64,
    #[serde(rename = "src")]
    pub source: u8,
}

/// Heartbeat — agent → client, every 5s of silence. `nonce` echoes Ping.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Heartbeat {
    pub seq: u64,
    #[serde(rename = "up")]
    pub uptime_nano: i64,
    pub now: i64,
    #[serde(rename = "n", skip_serializing_if = "Option::is_none")]
    pub nonce: Option<u64>,
}

/// Ping — client → agent. Agent responds with Heartbeat echoing the nonce.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Ping {
    #[serde(rename = "n")]
    pub nonce: u64,
}

/// ReqSnap — client → agent. Forces a fresh full Snapshot. Encodes as `{}`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReqSnap {}

/// Shutdown — client → agent. Agent answers Bye then exits.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Shutdown {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

/// Bye — agent → client. Final frame before agent exit.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Bye {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

/// AgentError — agent → client. `fatal` errors are followed by process exit.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AgentError {
    pub code: u16,
    pub msg: String,
    pub fatal: bool,
}

// ---------------------------------------------------------------------------
// Service payloads (ride Msg.payload; never dedicated Envelope fields in v4).
// ---------------------------------------------------------------------------

/// OpenURL — agent → client (service "openurl"). Relayed `portald open <url>`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct OpenUrl {
    pub url: String,
    pub seq: u64,
}

/// ClipRequest — agent → client (service "clip", kind "req"). A remote shim
/// asked the Mac to read its clipboard. `kind` ∈ {"targets","image","text"};
/// `format` is "png" for images. Nonce+epoch correlate the response across
/// reconnects.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipRequest {
    #[serde(rename = "n")]
    pub nonce: u64,
    #[serde(rename = "e")]
    pub epoch: u64,
    pub kind: String,
    #[serde(rename = "fmt", skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
}

/// ClipResponse — client → agent. Image/text bytes are NEVER inline: they
/// cross out-of-band to a content-addressed side-channel file and this frame
/// carries only the SHA (the agent reconstructs the single legal path).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipResponse {
    #[serde(rename = "n")]
    pub nonce: u64,
    #[serde(rename = "e")]
    pub epoch: u64,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub has: Option<bool>,
    #[serde(rename = "k", skip_serializing_if = "Option::is_none")]
    pub kind: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sha: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub err: Option<String>,
}

/// CredRequest — agent → client (service "cred", kind "req").
/// `mode` ∈ {"env","stdin","askpass"}.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct CredRequest {
    #[serde(rename = "n")]
    pub nonce: u64,
    #[serde(rename = "e")]
    pub epoch: u64,
    #[serde(rename = "l")]
    pub label: String,
    #[serde(rename = "r", skip_serializing_if = "Option::is_none")]
    pub requester: Option<String>,
    #[serde(rename = "m")]
    pub mode: String,
    #[serde(rename = "t", skip_serializing_if = "Option::is_none")]
    pub target: Option<String>,
}

/// CredResponse — client → agent. The secret is deliberately in-band (small,
/// must never touch the box's disk); CBOR byte string, ≤ 4096 bytes.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct CredResponse {
    #[serde(rename = "n")]
    pub nonce: u64,
    #[serde(rename = "e")]
    pub epoch: u64,
    pub ok: bool,
    #[serde(rename = "s", skip_serializing_if = "Option::is_none")]
    pub secret: Option<ByteBuf>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub err: Option<String>,
}

/// ClipWriteRequest — agent → client (service "clipwrite", kind "req").
/// `kind` ∈ {"text","image","clear"}; bytes sit on the box side channel.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipWriteRequest {
    #[serde(rename = "n")]
    pub nonce: u64,
    #[serde(rename = "e")]
    pub epoch: u64,
    pub kind: String,
    #[serde(rename = "fmt", skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sha: Option<String>,
    #[serde(rename = "sz", skip_serializing_if = "Option::is_none")]
    pub size: Option<i64>,
}

/// ClipWriteResponse — client → agent (service "clipwrite", kind "resp").
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipWriteResponse {
    #[serde(rename = "n")]
    pub nonce: u64,
    #[serde(rename = "e")]
    pub epoch: u64,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub err: Option<String>,
}

/// ClipSyncUpdate — client → agent (service "clipsync", kind "update").
/// The Mac clipboard changed; DESIGN-clipsync.md. Text ≤ [`CLIPSYNC_INLINE_MAX`]
/// rides inline; larger content is content-addressed (`sha`+`size`) with the
/// bytes streamed on a dedicated blob channel — never inside CBOR frames.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipSyncUpdate {
    #[serde(rename = "cid")]
    pub change_id: u64,
    /// "text" | "image".
    #[serde(rename = "k")]
    pub kind: String,
    /// "png" for images; absent for text.
    #[serde(rename = "fmt", skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sha: Option<String>,
    #[serde(rename = "sz", skip_serializing_if = "Option::is_none")]
    pub size: Option<i64>,
    /// Inline bytes for small text (≤ CLIPSYNC_INLINE_MAX).
    #[serde(rename = "b", skip_serializing_if = "Option::is_none")]
    pub inline: Option<ByteBuf>,
}

/// Largest inline clipsync payload; everything bigger is a blob transfer.
/// Comfortably under MAX_FRAME_BYTES (1 MiB).
pub const CLIPSYNC_INLINE_MAX: usize = 256 << 10;

/// ClipSyncClear — client → agent (service "clipsync", kind "clear").
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipSyncClear {
    #[serde(rename = "cid")]
    pub change_id: u64,
}

/// ClipSyncAck — agent → client (service "clipsync", kind "ack").
/// `have_blob=false` asks the Mac to stream the blob (then re-send update);
/// acks make per-box sync state observable in `portal status`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClipSyncAck {
    #[serde(rename = "cid")]
    pub change_id: u64,
    #[serde(rename = "hb")]
    pub have_blob: bool,
}

/// Notify — agent → client (service "notify"). Fire-and-forget; `urgency`
/// 0 = completion, 1 = attention, 2 = critical. `verified` distinguishes the
/// structured Claude Code hook path from generic `portald notify`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Notify {
    pub title: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub body: Option<String>,
    #[serde(rename = "sub", skip_serializing_if = "Option::is_none")]
    pub subtitle: Option<String>,
    #[serde(rename = "urg", skip_serializing_if = "Option::is_none")]
    pub urgency: Option<u8>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub verified: Option<bool>,
    #[serde(rename = "src", skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sound: Option<String>,
}
