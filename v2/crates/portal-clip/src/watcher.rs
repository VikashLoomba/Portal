//! The clipboard watcher (DESIGN-clipsync §2.1): polls the pasteboard
//! generation counter (~4Hz — one int read, microseconds) and emits a
//! [`WatchEvent`] whenever the content generation changes. Platform-agnostic
//! over a [`SnapshotSource`] so the state machine is testable without a Mac
//! pasteboard; production wires [`crate::macos::NativePasteboard`].
//!
//! Gating (fail-closed) happens HERE, at publish time — concealed/transient
//! content and feature-disabled kinds never leave the process:
//! - concealed ⇒ no event at all (not even Clear: revealing that "something
//!   secret was copied" is itself a leak);
//! - `clip-text` / `clip-image` capability gates are re-read LIVE per change
//!   (the v1 file-per-toggle contract);
//! - a gated-off kind emits [`WatchEvent::Cleared`] so boxes drop the
//!   PREVIOUS content instead of serving it forever.

use std::time::Duration;

use crate::ClipKind;

/// One consistent read of the clipboard at a generation (mirror of
/// `macos::Snapshot`, re-declared here so the watcher has no macOS dep).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Observation {
    Empty,
    Concealed,
    Content { kind: ClipKind, data: Vec<u8> },
}

/// Source of generation counters + consistent snapshots.
pub trait SnapshotSource: Send {
    fn change_count(&self) -> i64;
    fn observe(&self) -> Result<Observation, crate::ClipError>;
}

#[cfg(target_os = "macos")]
impl SnapshotSource for crate::macos::NativePasteboard {
    fn change_count(&self) -> i64 {
        crate::macos::NativePasteboard::change_count(self)
    }
    fn observe(&self) -> Result<Observation, crate::ClipError> {
        Ok(match self.snapshot()? {
            crate::macos::Snapshot::Empty { .. } => Observation::Empty,
            crate::macos::Snapshot::Concealed { .. } => Observation::Concealed,
            crate::macos::Snapshot::Content { kind, data, .. } => {
                Observation::Content { kind, data }
            }
        })
    }
}

/// What the publisher receives. `change_id` is the watcher's own monotonic
/// counter (NOT the raw NSPasteboard changeCount, which can skip and resets
/// per login session).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WatchEvent {
    Changed {
        change_id: u64,
        kind: ClipKind,
        data: Vec<u8>,
    },
    Cleared {
        change_id: u64,
    },
}

/// Live capability gates; production reads the feature files.
pub trait Gates: Send {
    fn text_enabled(&self) -> bool;
    fn image_enabled(&self) -> bool;
}

/// Default poll cadence (§2.1). Cheap enough for 250ms; copy→paste latency
/// budget is dominated by the blob transfer anyway.
pub const POLL_INTERVAL: Duration = Duration::from_millis(250);

/// The pure state machine: feed it counter observations, get events.
pub struct Watcher<S: SnapshotSource, G: Gates> {
    source: S,
    gates: G,
    last_count: Option<i64>,
    next_change_id: u64,
    /// Whether the last emitted event was Cleared (dedupe consecutive clears).
    last_was_clear: bool,
}

impl<S: SnapshotSource, G: Gates> Watcher<S, G> {
    pub fn new(source: S, gates: G) -> Self {
        // change_id is CLOCK-SEEDED, not zero-seeded: box stores persist
        // across Mac daemon restarts, and a restarted daemon starting over at
        // 1,2,3… would have every update Stale-rejected against the store's
        // previous (higher) change_id. Milliseconds-since-epoch is monotonic
        // across restarts at any human copy cadence.
        let seed = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(1);
        Self {
            source,
            gates,
            last_count: None,
            next_change_id: seed,
            last_was_clear: false,
        }
    }

    /// One poll step. Returns an event iff the generation moved AND the
    /// content is publishable under the gates.
    pub fn poll(&mut self) -> Option<WatchEvent> {
        let count = self.source.change_count();
        if self.last_count == Some(count) {
            return None;
        }
        self.last_count = Some(count);

        let observation = match self.source.observe() {
            Ok(o) => o,
            Err(err) => {
                // Fail closed: an unreadable clipboard publishes nothing.
                tracing::warn!(target: "portal::clip", %err, "pasteboard read failed; not publishing");
                return None;
            }
        };
        match observation {
            Observation::Concealed => {
                // Deliberately NOT a Clear: boxes keep the previous content,
                // and the concealed copy's existence is never signaled.
                tracing::debug!(target: "portal::clip", "concealed clipboard change; skipping");
                None
            }
            Observation::Empty => self.emit_clear(),
            Observation::Content { kind, data } => {
                let enabled = match kind {
                    ClipKind::Text => self.gates.text_enabled(),
                    ClipKind::Image => self.gates.image_enabled(),
                };
                if !enabled {
                    // Gated off: clear so boxes drop the PREVIOUS content.
                    return self.emit_clear();
                }
                self.last_was_clear = false;
                self.next_change_id += 1;
                Some(WatchEvent::Changed {
                    change_id: self.next_change_id,
                    kind,
                    data,
                })
            }
        }
    }

    fn emit_clear(&mut self) -> Option<WatchEvent> {
        if self.last_was_clear {
            return None; // consecutive clears collapse
        }
        self.last_was_clear = true;
        self.next_change_id += 1;
        Some(WatchEvent::Cleared {
            change_id: self.next_change_id,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
    use std::sync::{Arc, Mutex};

    struct FakeSource {
        count: Arc<AtomicI64>,
        observation: Arc<Mutex<Observation>>,
    }

    impl SnapshotSource for FakeSource {
        fn change_count(&self) -> i64 {
            self.count.load(Ordering::SeqCst)
        }
        fn observe(&self) -> Result<Observation, crate::ClipError> {
            Ok(self.observation.lock().unwrap().clone())
        }
    }

    struct FakeGates {
        text: Arc<AtomicBool>,
        image: Arc<AtomicBool>,
    }

    impl Gates for FakeGates {
        fn text_enabled(&self) -> bool {
            self.text.load(Ordering::SeqCst)
        }
        fn image_enabled(&self) -> bool {
            self.image.load(Ordering::SeqCst)
        }
    }

    struct Rig {
        count: Arc<AtomicI64>,
        observation: Arc<Mutex<Observation>>,
        text: Arc<AtomicBool>,
        image: Arc<AtomicBool>,
        watcher: Watcher<FakeSource, FakeGates>,
    }

    fn rig() -> Rig {
        let count = Arc::new(AtomicI64::new(0));
        let observation = Arc::new(Mutex::new(Observation::Empty));
        let text = Arc::new(AtomicBool::new(true));
        let image = Arc::new(AtomicBool::new(true));
        let watcher = Watcher::new(
            FakeSource {
                count: count.clone(),
                observation: observation.clone(),
            },
            FakeGates {
                text: text.clone(),
                image: image.clone(),
            },
        );
        Rig {
            count,
            observation,
            text,
            image,
            watcher,
        }
    }

    impl Rig {
        fn copy(&mut self, o: Observation) -> Option<WatchEvent> {
            self.count.fetch_add(1, Ordering::SeqCst);
            *self.observation.lock().unwrap() = o;
            self.watcher.poll()
        }
    }

    fn text(data: &[u8]) -> Observation {
        Observation::Content {
            kind: ClipKind::Text,
            data: data.to_vec(),
        }
    }

    #[test]
    fn emits_on_change_only_with_monotonic_ids() {
        let mut r = rig();
        // First poll observes the initial empty clipboard → one Cleared.
        let first = match r.watcher.poll() {
            Some(WatchEvent::Cleared { change_id }) => change_id,
            other => panic!("{other:?}"),
        };
        assert!(first > 0, "clock-seeded, not zero");
        assert_eq!(r.watcher.poll(), None, "no generation change, no event");

        let second = match r.copy(text(b"hello")) {
            Some(WatchEvent::Changed {
                change_id,
                kind: ClipKind::Text,
                data,
            }) => {
                assert_eq!(data, b"hello");
                change_id
            }
            other => panic!("{other:?}"),
        };
        assert_eq!(second, first + 1, "monotonic");
        assert_eq!(r.watcher.poll(), None);
        match r.copy(text(b"world")) {
            Some(WatchEvent::Changed { change_id, .. }) => assert_eq!(change_id, second + 1),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn concealed_copies_are_invisible_and_keep_prior_content() {
        let mut r = rig();
        let _ = r.watcher.poll();
        assert!(r.copy(text(b"public")).is_some());
        // A password-manager copy: no event of ANY kind.
        assert_eq!(r.copy(Observation::Concealed), None);
        // Next real copy resumes normally.
        assert!(matches!(
            r.copy(text(b"next")),
            Some(WatchEvent::Changed { .. })
        ));
    }

    #[test]
    fn gated_off_kind_clears_instead_of_publishing() {
        let mut r = rig();
        let _ = r.watcher.poll();
        // Establish real content first — the gate-off must CLEAR it on boxes.
        assert!(matches!(
            r.copy(text(b"published")),
            Some(WatchEvent::Changed { .. })
        ));
        r.text.store(false, Ordering::SeqCst);
        assert!(matches!(
            r.copy(text(b"secret-ish")),
            Some(WatchEvent::Cleared { .. })
        ));
        // Consecutive gated copies collapse (no clear-spam).
        assert_eq!(r.copy(text(b"more")), None);
        // Re-enable: publishing resumes.
        r.text.store(true, Ordering::SeqCst);
        assert!(matches!(
            r.copy(text(b"visible again")),
            Some(WatchEvent::Changed { .. })
        ));
        // Image gate is independent.
        r.image.store(false, Ordering::SeqCst);
        assert!(matches!(
            r.copy(Observation::Content {
                kind: ClipKind::Image,
                data: b"png".to_vec()
            }),
            Some(WatchEvent::Cleared { .. })
        ));
    }

    #[test]
    fn empty_clipboard_clears_once() {
        let mut r = rig();
        let _ = r.watcher.poll();
        assert!(r.copy(text(b"x")).is_some());
        assert!(matches!(
            r.copy(Observation::Empty),
            Some(WatchEvent::Cleared { .. })
        ));
        assert_eq!(r.copy(Observation::Empty), None, "clears collapse");
    }
}
