//! The clipsync PUBLISHER (Mac side, DESIGN-clipsync §2.1/§2.4): consumes
//! watcher events, decides inline-vs-blob, sends `clipsync update`/`clear`
//! frames, and answers `Ack{have_blob:false}` by pushing the blob over a
//! dedicated exec channel (`portald blob put <sha> <size>`, bytes on stdin,
//! sha-verified box-side) and re-sending the update.
//!
//! State machine per box (latest-wins — a newer copy aborts any older
//! transfer; there is never a queue of stale clipboards):
//!
//! ```text
//! WatchEvent::Changed ──► inline?  ──► send update ──► ack{have_blob:*} ──► synced
//!        │                  └─ blob ──► send update ──► ack{have_blob:false}
//!        │                                └──► blob put (exec) ──► resend update ──► ack ──► synced
//!        └─ Cleared ──► send clear ──► synced
//! ```
//!
//! Failure posture: any send/push failure marks the entry dirty and relies on
//! (a) the next watcher event, or (b) reconnect replay — `on_connected`
//! re-sends the current state — to reconverge. Session death mid-transfer is
//! therefore safe: the box's store rejects half-applied updates by design.

use portal_proto::messages::{CLIPSYNC_INLINE_MAX, ClipSyncAck, ClipSyncClear, ClipSyncUpdate};
use serde_bytes::ByteBuf;
use tokio::sync::mpsc;

use portal_clip::ClipKind;
use portal_clip::watcher::WatchEvent;

use crate::agentclient::session::Outbound;

/// How the publisher pushes blob bytes to the box (production: transport
/// exec of `portald blob put` with the bytes as stdin). Returns Err on any
/// transfer/verification failure.
#[async_trait::async_trait]
pub trait BlobPusher: Send + Sync {
    async fn push_blob(&self, sha: &str, data: &[u8]) -> Result<(), String>;
}

/// Production pusher over a Transport.
pub struct ExecBlobPusher {
    pub transport: std::sync::Arc<dyn portal_transport::Transport>,
    /// Remote portald path (the bootstrap's stable symlink).
    pub portald_path: String,
}

#[async_trait::async_trait]
impl BlobPusher for ExecBlobPusher {
    async fn push_blob(&self, sha: &str, data: &[u8]) -> Result<(), String> {
        let argv = vec![
            self.portald_path.clone(),
            "blob".to_string(),
            "put".to_string(),
            sha.to_string(),
            data.len().to_string(),
        ];
        match self.transport.exec(data, &argv).await {
            Ok(_) => Ok(()),
            Err(e) => Err(e.to_string()),
        }
    }
}

/// The current publishable clipboard state (latest-wins).
#[derive(Debug, Clone, PartialEq, Eq)]
enum Current {
    Nothing,
    Clear {
        change_id: u64,
    },
    Content {
        change_id: u64,
        kind: ClipKind,
        sha: String,
        data: Vec<u8>,
        inline: bool,
    },
}

impl Current {
    fn change_id(&self) -> u64 {
        match self {
            Current::Nothing => 0,
            Current::Clear { change_id } | Current::Content { change_id, .. } => *change_id,
        }
    }
}

/// Per-box publisher. Owns no I/O beyond the injected sinks, so the whole
/// state machine is unit-testable.
pub struct Publisher<P: BlobPusher> {
    outbound: mpsc::Sender<Outbound>,
    pusher: P,
    current: Current,
    /// True once the box acked the current change_id.
    synced: bool,
    box_name: String,
}

impl<P: BlobPusher> Publisher<P> {
    pub fn new(box_name: impl Into<String>, outbound: mpsc::Sender<Outbound>, pusher: P) -> Self {
        Self {
            outbound,
            pusher,
            current: Current::Nothing,
            synced: false,
            box_name: box_name.into(),
        }
    }

    /// Whether the box has confirmed the current state (status surface).
    pub fn synced(&self) -> bool {
        self.synced
    }

    pub fn current_change_id(&self) -> u64 {
        self.current.change_id()
    }

    /// A new clipboard generation from the watcher. Latest-wins: replaces
    /// whatever was in flight.
    pub async fn on_event(&mut self, ev: WatchEvent) {
        self.current = match ev {
            WatchEvent::Cleared { change_id } => Current::Clear { change_id },
            WatchEvent::Changed {
                change_id,
                kind,
                data,
            } => {
                let sha = portald_sha(&data);
                let inline = kind == ClipKind::Text && data.len() <= CLIPSYNC_INLINE_MAX;
                Current::Content {
                    change_id,
                    kind,
                    sha,
                    data,
                    inline,
                }
            }
        };
        self.synced = false;
        self.send_current().await;
    }

    /// Session (re)connected: replay the current state so a reconnecting box
    /// converges without waiting for the next copy.
    pub async fn on_connected(&mut self) {
        self.synced = false;
        self.send_current().await;
    }

    /// An ack landed from the box.
    pub async fn on_ack(&mut self, ack: ClipSyncAck) {
        if ack.change_id != self.current.change_id() {
            return; // stale ack for a superseded generation
        }
        if ack.have_blob {
            if !self.synced {
                self.synced = true;
                tracing::debug!(target: "portal::clipsync", box_name = %self.box_name,
                    change_id = ack.change_id, "box in sync");
            }
            return;
        }
        // Box needs the bytes: push, then re-send the update (the box applies
        // the manifest only once the blob is present).
        let Current::Content {
            change_id,
            sha,
            data,
            ..
        } = &self.current
        else {
            return; // Clear never carries a blob; nothing to push
        };
        let (change_id, sha, data) = (*change_id, sha.clone(), data.clone());
        match self.pusher.push_blob(&sha, &data).await {
            Ok(()) => {
                // Superseded while pushing? Drop the stale re-send.
                if self.current.change_id() == change_id {
                    self.send_current().await;
                }
            }
            Err(err) => {
                tracing::warn!(target: "portal::clipsync", box_name = %self.box_name,
                    change_id, %err, "blob push failed; will retry on next event/reconnect");
            }
        }
    }

    async fn send_current(&mut self) {
        let out = match &self.current {
            Current::Nothing => return,
            Current::Clear { change_id } => Outbound::clipsync_clear(&ClipSyncClear {
                change_id: *change_id,
            }),
            Current::Content {
                change_id,
                kind,
                sha,
                data,
                inline,
            } => Outbound::clipsync_update(&ClipSyncUpdate {
                change_id: *change_id,
                kind: kind.wire_name().to_string(),
                format: (*kind == ClipKind::Image).then(|| "png".to_string()),
                sha: Some(sha.clone()),
                size: Some(data.len() as i64),
                inline: inline.then(|| ByteBuf::from(data.clone())),
            }),
        };
        match out {
            Ok(frame) => {
                if self.outbound.send(frame).await.is_err() {
                    tracing::debug!(target: "portal::clipsync", box_name = %self.box_name,
                        "outbound closed; publisher going idle");
                }
            }
            Err(err) => {
                tracing::warn!(target: "portal::clipsync", box_name = %self.box_name, %err,
                    "clipsync frame marshal failed");
            }
        }
    }
}

/// sha256 hex — MUST match portald's store keying.
pub fn portald_sha(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    hex::encode(Sha256::digest(data))
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_proto::messages::unmarshal_payload;
    use std::sync::Mutex;

    struct FakePusher {
        pushes: Mutex<Vec<(String, Vec<u8>)>>,
        fail: std::sync::atomic::AtomicBool,
    }

    impl FakePusher {
        fn new() -> Self {
            Self {
                pushes: Mutex::new(Vec::new()),
                fail: false.into(),
            }
        }
    }

    #[async_trait::async_trait]
    impl BlobPusher for &FakePusher {
        async fn push_blob(&self, sha: &str, data: &[u8]) -> Result<(), String> {
            if self.fail.load(std::sync::atomic::Ordering::SeqCst) {
                return Err("scripted push failure".into());
            }
            self.pushes
                .lock()
                .unwrap()
                .push((sha.to_string(), data.to_vec()));
            Ok(())
        }
    }

    fn update_from(out: &Outbound) -> ClipSyncUpdate {
        assert_eq!((out.service, out.kind), ("clipsync", "update"));
        unmarshal_payload(&out.payload).unwrap()
    }

    fn changed(id: u64, kind: ClipKind, data: &[u8]) -> WatchEvent {
        WatchEvent::Changed {
            change_id: id,
            kind,
            data: data.to_vec(),
        }
    }

    fn ack(id: u64, have_blob: bool) -> ClipSyncAck {
        ClipSyncAck {
            change_id: id,
            have_blob,
        }
    }

    #[tokio::test]
    async fn small_text_goes_inline_and_syncs_on_ack() {
        let pusher = FakePusher::new();
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);

        p.on_event(changed(1, ClipKind::Text, b"hello")).await;
        let u = update_from(&rx.recv().await.unwrap());
        assert_eq!(u.change_id, 1);
        assert_eq!(u.kind, "text");
        assert_eq!(u.inline.as_ref().unwrap().as_slice(), b"hello");
        assert_eq!(u.sha.as_deref(), Some(portald_sha(b"hello").as_str()));
        assert!(!p.synced());

        p.on_ack(ack(1, true)).await;
        assert!(p.synced());
        assert!(
            pusher.pushes.lock().unwrap().is_empty(),
            "inline needs no push"
        );
    }

    #[tokio::test]
    async fn image_blob_flow_push_then_resend() {
        let pusher = FakePusher::new();
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);
        let img = vec![0x89u8; 1024];

        p.on_event(changed(1, ClipKind::Image, &img)).await;
        let u = update_from(&rx.recv().await.unwrap());
        assert_eq!(u.kind, "image");
        assert_eq!(u.format.as_deref(), Some("png"));
        assert!(u.inline.is_none(), "images are never inline");

        // Box lacks the blob → push + re-send.
        p.on_ack(ack(1, false)).await;
        {
            let pushes = pusher.pushes.lock().unwrap();
            assert_eq!(pushes.len(), 1);
            assert_eq!(pushes[0].0, portald_sha(&img));
            assert_eq!(pushes[0].1, img);
        }
        let resent = update_from(&rx.recv().await.unwrap());
        assert_eq!(resent.change_id, 1);

        p.on_ack(ack(1, true)).await;
        assert!(p.synced());
    }

    #[tokio::test]
    async fn oversized_text_takes_the_blob_path() {
        let pusher = FakePusher::new();
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);
        let big = vec![b'x'; CLIPSYNC_INLINE_MAX + 1];
        p.on_event(changed(1, ClipKind::Text, &big)).await;
        let u = update_from(&rx.recv().await.unwrap());
        assert!(u.inline.is_none());
        assert_eq!(u.size, Some(big.len() as i64));
    }

    #[tokio::test]
    async fn stale_acks_are_ignored_latest_wins() {
        let pusher = FakePusher::new();
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);

        p.on_event(changed(1, ClipKind::Image, b"old")).await;
        let _ = rx.recv().await.unwrap();
        p.on_event(changed(2, ClipKind::Text, b"new")).await;
        let _ = rx.recv().await.unwrap();

        // Ack for the superseded generation: no push, no state change.
        p.on_ack(ack(1, false)).await;
        assert!(pusher.pushes.lock().unwrap().is_empty());
        assert!(!p.synced());

        p.on_ack(ack(2, true)).await;
        assert!(p.synced());
    }

    #[tokio::test]
    async fn clear_propagates_and_needs_no_blob() {
        let pusher = FakePusher::new();
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);
        p.on_event(WatchEvent::Cleared { change_id: 3 }).await;
        let out = rx.recv().await.unwrap();
        assert_eq!((out.service, out.kind), ("clipsync", "clear"));
        let c: ClipSyncClear = unmarshal_payload(&out.payload).unwrap();
        assert_eq!(c.change_id, 3);
        // have_blob=false on a clear is nonsense from an old agent: ignored.
        p.on_ack(ack(3, false)).await;
        assert!(pusher.pushes.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn reconnect_replays_current_state() {
        let pusher = FakePusher::new();
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);
        p.on_event(changed(5, ClipKind::Text, b"sticky")).await;
        let _ = rx.recv().await.unwrap();
        p.on_ack(ack(5, true)).await;
        assert!(p.synced());

        // New session: replay without a new copy.
        p.on_connected().await;
        assert!(!p.synced());
        let u = update_from(&rx.recv().await.unwrap());
        assert_eq!(u.change_id, 5);
    }

    #[tokio::test]
    async fn push_failure_leaves_state_dirty_for_retry() {
        let pusher = FakePusher::new();
        pusher.fail.store(true, std::sync::atomic::Ordering::SeqCst);
        let (tx, mut rx) = mpsc::channel(8);
        let mut p = Publisher::new("devbox1", tx, &pusher);
        p.on_event(changed(1, ClipKind::Image, b"img")).await;
        let _ = rx.recv().await.unwrap();
        p.on_ack(ack(1, false)).await; // push fails
        assert!(!p.synced());
        assert!(rx.try_recv().is_err(), "no re-send after failed push");

        // Reconnect replay reconverges once pushing works again.
        pusher
            .fail
            .store(false, std::sync::atomic::Ordering::SeqCst);
        p.on_connected().await;
        let _ = rx.recv().await.unwrap();
        p.on_ack(ack(1, false)).await;
        assert_eq!(pusher.pushes.lock().unwrap().len(), 1);
        let _ = rx.recv().await.unwrap(); // re-sent update
        p.on_ack(ack(1, true)).await;
        assert!(p.synced());
    }
}
