//! CLI verb implementations (testable: injected store + sinks, returned exit
//! codes). The shims call these; every adverse path exits 1 WITH a reason on
//! stderr — the shim tees stderr into ~/.cache/portal/shim.log, so failures
//! are diagnosable instead of v1's silent fall-through (DESIGN-clipsync §2.6).

use std::io::{Read, Write};

use crate::store::{ClipKind, ClipStore, StoreError};

/// Generous sanity bound on one blob (DESIGN-clipsync §2.1) — NOT a v1-style
/// 8 MiB cliff; big screenshots and files fit with room to spare.
pub const MAX_BLOB_BYTES: u64 = 256 << 20;

/// Which tool's target vocabulary `clip targets` speaks (v1-exact lines).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Tool {
    Xclip,
    WlPaste,
}

/// `portald clip paste [--type text|image/png] [--trim]`
pub fn clip_paste(
    store: &ClipStore,
    want: ClipKind,
    trim: bool,
    out: &mut dyn Write,
    err: &mut dyn Write,
) -> i32 {
    match store.paste(want) {
        Ok(mut data) => {
            if trim && data.last() == Some(&b'\n') {
                data.pop();
            }
            if out.write_all(&data).is_err() {
                return 1;
            }
            0
        }
        Err(e) => {
            explain(err, &e);
            1
        }
    }
}

/// `portald clip targets [xclip|wl-paste]` — advertise what a paste would
/// yield, in the requesting tool's target vocabulary.
pub fn clip_targets(
    store: &ClipStore,
    tool: Tool,
    out: &mut dyn Write,
    err: &mut dyn Write,
) -> i32 {
    let manifest = match store.manifest() {
        Ok(Some(m)) => m,
        Ok(None) => {
            let _ = writeln!(err, "clip targets: no content synced yet");
            return 1;
        }
        Err(e) => {
            explain(err, &e);
            return 1;
        }
    };
    let lines: &str = match (manifest.kind, tool) {
        (ClipKind::Image, _) => "image/png\n",
        (ClipKind::Text, Tool::Xclip) => "UTF8_STRING\nTEXT\nSTRING\n",
        (ClipKind::Text, Tool::WlPaste) => "text/plain\n",
        (ClipKind::Clear, _) => {
            let _ = writeln!(err, "clip targets: clipboard is clear");
            return 1;
        }
    };
    if out.write_all(lines.as_bytes()).is_err() {
        return 1;
    }
    0
}

/// `portald blob put <sha> <size>` — the Mac's blob-push landing point
/// (exec channel; stdin = bytes). Verifies length AND sha before install.
pub fn blob_put(
    store: &ClipStore,
    sha: &str,
    size: u64,
    stdin: &mut dyn Read,
    err: &mut dyn Write,
) -> i32 {
    if size > MAX_BLOB_BYTES {
        let _ = writeln!(err, "blob put: {size} bytes exceeds cap {MAX_BLOB_BYTES}");
        return 1;
    }
    let mut data = Vec::with_capacity(size.min(1 << 20) as usize);
    // Bounded read: never trust the declared size alone.
    if let Err(e) = stdin.take(size + 1).read_to_end(&mut data) {
        let _ = writeln!(err, "blob put: read failed: {e}");
        return 1;
    }
    if data.len() as u64 != size {
        let _ = writeln!(err, "blob put: expected {size} bytes, got {}", data.len());
        return 1;
    }
    match store.put_blob(sha, &data) {
        Ok(()) => {
            let _ = store.gc(); // opportunistic prune, best-effort
            0
        }
        Err(e) => {
            explain(err, &e);
            1
        }
    }
}

/// `portald clip status` — human/doctor view of sync state.
pub fn clip_status(store: &ClipStore, now_unix: u64, out: &mut dyn Write) -> i32 {
    match store.manifest() {
        Ok(Some(m)) => {
            let age = now_unix.saturating_sub(m.received_at);
            let sha8 = m.sha.as_deref().map(|s| &s[..8]).unwrap_or("-");
            let _ = writeln!(
                out,
                "kind={:?} change_id={} age={}s sha={} size={}",
                m.kind,
                m.change_id,
                age,
                sha8,
                m.size.unwrap_or(0),
            );
            0
        }
        Ok(None) => {
            let _ = writeln!(out, "no content synced yet");
            0
        }
        Err(e) => {
            let _ = writeln!(out, "store error: {e}");
            1
        }
    }
}

/// `portald clip copy [--type text|image/png] [--trim] [--empty-clears]` —
/// the box→Mac write path: buffer stdin, install into the LOCAL store (the
/// Mac pulls by sha over exec — bytes never ride the frame), then send the
/// clipwrite request through the running agent. Exit 0 only after the Mac
/// confirmed the pasteboard set.
pub fn clip_copy(
    store: &ClipStore,
    kind: ClipKind,
    trim: bool,
    empty_clears: bool,
    stdin: &mut dyn Read,
    err: &mut dyn Write,
    send: &mut dyn FnMut(&str) -> Result<(), String>,
) -> i32 {
    let mut data = Vec::new();
    if let Err(e) = stdin.take(MAX_BLOB_BYTES + 1).read_to_end(&mut data) {
        let _ = writeln!(err, "clip copy: read failed: {e}");
        return 1;
    }
    if data.len() as u64 > MAX_BLOB_BYTES {
        let _ = writeln!(err, "clip copy: content exceeds cap {MAX_BLOB_BYTES}");
        return 1;
    }
    if trim && data.last() == Some(&b'\n') {
        data.pop();
    }
    let payload = if data.is_empty() {
        if !empty_clears {
            let _ = writeln!(err, "clip copy: empty input");
            return 1;
        }
        serde_json::json!({ "kind": "clear" })
    } else {
        let sha = crate::store::sha256_hex(&data);
        if let Err(e) = store.put_blob(&sha, &data) {
            let _ = writeln!(err, "clip copy: store: {e}");
            return 1;
        }
        serde_json::json!({
            "kind": match kind { ClipKind::Text => "text", ClipKind::Image => "image", ClipKind::Clear => "clear" },
            "format": matches!(kind, ClipKind::Image).then_some("png"),
            "sha": sha,
            "size": data.len() as i64,
        })
    };
    match send(&format!("clipwrite\t{payload}")) {
        Ok(()) => 0,
        Err(reason) => {
            let _ = writeln!(err, "clip copy: {reason}");
            1
        }
    }
}

fn explain(err: &mut dyn Write, e: &StoreError) {
    let hint = match e {
        StoreError::Empty => "nothing on the synced clipboard",
        StoreError::WrongKind => "synced clipboard holds a different content kind",
        StoreError::BlobMissing { .. } => "content transfer from the Mac has not landed yet",
        StoreError::Stale { .. } => "update ordering conflict",
        _ => "store failure",
    };
    let _ = writeln!(err, "portald clip: {e} ({hint})");
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::{Manifest, PNG_MAGIC, sha256_hex};

    fn store_with_text(text: &[u8]) -> (tempfile::TempDir, ClipStore) {
        let dir = tempfile::tempdir().unwrap();
        let s = ClipStore::new(dir.path().join("clip"));
        s.put_blob(&sha256_hex(text), text).unwrap();
        s.apply(&Manifest {
            change_id: 1,
            kind: ClipKind::Text,
            format: None,
            sha: Some(sha256_hex(text)),
            size: Some(text.len() as u64),
            received_at: 100,
        })
        .unwrap();
        (dir, s)
    }

    #[test]
    fn paste_writes_bytes_and_trim_drops_one_newline() {
        let (_d, s) = store_with_text(b"hello\n");
        let (mut out, mut err) = (Vec::new(), Vec::new());
        assert_eq!(clip_paste(&s, ClipKind::Text, false, &mut out, &mut err), 0);
        assert_eq!(out, b"hello\n");
        out.clear();
        assert_eq!(clip_paste(&s, ClipKind::Text, true, &mut out, &mut err), 0);
        assert_eq!(out, b"hello");
    }

    #[test]
    fn paste_failure_is_loud_not_silent() {
        let dir = tempfile::tempdir().unwrap();
        let s = ClipStore::new(dir.path().join("clip"));
        let (mut out, mut err) = (Vec::new(), Vec::new());
        assert_eq!(clip_paste(&s, ClipKind::Text, false, &mut out, &mut err), 1);
        assert!(out.is_empty());
        let msg = String::from_utf8(err).unwrap();
        assert!(msg.contains("no clipboard content"), "{msg}");
    }

    #[test]
    fn targets_speak_each_tools_vocabulary() {
        let (_d, s) = store_with_text(b"txt");
        let (mut out, mut err) = (Vec::new(), Vec::new());
        assert_eq!(clip_targets(&s, Tool::Xclip, &mut out, &mut err), 0);
        assert_eq!(out, b"UTF8_STRING\nTEXT\nSTRING\n");
        out.clear();
        assert_eq!(clip_targets(&s, Tool::WlPaste, &mut out, &mut err), 0);
        assert_eq!(out, b"text/plain\n");

        // Image manifest → image/png for both tools.
        let mut img = PNG_MAGIC.to_vec();
        img.extend_from_slice(b"img");
        s.put_blob(&sha256_hex(&img), &img).unwrap();
        s.apply(&Manifest {
            change_id: 2,
            kind: ClipKind::Image,
            format: Some("png".into()),
            sha: Some(sha256_hex(&img)),
            size: Some(img.len() as u64),
            received_at: 100,
        })
        .unwrap();
        out.clear();
        assert_eq!(clip_targets(&s, Tool::Xclip, &mut out, &mut err), 0);
        assert_eq!(out, b"image/png\n");
    }

    #[test]
    fn blob_put_validates_size_and_hash() {
        let dir = tempfile::tempdir().unwrap();
        let s = ClipStore::new(dir.path().join("clip"));
        let data = b"pushed from the mac";
        let sha = sha256_hex(data);
        let mut err = Vec::new();

        // Wrong declared size.
        assert_eq!(
            blob_put(&s, &sha, 5, &mut &data[..], &mut err),
            1,
            "{}",
            String::from_utf8_lossy(&err)
        );
        // Correct.
        assert_eq!(
            blob_put(&s, &sha, data.len() as u64, &mut &data[..], &mut err),
            0
        );
        assert!(s.has_blob(&sha));
        // Oversized cap.
        assert_eq!(
            blob_put(&s, &sha, MAX_BLOB_BYTES + 1, &mut &data[..], &mut err),
            1
        );
    }

    #[test]
    fn status_reports_state_and_age() {
        let (_d, s) = store_with_text(b"txt");
        let mut out = Vec::new();
        assert_eq!(clip_status(&s, 160, &mut out), 0);
        let line = String::from_utf8(out).unwrap();
        assert!(line.contains("kind=Text"), "{line}");
        assert!(line.contains("age=60s"), "{line}");
    }
}
