//! portal wire protocol (v2 Rust port).
//!
//! WIRE-COMPATIBLE with the Go implementation in `pkg/protocol` (ProtoVersion 4):
//! frames are `"PF"` magic + u32 big-endian length + CBOR `Envelope`, where the
//! envelope is a 1-key tagged-union map. Compatibility is enforced by the golden
//! vectors in `docs/vectors/protocol_*.hex` (see `tests/golden_vectors.rs`),
//! which every frame must decode from AND re-encode to byte-exactly.
//!
//! Encoding discipline (must match fxamacker/cbor with `Sort: SortNone`):
//! - struct fields are declared in the SAME ORDER as the Go structs — ciborium
//!   serializes map keys in declaration order;
//! - Go `omitempty` fields are `Option<T>` here and are skipped when `None`;
//!   callers must use `None` (not `Some(0)`/`Some("")`) for absent values;
//! - `[]byte` fields are `serde_bytes::ByteBuf` (CBOR byte strings, NOT arrays);
//! - `cbor.RawMessage` payloads are `ciborium::Value` (spliced inline, order
//!   preserved on re-encode).

pub mod codec;
pub mod envelope;
pub mod messages;

pub use codec::{CodecError, read_frame, write_frame};
pub use envelope::Envelope;

/// ProtoVersion is bumped only on incompatible schema changes. v4 introduced
/// the generic `Msg` service frame plus symmetric capability negotiation in
/// Hello/HelloAck. The Rust rewrite speaks v4 unchanged so a Rust Mac client
/// can drive a deployed Go agent (and vice versa) during rollout.
pub const PROTO_VERSION: u32 = 4;

/// Hard cap on a single frame's CBOR payload. The decoder rejects oversized
/// frames before allocating so a hostile peer cannot OOM us. Clipboard bytes
/// never ride frames (side-channel files, SHA-only in-band), so 1 MiB holds.
pub const MAX_FRAME_BYTES: usize = 1 << 20;

/// Two-byte sentinel preceding every length prefix. On mismatch the reader
/// closes the connection — no in-band recovery; reconnect is fast.
pub const FRAME_MAGIC: [u8; 2] = *b"PF";

/// Agent fatal/protocol error codes — exhaustive, mirrors Go.
pub mod code {
    pub const PROTOCOL_MISMATCH: u16 = 1;
    pub const BAD_SUBSCRIBE: u16 = 2;
    pub const WATCHER_FAILED: u16 = 3;
    pub const UNAUTHORIZED: u16 = 4;
    pub const INTERNAL_PANIC: u16 = 5;
}

/// PortRemoved.source values.
pub mod removed_source {
    pub const DUMP_DIFF: u8 = 1;
    pub const DESTROY_MULTI: u8 = 2;
}
