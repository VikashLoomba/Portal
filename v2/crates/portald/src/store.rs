//! The box-side clipboard store (DESIGN-clipsync §2.2): what makes paste a
//! LOCAL read. The Mac publishes; this store holds the current content;
//! `portald clip paste` answers from it in microseconds.
//!
//! Layout under `~/.cache/portal/clip/` (dir 0700, files 0600):
//!   current.manifest          JSON (human-inspectable; never crosses the wire)
//!   blob-<sha256-64hex>       content-addressed bytes (text or PNG)
//!
//! Invariants:
//! - a paste can NEVER observe a manifest pointing at a missing or partial
//!   blob: blobs install atomically (tmp+rename, sha-verified) BEFORE the
//!   manifest (tmp+rename) is switched;
//! - change_id is monotonic: stale updates (< current) are rejected,
//!   idempotent re-applies (== current) are accepted (the blob-miss retry
//!   re-sends the same id);
//! - reads verify the sha BEFORE emitting a byte (buffer-then-verify, v1
//!   doctrine) and images must carry the PNG magic;
//! - two writers exist (the agent applies manifests, `portald blob put`
//!   installs blobs over an exec channel) — both effects are atomic renames,
//!   and the manifest authority is the agent alone, so cross-process
//!   interleaving cannot corrupt state.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const PNG_MAGIC: [u8; 8] = [0x89, b'P', b'N', b'G', b'\r', b'\n', 0x1a, b'\n'];

/// Keep the current blob plus this many most-recent others (copy→paste races
/// on rapid re-copies); everything older is pruned by [`ClipStore::gc`].
pub const GC_KEEP_RECENT: usize = 3;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ClipKind {
    Text,
    Image,
    /// The Mac cleared its clipboard; paste answers "nothing".
    Clear,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Manifest {
    pub change_id: u64,
    pub kind: ClipKind,
    /// "png" for images; absent for text/clear.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
    /// Full 64-hex sha256 of the blob; absent for clear.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sha: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<u64>,
    /// Unix seconds when this box applied the update (staleness display).
    pub received_at: u64,
}

#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum StoreError {
    #[error("io: {0}")]
    Io(String),
    #[error("stale update: change_id {got} <= current {current}")]
    Stale { got: u64, current: u64 },
    #[error("blob {sha} not present")]
    BlobMissing { sha: String },
    #[error("content hash mismatch (expected {expected})")]
    HashMismatch { expected: String },
    #[error("no clipboard content")]
    Empty,
    #[error("content kind mismatch")]
    WrongKind,
    #[error("corrupt store: {0}")]
    Corrupt(String),
}

impl From<std::io::Error> for StoreError {
    fn from(e: std::io::Error) -> Self {
        StoreError::Io(e.to_string())
    }
}

pub struct ClipStore {
    dir: PathBuf,
}

impl ClipStore {
    /// Standard location: `~/.cache/portal/clip`.
    pub fn default_dir() -> Option<PathBuf> {
        std::env::var_os("HOME")
            .map(|h| PathBuf::from(h).join(".cache").join("portal").join("clip"))
    }

    pub fn new(dir: impl Into<PathBuf>) -> Self {
        Self { dir: dir.into() }
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    fn manifest_path(&self) -> PathBuf {
        self.dir.join("current.manifest")
    }

    fn blob_path(&self, sha: &str) -> PathBuf {
        self.dir.join(format!("blob-{sha}"))
    }

    fn ensure_dir(&self) -> Result<(), StoreError> {
        fs::create_dir_all(&self.dir)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&self.dir, fs::Permissions::from_mode(0o700))?;
        }
        Ok(())
    }

    /// Install a blob content-addressed: verify sha, write 0600 tmp, rename.
    /// Idempotent; safe from any process.
    pub fn put_blob(&self, sha: &str, data: &[u8]) -> Result<(), StoreError> {
        if !valid_sha(sha) {
            return Err(StoreError::Corrupt(format!("bad sha {sha:?}")));
        }
        let actual = hex::encode(Sha256::digest(data));
        if actual != sha {
            return Err(StoreError::HashMismatch {
                expected: sha.to_string(),
            });
        }
        self.ensure_dir()?;
        let target = self.blob_path(sha);
        if target.exists() {
            return Ok(()); // content-addressed: already have it
        }
        let tmp = self
            .dir
            .join(format!(".blob.tmp.{}.{}", std::process::id(), sha));
        {
            let mut f = open_0600(&tmp)?;
            f.write_all(data)?;
            f.sync_all()?;
        }
        fs::rename(&tmp, &target)?;
        Ok(())
    }

    pub fn has_blob(&self, sha: &str) -> bool {
        valid_sha(sha) && self.blob_path(sha).exists()
    }

    /// Apply a manifest update (agent-only path). The referenced blob must
    /// already be present — otherwise `BlobMissing`, and the caller answers
    /// the Mac with Ack{have_blob:false} to trigger the blob push + re-send.
    pub fn apply(&self, m: &Manifest) -> Result<(), StoreError> {
        if let Some(current) = self.manifest()? {
            if m.change_id < current.change_id {
                return Err(StoreError::Stale {
                    got: m.change_id,
                    current: current.change_id,
                });
            }
            if m.change_id == current.change_id && *m == current {
                return Ok(()); // idempotent re-apply
            }
        }
        match m.kind {
            ClipKind::Clear => {
                if m.sha.is_some() {
                    return Err(StoreError::Corrupt("clear with a sha".into()));
                }
            }
            ClipKind::Text | ClipKind::Image => {
                let sha = m
                    .sha
                    .as_deref()
                    .ok_or_else(|| StoreError::Corrupt("content manifest without sha".into()))?;
                if !self.has_blob(sha) {
                    return Err(StoreError::BlobMissing {
                        sha: sha.to_string(),
                    });
                }
            }
        }
        self.ensure_dir()?;
        let tmp = self
            .dir
            .join(format!(".manifest.tmp.{}", std::process::id()));
        {
            let mut f = open_0600(&tmp)?;
            f.write_all(
                serde_json::to_string_pretty(m)
                    .map_err(|e| StoreError::Corrupt(e.to_string()))?
                    .as_bytes(),
            )?;
            f.sync_all()?;
        }
        fs::rename(&tmp, self.manifest_path())?;
        Ok(())
    }

    /// Current manifest, or None before the first sync.
    pub fn manifest(&self) -> Result<Option<Manifest>, StoreError> {
        let raw = match fs::read(self.manifest_path()) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        serde_json::from_slice(&raw)
            .map(Some)
            .map_err(|e| StoreError::Corrupt(format!("manifest: {e}")))
    }

    /// The paste read: manifest → kind gate → blob → sha verify → (PNG magic
    /// for images) → bytes. Every failure is a typed error so the CLI can
    /// exit 1 with a LOGGED reason (never silent, per the design).
    pub fn paste(&self, want: ClipKind) -> Result<Vec<u8>, StoreError> {
        let m = self.manifest()?.ok_or(StoreError::Empty)?;
        if m.kind == ClipKind::Clear {
            return Err(StoreError::Empty);
        }
        if m.kind != want {
            return Err(StoreError::WrongKind);
        }
        let sha = m
            .sha
            .as_deref()
            .ok_or_else(|| StoreError::Corrupt("content manifest without sha".into()))?;
        let data = fs::read(self.blob_path(sha)).map_err(|e| {
            if e.kind() == std::io::ErrorKind::NotFound {
                StoreError::BlobMissing {
                    sha: sha.to_string(),
                }
            } else {
                e.into()
            }
        })?;
        if hex::encode(Sha256::digest(&data)) != sha {
            return Err(StoreError::HashMismatch {
                expected: sha.to_string(),
            });
        }
        if m.kind == ClipKind::Image && !data.starts_with(&PNG_MAGIC) {
            return Err(StoreError::Corrupt("image blob is not a PNG".into()));
        }
        Ok(data)
    }

    /// Prune blobs beyond current + [`GC_KEEP_RECENT`] most-recent, plus any
    /// leftover tmp fragments. Best-effort.
    pub fn gc(&self) -> Result<usize, StoreError> {
        let current_sha = self.manifest()?.and_then(|m| m.sha);
        let mut blobs: Vec<(PathBuf, std::time::SystemTime)> = Vec::new();
        let entries = match fs::read_dir(&self.dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(0),
            Err(e) => return Err(e.into()),
        };
        let mut removed = 0;
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if name.starts_with(".blob.tmp.") || name.starts_with(".manifest.tmp.") {
                if fs::remove_file(entry.path()).is_ok() {
                    removed += 1;
                }
                continue;
            }
            if let Some(sha) = name.strip_prefix("blob-") {
                if Some(sha) == current_sha.as_deref() {
                    continue; // never GC the live blob
                }
                let mtime = entry
                    .metadata()
                    .and_then(|m| m.modified())
                    .unwrap_or(std::time::UNIX_EPOCH);
                blobs.push((entry.path(), mtime));
            }
        }
        blobs.sort_by(|a, b| b.1.cmp(&a.1)); // newest first
        for (path, _) in blobs.into_iter().skip(GC_KEEP_RECENT) {
            if fs::remove_file(path).is_ok() {
                removed += 1;
            }
        }
        Ok(removed)
    }
}

fn valid_sha(sha: &str) -> bool {
    sha.len() == 64
        && sha
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
}

fn open_0600(path: &Path) -> Result<fs::File, StoreError> {
    let mut opts = fs::OpenOptions::new();
    opts.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    Ok(opts.open(path)?)
}

pub fn sha256_hex(data: &[u8]) -> String {
    hex::encode(Sha256::digest(data))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn store() -> (tempfile::TempDir, ClipStore) {
        let dir = tempfile::tempdir().unwrap();
        let s = ClipStore::new(dir.path().join("clip"));
        (dir, s)
    }

    fn text_manifest(id: u64, data: &[u8]) -> Manifest {
        Manifest {
            change_id: id,
            kind: ClipKind::Text,
            format: None,
            sha: Some(sha256_hex(data)),
            size: Some(data.len() as u64),
            received_at: 1_700_000_000,
        }
    }

    fn png() -> Vec<u8> {
        let mut v = PNG_MAGIC.to_vec();
        v.extend_from_slice(b"fake image payload");
        v
    }

    #[test]
    fn blob_then_manifest_then_paste() {
        let (_d, s) = store();
        let data = b"hello from the mac";
        s.put_blob(&sha256_hex(data), data).unwrap();
        s.apply(&text_manifest(1, data)).unwrap();
        assert_eq!(s.paste(ClipKind::Text).unwrap(), data);
        // Kind gate: an image paste on a text clipboard answers WrongKind.
        assert_eq!(s.paste(ClipKind::Image).unwrap_err(), StoreError::WrongKind);
    }

    #[test]
    fn manifest_without_blob_is_rejected_for_ack_retry() {
        let (_d, s) = store();
        let err = s.apply(&text_manifest(1, b"never pushed")).unwrap_err();
        assert!(matches!(err, StoreError::BlobMissing { .. }));
        // Paste still answers Empty — the half-applied update is invisible.
        assert_eq!(s.paste(ClipKind::Text).unwrap_err(), StoreError::Empty);
    }

    #[test]
    fn stale_updates_rejected_idempotent_reapply_ok() {
        let (_d, s) = store();
        let a = b"first";
        let b = b"second";
        s.put_blob(&sha256_hex(a), a).unwrap();
        s.put_blob(&sha256_hex(b), b).unwrap();
        s.apply(&text_manifest(5, a)).unwrap();
        s.apply(&text_manifest(5, a)).unwrap(); // re-send after blob push
        assert!(matches!(
            s.apply(&text_manifest(4, b)).unwrap_err(),
            StoreError::Stale { got: 4, current: 5 }
        ));
        s.apply(&text_manifest(6, b)).unwrap();
        assert_eq!(s.paste(ClipKind::Text).unwrap(), b);
    }

    #[test]
    fn put_blob_verifies_hash_and_sha_shape() {
        let (_d, s) = store();
        assert!(matches!(
            s.put_blob(&sha256_hex(b"other"), b"data").unwrap_err(),
            StoreError::HashMismatch { .. }
        ));
        assert!(matches!(
            s.put_blob("not-a-sha", b"data").unwrap_err(),
            StoreError::Corrupt(_)
        ));
    }

    #[test]
    fn paste_verifies_blob_hash_and_png_magic() {
        let (_d, s) = store();
        let data = b"text payload";
        let sha = sha256_hex(data);
        s.put_blob(&sha, data).unwrap();
        s.apply(&text_manifest(1, data)).unwrap();
        // Corrupt the blob on disk behind the store's back.
        std::fs::write(s.dir().join(format!("blob-{sha}")), b"tampered").unwrap();
        assert!(matches!(
            s.paste(ClipKind::Text).unwrap_err(),
            StoreError::HashMismatch { .. }
        ));

        // Image manifest pointing at a non-PNG blob refuses to emit.
        let not_png = b"not a png";
        let sha2 = sha256_hex(not_png);
        s.put_blob(&sha2, not_png).unwrap();
        s.apply(&Manifest {
            change_id: 2,
            kind: ClipKind::Image,
            format: Some("png".into()),
            sha: Some(sha2),
            size: Some(not_png.len() as u64),
            received_at: 0,
        })
        .unwrap();
        assert!(matches!(
            s.paste(ClipKind::Image).unwrap_err(),
            StoreError::Corrupt(_)
        ));

        // A real PNG-magic blob pastes fine.
        let img = png();
        let sha3 = sha256_hex(&img);
        s.put_blob(&sha3, &img).unwrap();
        s.apply(&Manifest {
            change_id: 3,
            kind: ClipKind::Image,
            format: Some("png".into()),
            sha: Some(sha3),
            size: Some(img.len() as u64),
            received_at: 0,
        })
        .unwrap();
        assert_eq!(s.paste(ClipKind::Image).unwrap(), img);
    }

    #[test]
    fn clear_propagates_and_empty_before_first_sync() {
        let (_d, s) = store();
        assert_eq!(s.paste(ClipKind::Text).unwrap_err(), StoreError::Empty);
        let data = b"soon cleared";
        s.put_blob(&sha256_hex(data), data).unwrap();
        s.apply(&text_manifest(1, data)).unwrap();
        s.apply(&Manifest {
            change_id: 2,
            kind: ClipKind::Clear,
            format: None,
            sha: None,
            size: None,
            received_at: 0,
        })
        .unwrap();
        assert_eq!(s.paste(ClipKind::Text).unwrap_err(), StoreError::Empty);
    }

    #[test]
    fn gc_keeps_current_plus_recent_and_sweeps_tmp() {
        let (_d, s) = store();
        let mut shas = Vec::new();
        for i in 0..6u8 {
            let data = vec![i; 8];
            let sha = sha256_hex(&data);
            s.put_blob(&sha, &data).unwrap();
            shas.push((sha, data));
        }
        // Manifest points at the FIRST (oldest) blob.
        let (sha0, data0) = &shas[0];
        s.apply(&Manifest {
            change_id: 1,
            kind: ClipKind::Text,
            format: None,
            sha: Some(sha0.clone()),
            size: Some(data0.len() as u64),
            received_at: 0,
        })
        .unwrap();
        std::fs::write(s.dir().join(".blob.tmp.999.junk"), b"x").unwrap();

        let removed = s.gc().unwrap();
        assert!(removed >= 1, "tmp fragment swept");
        assert!(s.has_blob(sha0), "live blob never GC'd");
        let remaining = shas.iter().filter(|(sha, _)| s.has_blob(sha)).count();
        assert_eq!(remaining, 1 + GC_KEEP_RECENT);
        assert_eq!(s.paste(ClipKind::Text).unwrap(), *data0);
    }

    #[cfg(unix)]
    #[test]
    fn files_are_private() {
        use std::os::unix::fs::PermissionsExt;
        let (_d, s) = store();
        let data = b"secret-ish";
        let sha = sha256_hex(data);
        s.put_blob(&sha, data).unwrap();
        s.apply(&text_manifest(1, data)).unwrap();
        let dir_mode = std::fs::metadata(s.dir()).unwrap().permissions().mode();
        assert_eq!(dir_mode & 0o777, 0o700);
        let blob_mode = std::fs::metadata(s.dir().join(format!("blob-{sha}")))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(blob_mode & 0o777, 0o600);
    }
}
