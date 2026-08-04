//! Mac clipboard access (port of `internal/clip`, redesigned).
//!
//! v1 shelled out to `osascript` for every probe/read: AppleScript coercion
//! through a temp file, a 5s hard cap, and a fail-closed concealment probe.
//! That stack is WHY large-image paste breaks today (the 5s coercion cliff
//! inside an 8s coerce+upload budget, feeding an 8 MiB upload cap).
//!
//! v2 reads NSPasteboard IN-PROCESS (objc2-app-kit):
//! - `has_image`/`has_text` inspect pasteboard types without pulling bytes;
//! - `image_png` reads image data and converts to PNG natively (no temp
//!   file, no AppleScript, milliseconds instead of seconds);
//! - `is_concealed` sees `org.nspasteboard.ConcealedType` directly on the
//!   type list — no fragile ObjC-bridge script, still fail-closed on error;
//! - size limits move to the TRANSPORT layer (streamed side-channel upload
//!   with a size-aware budget), not the read layer.

#[cfg(target_os = "macos")]
pub mod macos;
pub mod mock;
pub mod watcher;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ClipKind {
    Image,
    Text,
}

impl ClipKind {
    /// Wire name used by clipsync manifests/frames ("text" | "image").
    pub fn wire_name(self) -> &'static str {
        match self {
            ClipKind::Text => "text",
            ClipKind::Image => "image",
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ClipError {
    #[error("clipboard backend unavailable: {0}")]
    Unavailable(&'static str),
    #[error("clipboard is empty (no {0:?} content)")]
    Empty(ClipKind),
    #[error("clipboard content too large: {size} bytes (max {max})")]
    TooLarge { size: usize, max: usize },
    #[error("clipboard backend: {0}")]
    Backend(String),
}

/// The read surface the clip service handler consumes. All methods are
/// synchronous; the daemon calls them from a blocking task (pasteboard access
/// must not stall the async runtime).
pub trait Clipboard: Send + Sync {
    /// Cheap type-list probe — never pulls bytes.
    fn has_image(&self) -> bool;
    /// Cheap type-list probe — never pulls bytes.
    fn has_text(&self) -> bool;
    /// True when the clipboard owner marked the contents secret/transient
    /// (org.nspasteboard.ConcealedType / TransientType). FAIL-CLOSED: any
    /// probe error must return true — text serving is a standing pull
    /// endpoint and must never auto-exfiltrate a password manager's copy.
    fn is_concealed(&self) -> bool;
    /// Clipboard image as PNG bytes (converting from TIFF/JPEG/… natively).
    fn image_png(&self) -> Result<Vec<u8>, ClipError>;
    /// Clipboard text as UTF-8.
    fn text(&self) -> Result<String, ClipError>;
}

/// The write surface (remote → Mac pasteboard), gated by the clip-write
/// capability at the call site.
pub trait ClipboardWriter: Send + Sync {
    fn write_text(&self, text: &str) -> Result<(), ClipError>;
    fn write_image_png(&self, png: &[u8]) -> Result<(), ClipError>;
    fn clear(&self) -> Result<(), ClipError>;
}
