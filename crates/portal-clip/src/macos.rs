//! Native NSPasteboard backend (objc2 — in-process, no osascript). This is
//! what fixes v1's broken image paste at the source: reads take microseconds
//! instead of a 5s AppleScript coercion cliff, and concealment markers are
//! read directly off the type list.
//!
//! Threading: NSPasteboard is documented main-thread-agnostic for these
//! operations (unlike most of AppKit); the daemon still calls this from one
//! dedicated blocking task for ordering, per the trait contract.

use objc2::rc::Retained;
use objc2::runtime::ProtocolObject;
use objc2_app_kit::{
    NSBitmapImageFileType, NSBitmapImageRep, NSPasteboard, NSPasteboardType, NSPasteboardTypePNG,
    NSPasteboardTypeString, NSPasteboardTypeTIFF,
};
use objc2_foundation::{NSArray, NSData, NSDictionary, NSString};

use crate::{ClipError, ClipKind, Clipboard, ClipboardWriter};

/// NSPasteboard types that signal "do not persist/exfiltrate" (nspasteboard.org
/// conventions honored by 1Password/Bitwarden/KeePassXC/etc.).
const CONCEALED_TYPES: [&str; 2] = [
    "org.nspasteboard.ConcealedType",
    "org.nspasteboard.TransientType",
];

/// Raster types beyond PNG we can convert via NSBitmapImageRep. TIFF is the
/// universal interchange type — screenshots, Preview, and most apps offer it.
const CONVERTIBLE_IMAGE_TYPES: [&str; 3] = ["public.tiff", "public.jpeg", "public.heic"];

/// A changeCount-consistent view of the pasteboard, for the clipsync watcher:
/// classify + read happen against ONE generation or not at all (the v1 TTL
/// cache existed to paper over exactly this race).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Snapshot {
    /// Nothing servable (empty, unknown-only types, or cleared).
    Empty { change_count: i64 },
    /// Concealed/transient — never published (fail-closed).
    Concealed { change_count: i64 },
    Content {
        change_count: i64,
        kind: ClipKind,
        /// UTF-8 text or PNG bytes (converted in-process when needed).
        data: Vec<u8>,
    },
}

impl Snapshot {
    pub fn change_count(&self) -> i64 {
        match self {
            Snapshot::Empty { change_count }
            | Snapshot::Concealed { change_count }
            | Snapshot::Content { change_count, .. } => *change_count,
        }
    }
}

#[derive(Debug, Default)]
pub struct NativePasteboard;

impl NativePasteboard {
    pub fn new() -> Self {
        Self
    }

    fn pasteboard(&self) -> Retained<NSPasteboard> {
        NSPasteboard::generalPasteboard()
    }

    /// The current pasteboard generation counter (cheap: one int read).
    /// The watcher polls this and takes a [`snapshot`](Self::snapshot) only
    /// when it moves.
    pub fn change_count(&self) -> i64 {
        self.pasteboard().changeCount() as i64
    }

    fn type_names(&self, pb: &NSPasteboard) -> Vec<String> {
        let Some(types) = pb.types() else {
            return Vec::new();
        };
        types.iter().map(|t| t.to_string()).collect()
    }

    /// Read one consistent generation: retry while changeCount moves under
    /// us (a copy racing the read), giving up after a few laps rather than
    /// serving torn content.
    pub fn snapshot(&self) -> Result<Snapshot, ClipError> {
        let pb = self.pasteboard();
        for _ in 0..4 {
            let before = pb.changeCount() as i64;
            let snap = self.snapshot_at(&pb, before);
            let after = pb.changeCount() as i64;
            if before == after {
                return snap;
            }
        }
        Err(ClipError::Backend(
            "pasteboard kept changing during read".into(),
        ))
    }

    fn snapshot_at(&self, pb: &NSPasteboard, change_count: i64) -> Result<Snapshot, ClipError> {
        let names = self.type_names(pb);
        if names.is_empty() {
            return Ok(Snapshot::Empty { change_count });
        }
        if names.iter().any(|n| CONCEALED_TYPES.contains(&n.as_str())) {
            return Ok(Snapshot::Concealed { change_count });
        }
        // Image wins over text (v1 semantics: a screenshot paste is an image
        // even though Finder also offers a text file path).
        if let Some(png) = self.read_image_png(pb, &names)? {
            return Ok(Snapshot::Content {
                change_count,
                kind: ClipKind::Image,
                data: png,
            });
        }
        if let Some(text) = self.read_text(pb)
            && !text.is_empty()
        {
            return Ok(Snapshot::Content {
                change_count,
                kind: ClipKind::Text,
                data: text.into_bytes(),
            });
        }
        Ok(Snapshot::Empty { change_count })
    }

    fn read_text(&self, pb: &NSPasteboard) -> Option<String> {
        let data = unsafe { pb.dataForType(NSPasteboardTypeString) }?;
        Some(String::from_utf8_lossy(&data.to_vec()).into_owned())
    }

    /// PNG directly if offered; else any convertible raster type through
    /// NSBitmapImageRep → PNG. In-process, no temp files.
    fn read_image_png(
        &self,
        pb: &NSPasteboard,
        names: &[String],
    ) -> Result<Option<Vec<u8>>, ClipError> {
        if names.iter().any(|n| n == "public.png")
            && let Some(data) = unsafe { pb.dataForType(NSPasteboardTypePNG) }
        {
            return Ok(Some(data.to_vec()));
        }
        let convertible = names
            .iter()
            .find(|n| CONVERTIBLE_IMAGE_TYPES.contains(&n.as_str()));
        let Some(type_name) = convertible else {
            return Ok(None);
        };
        let ns_type = NSString::from_str(type_name);
        let Some(raw) = pb.dataForType(&ns_type) else {
            return Ok(None);
        };
        let Some(rep) = NSBitmapImageRep::imageRepWithData(&NSData::with_bytes(&raw.to_vec()))
        else {
            return Err(ClipError::Backend(format!(
                "could not decode {type_name} clipboard image"
            )));
        };
        let png = unsafe {
            rep.representationUsingType_properties(NSBitmapImageFileType::PNG, &NSDictionary::new())
        }
        .ok_or_else(|| ClipError::Backend("PNG encode failed".into()))?;
        Ok(Some(png.to_vec()))
    }
}

impl Clipboard for NativePasteboard {
    fn has_image(&self) -> bool {
        matches!(
            self.snapshot(),
            Ok(Snapshot::Content {
                kind: ClipKind::Image,
                ..
            })
        )
    }

    fn has_text(&self) -> bool {
        matches!(
            self.snapshot(),
            Ok(Snapshot::Content {
                kind: ClipKind::Text,
                ..
            })
        )
    }

    fn is_concealed(&self) -> bool {
        // Fail CLOSED: any error is treated as concealed.
        !matches!(
            self.snapshot(),
            Ok(Snapshot::Empty { .. } | Snapshot::Content { .. })
        )
    }

    fn image_png(&self) -> Result<Vec<u8>, ClipError> {
        match self.snapshot()? {
            Snapshot::Content {
                kind: ClipKind::Image,
                data,
                ..
            } => Ok(data),
            Snapshot::Concealed { .. } => Err(ClipError::Unavailable("clipboard is concealed")),
            _ => Err(ClipError::Empty(ClipKind::Image)),
        }
    }

    fn text(&self) -> Result<String, ClipError> {
        match self.snapshot()? {
            Snapshot::Content {
                kind: ClipKind::Text,
                data,
                ..
            } => Ok(String::from_utf8_lossy(&data).into_owned()),
            Snapshot::Concealed { .. } => Err(ClipError::Unavailable("clipboard is concealed")),
            _ => Err(ClipError::Empty(ClipKind::Text)),
        }
    }
}

impl ClipboardWriter for NativePasteboard {
    fn write_text(&self, text: &str) -> Result<(), ClipError> {
        let pb = self.pasteboard();
        pb.clearContents();
        let types: Retained<NSArray<NSPasteboardType>> =
            NSArray::from_slice(&[unsafe { NSPasteboardTypeString }]);
        unsafe { pb.declareTypes_owner(&types, None) };
        let data = NSData::with_bytes(text.as_bytes());
        if unsafe { pb.setData_forType(Some(&data), NSPasteboardTypeString) } {
            Ok(())
        } else {
            Err(ClipError::Backend("pasteboard text write failed".into()))
        }
    }

    fn write_image_png(&self, png: &[u8]) -> Result<(), ClipError> {
        let pb = self.pasteboard();
        pb.clearContents();
        // Offer PNG and TIFF: PNG-only confuses several AppKit consumers.
        let rep = NSBitmapImageRep::imageRepWithData(&NSData::with_bytes(png))
            .ok_or_else(|| ClipError::Backend("invalid PNG".into()))?;
        let tiff = rep.TIFFRepresentation();
        let types: Retained<NSArray<NSPasteboardType>> =
            NSArray::from_slice(&[unsafe { NSPasteboardTypePNG }, unsafe {
                NSPasteboardTypeTIFF
            }]);
        unsafe { pb.declareTypes_owner(&types, None) };
        let ok_png =
            unsafe { pb.setData_forType(Some(&NSData::with_bytes(png)), NSPasteboardTypePNG) };
        if let Some(tiff) = tiff {
            let _ = unsafe { pb.setData_forType(Some(&tiff), NSPasteboardTypeTIFF) };
        }
        if ok_png {
            Ok(())
        } else {
            Err(ClipError::Backend("pasteboard image write failed".into()))
        }
    }

    fn clear(&self) -> Result<(), ClipError> {
        self.pasteboard().clearContents();
        Ok(())
    }
}

// Silence the unused-import lint for ProtocolObject on builds where the
// declareTypes path is elided.
#[allow(unused)]
fn _keep(_: Option<&ProtocolObject<dyn objc2::runtime::NSObjectProtocol>>) {}

/// Interactive verification (touches the REAL user pasteboard — ignored by
/// default; run with `cargo test -p portal-clip -- --ignored` on a Mac
/// where clobbering the clipboard is acceptable).
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    #[ignore = "mutates the real user pasteboard"]
    fn roundtrip_text_and_image_on_real_pasteboard() {
        let pb = NativePasteboard::new();
        let before = pb.change_count();
        pb.write_text("portal-clip roundtrip test").unwrap();
        assert!(pb.change_count() > before);
        assert_eq!(pb.text().unwrap(), "portal-clip roundtrip test");
        match pb.snapshot().unwrap() {
            Snapshot::Content { kind, data, .. } => {
                assert_eq!(kind, ClipKind::Text);
                assert_eq!(data, b"portal-clip roundtrip test");
            }
            other => panic!("expected text content, got {other:?}"),
        }

        // 1x1 red PNG.
        let png: &[u8] = &[
            0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48,
            0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00,
            0x00, 0x90, 0x77, 0x53, 0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41, 0x54, 0x08,
            0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, 0x00, 0x03, 0x00, 0x01, 0x9E, 0x91, 0x0A,
            0x2F, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
        ];
        pb.write_image_png(png).unwrap();
        let out = pb.image_png().unwrap();
        assert_eq!(
            &out[..8],
            &[0x89, b'P', b'N', b'G', b'\r', b'\n', 0x1A, b'\n']
        );
    }
}
