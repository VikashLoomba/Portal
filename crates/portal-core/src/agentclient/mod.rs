//! Agent client: speaks the v4 protocol with the remote portald over the
//! transport's stream.
//!
//! Semantics, each pinned by a test:
//! - reconnect loop with exponential backoff (500ms..10s), reset after any
//!   session that survived ≥5s (a short flap cluster must not pin a blip
//!   6 hours later at 10s);
//! - heartbeat watchdog: >12s without ANY agent→client frame ⇒ hung agent ⇒
//!   reconnect;
//! - delta coalescing: PortAdded/PortRemoved bursts within 50ms collapse to
//!   one Delta event (docker compose up = 1 reconcile, not 8);
//! - staleness: events with seq <= snapshot seq are dropped; Snapshot is a
//!   RESET (clears pending deltas);
//! - HelloAck SHA mismatch ⇒ force-delete the stale remote binary and
//!   reconnect (bootstrap re-uploads);
//! - QoS channels: port events ride a shared drop-on-full channel; clip,
//!   clipwrite, notify, and cred each get a DEDICATED channel so a port
//!   burst can never evict user-facing work (v1 DESIGN §5/S10);
//! - disconnect KEEPS forwards (the engine hears Disconnected and does
//!   nothing; reconnect's Snapshot reconverges).

pub mod session;

use std::collections::BTreeMap;

use portal_proto::messages::{
    ClipRequest, ClipSyncAck, ClipWriteRequest, CredRequest, Notify, Port,
};
use tokio::sync::mpsc;

/// Events the reconcile engine (and service handlers) consume.
#[derive(Debug, Clone)]
pub enum Event {
    Connected,
    Disconnected { error: Option<String> },
    SnapshotReplaced,
    Delta { added: Vec<u16>, removed: Vec<u16> },
    OpenUrl { url: String },
}

/// Service requests, delivered on dedicated channels (never the engine one).
#[derive(Debug, Clone)]
pub enum ServiceRequest {
    Clip(ClipRequest),
    ClipWrite(ClipWriteRequest),
    Cred(CredRequest),
    Notify {
        notify: Notify,
        seq: u64,
    },
    /// clipsync ack from the box agent (drives the publisher's retry loop).
    ClipSyncAck(ClipSyncAck),
}

/// Sink bundle: the session pushes into these; the supervisor owns the
/// receive ends. Sends are non-blocking (`try_send`); a full engine channel
/// drops (reconcile re-derives from the snapshot anyway), while the QoS
/// channels are sized so drops mean the handler is genuinely wedged.
#[derive(Clone)]
pub struct EventSinks {
    pub engine: mpsc::Sender<Event>,
    pub clip: mpsc::Sender<ServiceRequest>,
    pub clip_write: mpsc::Sender<ServiceRequest>,
    pub notify: mpsc::Sender<ServiceRequest>,
    pub cred: mpsc::Sender<ServiceRequest>,
    /// clipsync acks → the publisher task (dedicated: an ack must never be
    /// evicted by port-event bursts — it gates the blob-push retry).
    pub clipsync: mpsc::Sender<ServiceRequest>,
}

/// Capacities mirror v1 (events 64, clip 8, clipwrite 8, notify 16, cred 2).
pub struct EventChannels {
    pub sinks: EventSinks,
    pub engine: mpsc::Receiver<Event>,
    pub clip: mpsc::Receiver<ServiceRequest>,
    pub clip_write: mpsc::Receiver<ServiceRequest>,
    pub notify: mpsc::Receiver<ServiceRequest>,
    pub cred: mpsc::Receiver<ServiceRequest>,
    pub clipsync: mpsc::Receiver<ServiceRequest>,
}

impl EventChannels {
    pub fn new() -> Self {
        let (engine_tx, engine_rx) = mpsc::channel(64);
        let (clip_tx, clip_rx) = mpsc::channel(8);
        let (clipw_tx, clipw_rx) = mpsc::channel(8);
        let (notify_tx, notify_rx) = mpsc::channel(16);
        let (cred_tx, cred_rx) = mpsc::channel(2);
        let (clipsync_tx, clipsync_rx) = mpsc::channel(8);
        Self {
            sinks: EventSinks {
                engine: engine_tx,
                clip: clip_tx,
                clip_write: clipw_tx,
                notify: notify_tx,
                cred: cred_tx,
                clipsync: clipsync_tx,
            },
            engine: engine_rx,
            clip: clip_rx,
            clip_write: clipw_rx,
            notify: notify_rx,
            cred: cred_rx,
            clipsync: clipsync_rx,
        }
    }
}

impl Default for EventChannels {
    fn default() -> Self {
        Self::new()
    }
}

/// Shared snapshot cache: the session writes, `desired_ports` (the
/// discoverer) reads. `seq == 0` means "no snapshot yet" — the engine keeps
/// current forwards (ErrAgentNotReady semantics).
#[derive(Debug, Default)]
pub struct SnapshotCache {
    inner: std::sync::Mutex<SnapState>,
}

#[derive(Debug, Default)]
struct SnapState {
    seq: u64,
    ports: Vec<Port>,
}

impl SnapshotCache {
    /// Replace wholesale (Snapshot frame = RESET).
    pub fn replace(&self, seq: u64, ports: Vec<Port>) {
        let mut st = self.inner.lock().unwrap();
        st.seq = seq;
        st.ports = ports;
    }

    /// Apply PortAdded. Returns false (and ignores) when stale (seq <= cached).
    pub fn add(&self, seq: u64, port: Port) -> bool {
        let mut st = self.inner.lock().unwrap();
        if st.seq == 0 || seq <= st.seq {
            return false;
        }
        st.seq = seq;
        st.ports.push(port);
        true
    }

    /// Apply PortRemoved. Returns false when stale.
    pub fn remove(&self, seq: u64, port: u16) -> bool {
        let mut st = self.inner.lock().unwrap();
        if st.seq == 0 || seq <= st.seq {
            return false;
        }
        st.seq = seq;
        st.ports.retain(|p| p.port != port);
        true
    }

    pub fn clear(&self) {
        let mut st = self.inner.lock().unwrap();
        st.seq = 0;
        st.ports.clear();
    }

    /// `None` until the first snapshot lands (agent-not-ready).
    pub fn desired_ports(&self) -> Option<Vec<u16>> {
        let st = self.inner.lock().unwrap();
        if st.seq == 0 {
            return None;
        }
        let mut out: Vec<u16> = st.ports.iter().map(|p| p.port).collect();
        out.sort_unstable();
        out.dedup();
        Some(out)
    }

    pub fn seq(&self) -> u64 {
        self.inner.lock().unwrap().seq
    }
}

/// The services this client advertises in Hello (v4 symmetric negotiation).
/// HONEST advertisement: only what the daemon actually serves today. The
/// agent answers "none"/"no-client" immediately for anything a client did not
/// advertise, while advertising without a handler costs a request timeout.
pub fn client_services() -> BTreeMap<String, u32> {
    BTreeMap::from([
        ("openurl".to_string(), 1),
        ("notify".to_string(), 1),
        ("clipsync".to_string(), 1),
        ("cred".to_string(), 1),
        ("clipwrite".to_string(), 1),
    ])
}

#[cfg(test)]
mod tests {
    use super::*;

    fn port(p: u16) -> Port {
        Port {
            port: p,
            family: 4,
            addr: "127.0.0.1".into(),
            inode_ns: 0,
        }
    }

    #[test]
    fn snapshot_cache_not_ready_until_first_snapshot() {
        let c = SnapshotCache::default();
        assert_eq!(c.desired_ports(), None);
        // Events before any snapshot are stale by definition.
        assert!(!c.add(5, port(80)));
        c.replace(10, vec![port(8000), port(3000)]);
        assert_eq!(c.desired_ports(), Some(vec![3000, 8000]));
    }

    #[test]
    fn stale_events_dropped_fresh_applied() {
        let c = SnapshotCache::default();
        c.replace(100, vec![port(8000)]);
        assert!(!c.add(100, port(9000)), "seq == snapshot seq is stale");
        assert!(!c.remove(99, 8000));
        assert!(c.add(101, port(9000)));
        assert!(c.remove(102, 8000));
        assert_eq!(c.desired_ports(), Some(vec![9000]));
        assert_eq!(c.seq(), 102);
    }
}
