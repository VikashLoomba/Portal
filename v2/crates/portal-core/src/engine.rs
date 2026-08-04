//! Reconcile planner — the pure heart of the forwarding engine (port of
//! `internal/forward/engine.go`, minus the I/O).
//!
//! v1 doctrine carried over unchanged:
//! - STATELESS per pass: desired comes from the agent snapshot, current from
//!   the live master (`PortForwarder::list_forwards`) — never a cache;
//! - discovery failure ⇒ KEEP current forwards (never cancel on error);
//! - agent disconnect ⇒ KEEP forwards; reconnect's snapshot reconverges.
//!
//! Those behaviors live in the daemon loop (supervisor); this module is the
//! pure diff so it can be exhaustively unit-tested.
//!
//! v2 change: forwards are (local, remote) pairs via a mapping function
//! (see `portmap`), not same-port.

use std::collections::{BTreeSet, HashMap};
use std::time::Duration;

use portal_transport::lsof::LsofPorts;
use portal_transport::{ForwardSpec, PortForwarder, Transport, TransportError};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::agentclient::Event;
use crate::portmap::PortMap;

/// One reconcile pass's work order.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Plan {
    pub add: Vec<ForwardSpec>,
    pub remove: Vec<ForwardSpec>,
    /// Remote ports we could not map to any local port (fallback range
    /// exhausted). Reported, never silently dropped.
    pub unmappable: Vec<u16>,
}

/// Compute the pass: `desired_remote` (already deny/allow-filtered), `current`
/// (ground truth from the master), and `map` (remote → local, e.g.
/// `PortMap::local_for`). A current forward is kept only if its remote is
/// still desired AND its local matches the current mapping; anything else is
/// removed (a mapping change removes + re-adds).
pub fn plan(
    desired_remote: &[u16],
    current: &[ForwardSpec],
    mut map: impl FnMut(u16) -> Option<u16>,
) -> Plan {
    let mut out = Plan::default();

    let mut desired_pairs: Vec<ForwardSpec> = Vec::with_capacity(desired_remote.len());
    for &remote in desired_remote {
        match map(remote) {
            Some(local) => desired_pairs.push(ForwardSpec { local, remote }),
            None => out.unmappable.push(remote),
        }
    }

    for spec in &desired_pairs {
        if !current.contains(spec) {
            out.add.push(*spec);
        }
    }
    for spec in current {
        if !desired_pairs.contains(spec) {
            out.remove.push(*spec);
        }
    }

    out.add.sort();
    out.remove.sort();
    out.unmappable.sort();
    out
}

/// Local port-conflict DIAGNOSTICS (production = lsof/ps). v2 detects the
/// conflict itself from the bind error ([`TransportError::PortInUse`]); this
/// seam only names the holder for the log/status message.
#[async_trait::async_trait]
pub trait LocalPorts: Send + Sync {
    async fn holder(&self, port: u16) -> Option<u32>;
    async fn process_name(&self, pid: u32) -> String;
}

#[async_trait::async_trait]
impl LocalPorts for LsofPorts {
    async fn holder(&self, port: u16) -> Option<u32> {
        self.local_holder(port).await
    }
    async fn process_name(&self, pid: u32) -> String {
        LsofPorts::process_name(self, pid).await
    }
}

/// Deduped conflict notes (port of internal/forward/conflict.go): a
/// port+holder pair is logged once, cleared when the forward succeeds or the
/// port stops being desired.
#[derive(Debug, Default)]
pub struct ConflictSet {
    seen: HashMap<u16, u32>,
}

impl ConflictSet {
    /// Returns true when this (port, holder) pair is NEW (caller logs).
    pub fn note(&mut self, port: u16, holder: u32) -> bool {
        self.seen.insert(port, holder) != Some(holder)
    }
    pub fn clear(&mut self, port: u16) {
        self.seen.remove(&port);
    }
    /// Drop notes for locals no longer in the desired mapping.
    pub fn prune(&mut self, keep_locals: &[u16]) {
        self.seen.retain(|port, _| keep_locals.contains(port));
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ReconcileError {
    #[error("transport: {0}")]
    Transport(#[from] TransportError),
    #[error("ssh master down")]
    MasterDown,
    /// No snapshot yet — KEEP current forwards, retry on the next event.
    #[error("agent snapshot not ready")]
    AgentNotReady,
}

/// What one pass did (for logs/status/tests).
#[derive(Debug, Default, PartialEq, Eq)]
pub struct PassSummary {
    pub rebuilt: bool,
    pub added: Vec<ForwardSpec>,
    pub removed: Vec<ForwardSpec>,
    /// (local, holder-pid) pairs newly skipped this pass.
    pub conflicts: Vec<(u16, u32)>,
    pub unmappable: Vec<u16>,
}

/// Everything one box's reconcile pass touches. `taken` is the CROSS-BOX
/// local-port claim set (the supervisor shares one across all stacks so two
/// boxes' fallback allocations can never collide).
pub struct BoxState {
    pub box_name: String,
    pub portmap: PortMap,
    pub conflicts: ConflictSet,
    pub skip_local: Vec<u16>,
}

/// ONE stateless pass (port of Engine.Reconcile):
/// 1. ensure + health — rebuild the master if down, then re-derive from
///    ground truth;
/// 2. desired: the agent snapshot (None ⇒ AgentNotReady ⇒ keep forwards);
/// 3. current: `list_forwards` — the transport's live view, never a cache;
/// 4. plan + apply: add desired−current (skipping local conflicts), cancel
///    current−desired. Forward failures log and continue — next pass retries.
pub async fn reconcile_once(
    transport: &dyn Transport,
    forwarder: &dyn PortForwarder,
    local: &dyn LocalPorts,
    st: &mut BoxState,
    taken: &mut BTreeSet<u16>,
    desired: Option<&[u16]>,
) -> Result<PassSummary, ReconcileError> {
    let mut summary = PassSummary {
        rebuilt: transport.ensure().await?,
        ..PassSummary::default()
    };
    let health = transport.health().await?;
    if !health.up {
        return Err(ReconcileError::MasterDown);
    }

    let desired = desired.ok_or(ReconcileError::AgentNotReady)?;
    let current = forwarder.list_forwards().await.unwrap_or_default();

    let plan = {
        let portmap = &mut st.portmap;
        plan(desired, &current, |remote| {
            portmap.local_for(remote, |p| taken.contains(&p))
        })
    };
    summary.unmappable = plan.unmappable;
    for &port in &summary.unmappable {
        tracing::warn!(target: "portal::engine", box_name = %st.box_name, remote = port,
            "no local port available for remote listener");
    }

    for spec in plan.add {
        if st.skip_local.contains(&spec.local) {
            continue;
        }
        match forwarder.forward(spec).await {
            Ok(()) => {
                taken.insert(spec.local);
                st.conflicts.clear(spec.local);
                tracing::info!(target: "portal::engine", box_name = %st.box_name,
                    "forwarded localhost:{} -> {}:{}", spec.local,
                    transport.describe().host, spec.remote);
                summary.added.push(spec);
            }
            Err(TransportError::PortInUse { .. }) => {
                // The bind itself is the conflict signal; lsof/ps only
                // name the holder for the (deduped) diagnostic.
                let holder = local.holder(spec.local).await.unwrap_or(0);
                if st.conflicts.note(spec.local, holder) {
                    let name = local.process_name(holder).await;
                    tracing::warn!(target: "portal::engine", box_name = %st.box_name,
                        local = spec.local, remote = spec.remote, holder, process = %name,
                        "local port in use; skipping forward");
                    summary.conflicts.push((spec.local, holder));
                }
            }
            Err(err) => {
                tracing::warn!(target: "portal::engine", box_name = %st.box_name,
                    local = spec.local, remote = spec.remote, %err, "forward failed");
            }
        }
    }

    for spec in plan.remove {
        let _ = forwarder.cancel(spec).await;
        st.portmap.release(spec.remote);
        taken.remove(&spec.local);
        tracing::info!(target: "portal::engine", box_name = %st.box_name,
            "removed forward {} (no longer wanted)", spec.local);
        summary.removed.push(spec);
    }

    let keep: Vec<u16> = st.portmap.assignments().map(|(_, l)| l).collect();
    st.conflicts.prune(&keep);
    Ok(summary)
}

/// The per-pass driver the event loop calls; implemented by the composition
/// root (holds transport/forwarder/state), faked in loop tests.
#[async_trait::async_trait]
pub trait Reconciler: Send {
    async fn reconcile(&mut self);
}

#[derive(Debug, Clone)]
pub struct LoopConfig {
    /// Coalesces event bursts into one pass (v1: 50ms).
    pub debounce: Duration,
    /// Backstop against master-side drift (v1: 60s when event-driven).
    pub safety_interval: Duration,
}

impl Default for LoopConfig {
    fn default() -> Self {
        Self {
            debounce: Duration::from_millis(50),
            safety_interval: Duration::from_secs(60),
        }
    }
}

/// Event-driven reconcile loop (port of Engine.runEventDriven):
/// - one pass immediately (forwards restore before the first agent event);
/// - Connected / SnapshotReplaced / Delta ⇒ debounced pass;
/// - Disconnected ⇒ log and KEEP forwards (reconnect reconverges);
/// - OpenUrl ⇒ forwarded to `open_url` (not a reconcile trigger);
/// - `kick` ⇒ same debounce path (POST /reconcile);
/// - safety ticker ⇒ unconditional pass.
pub async fn run_reconcile_loop(
    events: &mut mpsc::Receiver<Event>,
    kick: &mut mpsc::Receiver<()>,
    open_url: Option<mpsc::Sender<String>>,
    cfg: LoopConfig,
    cancel: CancellationToken,
    r: &mut dyn Reconciler,
) {
    r.reconcile().await;
    let mut fire_at: Option<tokio::time::Instant> = None;
    let mut safety = tokio::time::interval(cfg.safety_interval);
    safety.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    safety.reset(); // don't double-fire right after the initial pass

    loop {
        let debounce_sleep = async {
            match fire_at {
                Some(at) => tokio::time::sleep_until(at).await,
                None => std::future::pending().await,
            }
        };
        tokio::select! {
            _ = cancel.cancelled() => return,
            ev = events.recv() => {
                match ev {
                    None => return, // client gone: daemon shutting down
                    Some(Event::Connected | Event::SnapshotReplaced | Event::Delta { .. }) => {
                        fire_at.get_or_insert(tokio::time::Instant::now() + cfg.debounce);
                    }
                    Some(Event::Disconnected { error }) => {
                        tracing::warn!(target: "portal::engine",
                            err = error.as_deref().unwrap_or("clean"),
                            "agent disconnected; preserving forwards");
                    }
                    Some(Event::OpenUrl { url }) => {
                        if let Some(sink) = &open_url {
                            let _ = sink.try_send(url);
                        }
                    }
                }
            }
            _ = kick.recv() => {
                fire_at.get_or_insert(tokio::time::Instant::now() + cfg.debounce);
            }
            _ = debounce_sleep => {
                fire_at = None;
                r.reconcile().await;
            }
            _ = safety.tick() => {
                r.reconcile().await;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fs(local: u16, remote: u16) -> ForwardSpec {
        ForwardSpec { local, remote }
    }

    /// Identity-ish mapper for tests: index-1 pretty scheme.
    fn map1(remote: u16) -> Option<u16> {
        crate::portmap::indexed_local_port(1, remote)
    }

    #[test]
    fn adds_new_and_removes_stale() {
        let p = plan(&[3000, 8000], &[fs(18000, 8000), fs(15173, 5173)], map1);
        assert_eq!(p.add, vec![fs(13000, 3000)]);
        assert_eq!(p.remove, vec![fs(15173, 5173)]);
        assert!(p.unmappable.is_empty());
    }

    #[test]
    fn empty_desired_removes_everything() {
        let p = plan(&[], &[fs(18000, 8000)], map1);
        assert!(p.add.is_empty());
        assert_eq!(p.remove, vec![fs(18000, 8000)]);
    }

    #[test]
    fn converged_pass_is_a_noop() {
        let p = plan(&[8000], &[fs(18000, 8000)], map1);
        assert_eq!(p, Plan::default());
    }

    #[test]
    fn mapping_change_removes_and_readds() {
        // The forward exists but at a stale local port (e.g. mapping moved
        // from fallback to indexed after a conflict cleared).
        let p = plan(&[8000], &[fs(60123, 8000)], map1);
        assert_eq!(p.add, vec![fs(18000, 8000)]);
        assert_eq!(p.remove, vec![fs(60123, 8000)]);
    }

    #[test]
    fn unmappable_ports_are_reported_not_dropped() {
        // No fallback wired into the mapper here — 18000 has nowhere to go.
        let p = plan(&[8000, 18000], &[], map1);
        assert_eq!(p.add, vec![fs(18000, 8000)]);
        assert_eq!(p.unmappable, vec![18000]);
    }

    // ---- reconcile_once ----

    use portal_transport::testing::{FakeForwarder, FakeTransport};
    use std::collections::HashMap;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[derive(Default)]
    struct FakeLocal {
        holders: HashMap<u16, u32>,
    }

    #[async_trait::async_trait]
    impl LocalPorts for FakeLocal {
        async fn holder(&self, port: u16) -> Option<u32> {
            self.holders.get(&port).copied()
        }
        async fn process_name(&self, _pid: u32) -> String {
            "fake-proc".into()
        }
    }

    fn state() -> BoxState {
        BoxState {
            box_name: "devbox1".into(),
            portmap: PortMap::new("devbox1", 1),
            conflicts: ConflictSet::default(),
            skip_local: Vec::new(),
        }
    }

    #[tokio::test]
    async fn pass_adds_then_removes() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        let local = FakeLocal::default();
        let mut st = state();
        let mut taken = BTreeSet::new();

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[3000, 8000]))
            .await
            .unwrap();
        assert_eq!(s.added, vec![fs(13000, 3000), fs(18000, 8000)]);
        assert!(taken.contains(&13000) && taken.contains(&18000));

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert!(s.added.is_empty());
        assert_eq!(s.removed, vec![fs(13000, 3000)]);
        assert!(!taken.contains(&13000));
        assert_eq!(
            fwd.forwards
                .lock()
                .unwrap()
                .iter()
                .copied()
                .collect::<Vec<_>>(),
            vec![fs(18000, 8000)]
        );
    }

    #[tokio::test]
    async fn agent_not_ready_keeps_forwards() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.forwards.lock().unwrap().insert(fs(18000, 8000));
        let local = FakeLocal::default();
        let mut st = state();
        let mut taken = BTreeSet::new();

        let err = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, None)
            .await
            .unwrap_err();
        assert!(matches!(err, ReconcileError::AgentNotReady));
        assert_eq!(
            fwd.forwards.lock().unwrap().len(),
            1,
            "forwards must be kept"
        );
    }

    #[tokio::test]
    async fn master_down_is_an_error() {
        let t = FakeTransport::new("devbox1");
        t.health.lock().unwrap().up = false;
        let err = reconcile_once(
            &*t,
            &FakeForwarder::default(),
            &FakeLocal::default(),
            &mut state(),
            &mut BTreeSet::new(),
            Some(&[8000]),
        )
        .await
        .unwrap_err();
        assert!(matches!(err, ReconcileError::MasterDown));
    }

    #[tokio::test]
    async fn bind_conflicts_skip_and_dedupe() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.busy_locals.lock().unwrap().insert(18000); // bind would fail
        let local = FakeLocal {
            holders: HashMap::from([(18000, 999)]),
        };
        let mut st = state();
        let mut taken = BTreeSet::new();

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert_eq!(s.conflicts, vec![(18000, 999)]);
        assert!(fwd.forwards.lock().unwrap().is_empty());

        // Same conflict next pass: still skipped, but NOT re-reported.
        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert!(s.conflicts.is_empty());
        assert!(fwd.forwards.lock().unwrap().is_empty());

        // Holder frees the port: the forward lands and the note clears.
        fwd.busy_locals.lock().unwrap().clear();
        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert_eq!(s.added, vec![fs(18000, 8000)]);
    }

    #[tokio::test]
    async fn generic_forward_failure_logs_and_continues() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.fail_locals.lock().unwrap().insert(18000);
        let mut st = state();
        let mut taken = BTreeSet::new();
        let s = reconcile_once(
            &*t,
            &fwd,
            &FakeLocal::default(),
            &mut st,
            &mut taken,
            Some(&[3000, 8000]),
        )
        .await
        .unwrap();
        // 8000's forward failed (not a conflict), 3000's landed.
        assert_eq!(s.added, vec![fs(13000, 3000)]);
        assert!(s.conflicts.is_empty());
    }

    // ---- run_reconcile_loop ----

    struct Counting(Arc<AtomicUsize>);

    #[async_trait::async_trait]
    impl Reconciler for Counting {
        async fn reconcile(&mut self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    async fn settle() {
        for _ in 0..20 {
            tokio::task::yield_now().await;
        }
    }

    #[tokio::test(start_paused = true)]
    async fn loop_debounces_bursts_and_safety_ticks() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(16);
        let (ktx, mut krx) = mpsc::channel::<()>(1);
        let cancel = CancellationToken::new();
        let cfg = LoopConfig::default(); // 50ms debounce, 60s safety

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            async move {
                let mut r = Counting(n);
                run_reconcile_loop(&mut erx, &mut krx, None, cfg, cancel, &mut r).await;
            }
        });
        settle().await;
        assert_eq!(n.load(Ordering::SeqCst), 1, "initial pass");

        // A burst of events coalesces into ONE debounced pass.
        for _ in 0..3 {
            etx.send(Event::Delta {
                added: vec![1],
                removed: vec![],
            })
            .await
            .unwrap();
        }
        settle().await;
        tokio::time::advance(std::time::Duration::from_millis(49)).await;
        settle().await;
        assert_eq!(n.load(Ordering::SeqCst), 1, "debounce still pending");
        tokio::time::advance(std::time::Duration::from_millis(2)).await;
        settle().await;
        assert_eq!(n.load(Ordering::SeqCst), 2, "one pass for the burst");

        // Disconnected is NOT a trigger (keep forwards).
        etx.send(Event::Disconnected { error: None }).await.unwrap();
        settle().await;
        tokio::time::advance(std::time::Duration::from_millis(100)).await;
        settle().await;
        assert_eq!(n.load(Ordering::SeqCst), 2);

        // Kick takes the same debounce path.
        ktx.send(()).await.unwrap();
        settle().await;
        tokio::time::advance(std::time::Duration::from_millis(51)).await;
        settle().await;
        assert_eq!(n.load(Ordering::SeqCst), 3);

        // Safety tick fires even with no events.
        tokio::time::advance(std::time::Duration::from_secs(61)).await;
        settle().await;
        assert!(n.load(Ordering::SeqCst) >= 4, "safety pass");

        cancel.cancel();
        task.await.unwrap();
    }

    #[tokio::test(start_paused = true)]
    async fn loop_forwards_open_url_without_reconciling() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let (utx, mut urx) = mpsc::channel::<String>(4);
        let cancel = CancellationToken::new();

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            async move {
                let mut r = Counting(n);
                run_reconcile_loop(
                    &mut erx,
                    &mut krx,
                    Some(utx),
                    LoopConfig::default(),
                    cancel,
                    &mut r,
                )
                .await;
            }
        });
        settle().await;
        etx.send(Event::OpenUrl {
            url: "https://example.com".into(),
        })
        .await
        .unwrap();
        settle().await;
        assert_eq!(urx.try_recv().unwrap(), "https://example.com");
        tokio::time::advance(std::time::Duration::from_millis(100)).await;
        settle().await;
        assert_eq!(
            n.load(Ordering::SeqCst),
            1,
            "OpenUrl must not trigger a pass"
        );
        cancel.cancel();
        task.await.unwrap();
    }
}
