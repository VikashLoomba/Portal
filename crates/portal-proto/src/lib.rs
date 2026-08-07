//! portal wire protocol.
//!
//! The frame format is FROZEN at ProtoVersion 4: `"PF"` magic + u32 big-endian
//! length + CBOR `Envelope`, where the envelope is a 1-key tagged-union map.
//! The format is not ours to change unilaterally — agents already deployed on
//! boxes speak it. The golden vectors in `docs/vectors/protocol_*.hex` (see
//! `tests/golden_vectors.rs`) are the authority: every frame must decode from
//! AND re-encode to them byte-exactly.
//!
//! Encoding discipline the vectors pin:
//! - map keys are emitted in struct declaration order, so field ORDER is part
//!   of the contract — reordering a struct breaks the wire;
//! - optional fields are `Option<T>` and skipped when `None`; callers must use
//!   `None` (not `Some(0)`/`Some("")`) for absent values, since an explicitly
//!   zero-valued key encodes differently than an absent one;
//! - byte-string fields are `serde_bytes::ByteBuf` (CBOR byte strings, NOT
//!   arrays);
//! - opaque nested payloads are `ciborium::Value` (spliced inline, order
//!   preserved on re-encode).

pub mod codec;
pub mod envelope;
pub mod messages;

pub use codec::{CodecError, read_frame, write_frame};
pub use envelope::Envelope;

/// ProtoVersion is bumped only on incompatible schema changes. v4 introduced
/// the generic `Msg` service frame plus symmetric capability negotiation in
/// Hello/HelloAck.
pub const PROTO_VERSION: u32 = 4;

/// Hard cap on a single frame's CBOR payload. The decoder rejects oversized
/// frames before allocating so a hostile peer cannot OOM us. Clipboard bytes
/// never ride frames (side-channel files, SHA-only in-band), so 1 MiB holds.
pub const MAX_FRAME_BYTES: usize = 1 << 20;

/// Two-byte sentinel preceding every length prefix. On mismatch the reader
/// closes the connection — no in-band recovery; reconnect is fast.
pub const FRAME_MAGIC: [u8; 2] = *b"PF";

/// Agent fatal/protocol error codes — exhaustive; the numeric values are wire
/// contract.
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
