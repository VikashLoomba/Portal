//! Reconcile planner — the pure heart of the forwarding engine (no I/O).
//!
//! Doctrine, each clause pinned by a test:
//! - STATELESS per pass: desired comes from the agent snapshot, current from
//!   the live master (`PortForwarder::list_forwards`) — never a cache;
//! - discovery failure ⇒ KEEP current forwards (never cancel on error);
//! - agent disconnect ⇒ KEEP forwards; reconnect's snapshot reconverges.
//!
//! Those behaviors live in the daemon loop (supervisor); this module is the
//! pure diff so it can be exhaustively unit-tested.
//!
//! v2 change: forwards are (local, remote) pairs via a mapping function
//! (see `portmap`), which PREFERS the identity mapping (local == remote) so
//! `Host`/`Origin` reach the service unchanged, and falls back to translated
//! ports only when the identity port cannot be bound.

use std::collections::{BTreeSet, HashMap};
use std::time::{Duration, Instant};

use portal_transport::lsof::LsofPorts;
use portal_transport::{ForwardSpec, PortForwarder, Transport, TransportError};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::agentclient::Event;
use crate::pins::PinSet;
use crate::portmap::{PortMap, identity_eligible};

/// How many local ports one remote listener may be offered in a single pass:
/// the three tiers of [`crate::portmap`] (identity, indexed slot, allocator).
const MAX_BIND_ATTEMPTS: usize = 3;

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

/// Deduped conflict notes: a
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
    /// Explicitly requested remote ports (callback URLs), unioned into the
    /// desired set so a pass cannot cancel a forward the user is about to
    /// use. See [`crate::pins`].
    pub pins: PinSet,
}

/// ONE stateless pass (port of Engine.Reconcile):
/// 1. ensure + health — rebuild the master if down, then re-derive from
///    ground truth;
/// 2. desired: the agent snapshot UNION live pins ([`crate::pins`]) — a
///    pinned port is wanted even if no snapshot lists it;
/// 3. current: `list_forwards` — the transport's live view, never a cache;
/// 4. plan + apply: add desired−current (skipping local conflicts), cancel
///    current−desired. Forward failures log and continue — next pass retries.
///
/// A `None` snapshot still means AgentNotReady (keep forwards, change
/// nothing): with no observed listeners there is no way to tell a vanished
/// listener from an unobserved one, and tearing down on that ambiguity is the
/// failure mode the whole design avoids.
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

    let observed = desired.ok_or(ReconcileError::AgentNotReady)?;
    for port in st.pins.expire(Instant::now()) {
        tracing::debug!(target: "portal::engine", box_name = %st.box_name, remote = port,
            "callback pin expired (never confirmed by the agent)");
    }
    // Observation-driven retirement: pinned ports ride the Subscribe
    // allowlist, so a live callback listener IS in `observed`. Once seen,
    // its later absence means the listener exited — drop the pin (and with
    // it the forward) now instead of idling out the TTL.
    let listening: BTreeSet<u16> = observed.iter().copied().collect();
    for port in st.pins.observe(&listening) {
        tracing::info!(target: "portal::engine", box_name = %st.box_name, remote = port,
            "callback listener gone; retiring its forward");
    }
    // Union, deduped: a pinned port that IS in the snapshot must appear once,
    // or plan() would emit a duplicate add. Sorted for allocation determinism:
    // identity claims are first-come within a pass, so a stable order means a
    // stable mapping table across restarts.
    let desired: Vec<u16> = if st.pins.is_empty() {
        listening.iter().copied().collect()
    } else {
        listening
            .iter()
            .copied()
            .chain(st.pins.ports())
            .collect::<BTreeSet<u16>>()
            .into_iter()
            .collect()
    };
    let current = forwarder.list_forwards().await.unwrap_or_default();

    // UPGRADE PASS: a forward stuck on a translated port is a broken forward
    // for anything that validates Host/Origin, and the conflict that caused it
    // is usually gone long before the daemon is. Re-probe each degraded port;
    // when identity is free again, drop the assignment so plan() below cancels
    // the translated forward and re-adds it at identity, unattended.
    for (remote, was_local) in st.portmap.degraded() {
        if !desired.contains(&remote) || taken.contains(&remote) {
            continue;
        }
        if local.holder(remote).await.is_some() {
            continue;
        }
        st.portmap.release(remote);
        taken.remove(&was_local);
        st.conflicts.clear(was_local);
        tracing::info!(target: "portal::engine", box_name = %st.box_name,
            remote, was_local,
            "localhost:{} came free; reclaiming the same-port mapping", remote);
    }

    let plan = {
        let portmap = &mut st.portmap;
        plan(&desired, &current, |remote| {
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
        // A bind is the only way to discover a Mac-side holder: `taken` knows
        // only about ports portal itself claimed. On refusal, walk down the
        // preference order (identity → indexed → allocator) IN THIS PASS —
        // waiting for the next event would leave the service unreachable for
        // up to a safety interval.
        let mut spec = spec;
        for attempt in 0..MAX_BIND_ATTEMPTS {
            match forwarder.forward(spec).await {
                Ok(()) => {
                    taken.insert(spec.local);
                    st.conflicts.clear(spec.local);
                    tracing::info!(target: "portal::engine", box_name = %st.box_name,
                        "forwarded localhost:{} -> {}:{}", spec.local,
                        transport.describe().host, spec.remote);
                    if spec.local != spec.remote && identity_eligible(spec.remote) {
                        tracing::warn!(target: "portal::engine", box_name = %st.box_name,
                            local = spec.local, remote = spec.remote,
                            "forwarded on a TRANSLATED port: localhost:{} is held on the Mac. \
                             Services that validate Host/Origin (MCP servers, Vite, Django) may \
                             reject requests through localhost:{} — free the port to get the \
                             same-port mapping back", spec.remote, spec.local);
                    }
                    summary.added.push(spec);
                    break;
                }
                Err(TransportError::PortInUse { .. }) => {
                    // The bind itself is the conflict signal; lsof/ps only
                    // name the holder for the (deduped) diagnostic.
                    let holder = local.holder(spec.local).await.unwrap_or(0);
                    if st.conflicts.note(spec.local, holder) {
                        let name = local.process_name(holder).await;
                        tracing::warn!(target: "portal::engine", box_name = %st.box_name,
                            local = spec.local, remote = spec.remote, holder, process = %name,
                            "local port in use; remapping");
                        summary.conflicts.push((spec.local, holder));
                    }
                    st.portmap.reject_local(spec.remote, spec.local);
                    let next = (attempt + 1 < MAX_BIND_ATTEMPTS)
                        .then(|| st.portmap.local_for(spec.remote, |p| taken.contains(&p)))
                        .flatten();
                    match next {
                        Some(local) => spec = ForwardSpec { local, ..spec },
                        None => {
                            tracing::warn!(target: "portal::engine", box_name = %st.box_name,
                                remote = spec.remote,
                                "no bindable local port for remote listener");
                            summary.unmappable.push(spec.remote);
                            break;
                        }
                    }
                }
                Err(err) => {
                    tracing::warn!(target: "portal::engine", box_name = %st.box_name,
                        local = spec.local, remote = spec.remote, %err, "forward failed");
                    break;
                }
            }
        }
    }

    for spec in plan.remove {
        // A pass that MOVES a remote to a better local port emits both an add
        // and a remove for the same remote (identity reclaim, bind-refusal
        // fallback). Neither the live listener nor the fresh assignment may be
        // collateral damage of retiring the old spec.
        if summary.added.contains(&spec) {
            continue;
        }
        let _ = forwarder.cancel(spec).await;
        st.portmap.release_exact(spec.remote, spec.local);
        if !st.portmap.assignments().any(|(_, l)| l == spec.local) {
            taken.remove(&spec.local);
        }
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

    /// Guarantee a live forward for `remote` and return its local port.
    ///
    /// Used by the callback-URL relay, which must not open a URL until the
    /// port behind it actually answers on the Mac. Unlike [`reconcile`] this
    /// is a targeted, idempotent request: it pins `remote` (see
    /// [`crate::pins`]) so the next pass cannot cancel it, reuses the existing
    /// mapping when one is live, and returns `None` only if no local port
    /// could be established.
    async fn ensure_forward(&mut self, remote: u16) -> Option<u16>;

    /// Earliest moment a pin lapses, so the loop can wake exactly then and
    /// collect it. `None` means nothing is pinned and no timer is needed.
    fn next_pin_deadline(&self) -> Option<tokio::time::Instant> {
        None
    }
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

/// Turn a box-side URL into one that works from the Mac.
///
/// Two independent jobs, matching the two ways a callback port travels:
///
/// 1. EMBEDDED redirect targets (`redirect_uri=http://127.0.0.1:p/...` in a
///    public authorize URL — the aws-sso/gcloud/gh shape): forward `p`
///    SAME-PORT before the browser opens. The provider redirects to that
///    literal port after login; a translated port cannot help, so failure to
///    get the identity mapping is logged at ERROR while the (public, valid)
///    URL still opens — matching v1, where a same-port conflict logged and
///    the flow broke visibly at the redirect.
/// 2. The URL ITSELF loopback: establish a forward and rewrite to its local
///    end. Here failure is fail-closed (`Err`): opening the un-forwarded URL
///    verbatim would point the browser at a dead or WRONG local service.
async fn resolve_open_url(raw: &str, r: &mut dyn Reconciler) -> Result<String, OpenUrlError> {
    let classified = crate::callback::classify(raw);

    let top_level_port = match &classified.target {
        crate::callback::Target::Loopback { remote_port, .. } => Some(*remote_port),
        crate::callback::Target::AsIs(_) => None,
    };
    for &port in &classified.embedded_callback_ports {
        if Some(port) == top_level_port {
            continue; // the target arm below owns it
        }
        match tokio::time::timeout(ENSURE_FORWARD_TIMEOUT, r.ensure_forward(port))
            .await
            .unwrap_or(None)
        {
            Some(local) if local == port => {
                tracing::info!(target: "portal::engine", port,
                    "same-port forward ready for oauth redirect target");
            }
            Some(local) => tracing::error!(target: "portal::engine", port, local,
                "oauth redirect target could not get its identity port — the provider will \
                 redirect to port {port} and the login will not complete (something on the Mac \
                 is already bound there)"),
            None => tracing::error!(target: "portal::engine", port,
                "no forward for oauth redirect target — the post-login redirect will fail"),
        }
    }

    match classified.target {
        crate::callback::Target::AsIs(u) => Ok(u),
        crate::callback::Target::Loopback { url, remote_port } => {
            // Bounded: this runs on the reconcile loop task, so an unbounded
            // await would stall every pass behind a wedged SSH channel.
            let forwarded =
                tokio::time::timeout(ENSURE_FORWARD_TIMEOUT, r.ensure_forward(remote_port))
                    .await
                    .unwrap_or(None);
            match forwarded {
                Some(local) => {
                    let out = crate::callback::rewrite(&url, local);
                    tracing::info!(target: "portal::engine", remote = remote_port, local,
                        "callback url mapped to local forward");
                    Ok(out)
                }
                None => Err(OpenUrlError { remote_port }),
            }
        }
    }
}

/// Upper bound on establishing a callback forward. Generous relative to a
/// healthy round-trip (tens of ms) but far below a human's patience, and it
/// guarantees the reconcile loop cannot be parked indefinitely.
const ENSURE_FORWARD_TIMEOUT: Duration = Duration::from_secs(5);

/// A box-side loopback URL that could not be given a working local port.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OpenUrlError {
    pub remote_port: u16,
}

impl std::fmt::Display for OpenUrlError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "could not forward box port {} for a callback URL",
            self.remote_port
        )
    }
}

impl std::error::Error for OpenUrlError {}

/// Event-driven reconcile loop (port of Engine.runEventDriven):
/// - one pass immediately (forwards restore before the first agent event);
/// - Connected / SnapshotReplaced / Delta ⇒ debounced pass;
/// - Disconnected ⇒ log and KEEP forwards (reconnect reconverges);
/// - OpenUrl ⇒ port-translated, then forwarded to `open_url` (not a reconcile
///   trigger; see [`resolve_open_url`]);
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
        // Wake on the earliest pin lapse so a TTL is a real deadline and not
        // just "whenever something else happens to fire". Read before the
        // select so no borrow of `r` is held across arms that need it mutably;
        // recomputed each iteration because ensure_forward can add or extend
        // pins at any time.
        let pin_at = r.next_pin_deadline();
        let pin_sleep = async {
            match pin_at {
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
                            // Await here: establishing the forward BEFORE the
                            // browser opens is the entire point. Bounded by
                            // ENSURE_FORWARD_TIMEOUT so a wedged channel cannot
                            // park the loop.
                            match resolve_open_url(&url, r).await {
                                Ok(target) => { let _ = sink.try_send(target); }
                                Err(e) => tracing::error!(target: "portal::engine",
                                    remote = e.remote_port, url = %url,
                                    "{e}; not opening a browser at a port that would answer \
                                     with the wrong service or nothing at all"),
                            }
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
            _ = pin_sleep => {
                // A pin lapsed: reconcile drops it from the desired set and
                // the plan retires the forward.
                r.reconcile().await;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

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
            pins: PinSet::new(),
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
        assert_eq!(s.added, vec![fs(3000, 3000), fs(8000, 8000)]);
        assert!(taken.contains(&3000) && taken.contains(&8000));

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert!(s.added.is_empty());
        assert_eq!(s.removed, vec![fs(3000, 3000)]);
        assert!(!taken.contains(&3000));
        assert_eq!(
            fwd.forwards
                .lock()
                .unwrap()
                .iter()
                .copied()
                .collect::<Vec<_>>(),
            vec![fs(8000, 8000)]
        );
    }

    #[tokio::test]
    async fn agent_not_ready_keeps_forwards() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.forwards.lock().unwrap().insert(fs(8000, 8000));
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

    /// The header-correctness path: when the identity port is held on the Mac,
    /// the pass must land the forward somewhere else IMMEDIATELY rather than
    /// leaving the service unreachable until the next event.
    #[tokio::test]
    async fn identity_conflict_remaps_within_the_pass() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.busy_locals.lock().unwrap().insert(6274); // a Mac-side holder
        let local = FakeLocal {
            holders: HashMap::from([(6274, 999)]),
        };
        let mut st = state();
        let mut taken = BTreeSet::new();

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[6274]))
            .await
            .unwrap();
        assert_eq!(
            s.added,
            vec![fs(16274, 6274)],
            "must fall back, not give up"
        );
        assert_eq!(s.conflicts, vec![(6274, 999)]);
        assert!(taken.contains(&16274) && !taken.contains(&6274));

        // Stable next pass: no churn back onto the busy identity port.
        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[6274]))
            .await
            .unwrap();
        assert_eq!(s, PassSummary::default());
    }

    /// The conflict that forced a translated port almost always outlives its
    /// cause: kill the squatter and the forward must climb BACK to identity on
    /// its own, or every Host/Origin-checking service stays broken for the life
    /// of the daemon.
    #[tokio::test]
    async fn a_freed_identity_port_is_reclaimed_next_pass() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.busy_locals.lock().unwrap().insert(6277);
        let local = FakeLocal {
            holders: HashMap::from([(6277, 999)]),
        };
        let mut st = state();
        let mut taken = BTreeSet::new();

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[6277]))
            .await
            .unwrap();
        assert_eq!(s.added, vec![fs(16277, 6277)]);

        // The squatter exits: the bind would now succeed and lsof sees nobody.
        fwd.busy_locals.lock().unwrap().remove(&6277);
        let local = FakeLocal::default();

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[6277]))
            .await
            .unwrap();
        assert_eq!(s.added, vec![fs(6277, 6277)], "must climb back to identity");
        assert_eq!(s.removed, vec![fs(16277, 6277)], "and retire the detour");
        assert_eq!(
            fwd.list_forwards().await.unwrap(),
            vec![fs(6277, 6277)],
            "exactly one listener survives the swap"
        );
        assert!(taken.contains(&6277) && !taken.contains(&16277));

        // Converged: identity is not re-probed, so no churn.
        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[6277]))
            .await
            .unwrap();
        assert_eq!(s, PassSummary::default());
    }

    #[tokio::test]
    async fn bind_conflicts_walk_the_preference_order_then_report() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        // Identity, indexed slot AND the deterministic port are all held: the
        // pass exhausts its attempts and reports instead of looping forever.
        let mut st = state();
        let mut probe = PortMap::new("devbox1", 1);
        probe.reject_local(8000, 8000);
        probe.reject_local(8000, 18000);
        let fallback = probe.local_for(8000, |_| false).unwrap();
        for p in [8000, 18000, fallback] {
            fwd.busy_locals.lock().unwrap().insert(p);
        }
        let local = FakeLocal {
            holders: HashMap::from([(8000, 111), (18000, 222), (fallback, 333)]),
        };
        let mut taken = BTreeSet::new();

        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert_eq!(
            s.conflicts,
            vec![(8000, 111), (18000, 222), (fallback, 333)]
        );
        assert!(s.added.is_empty());
        assert_eq!(s.unmappable, vec![8000], "exhaustion must be reported");
        assert!(fwd.forwards.lock().unwrap().is_empty());

        // Next pass keeps probing the allocator range rather than re-reporting
        // the same three holders: refusals are remembered, so it makes progress.
        let s = reconcile_once(&*t, &fwd, &local, &mut st, &mut taken, Some(&[8000]))
            .await
            .unwrap();
        assert!(s.conflicts.is_empty(), "conflict notes must dedupe");
        let landed = s.added.first().copied().expect("a later probe must land");
        assert_eq!(landed.remote, 8000);
        assert!(crate::portmap::FALLBACK_RANGE.contains(&landed.local));
    }

    #[tokio::test]
    async fn generic_forward_failure_logs_and_continues() {
        let t = FakeTransport::new("devbox1");
        let fwd = FakeForwarder::default();
        fwd.fail_locals.lock().unwrap().insert(8000);
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
        // 8000's forward failed (not a conflict, so no remap), 3000's landed.
        assert_eq!(s.added, vec![fs(3000, 3000)]);
        assert!(s.conflicts.is_empty());
    }

    #[test]
    fn conflict_notes_dedupe_until_the_holder_changes() {
        let mut c = ConflictSet::default();
        assert!(c.note(8000, 111));
        assert!(!c.note(8000, 111));
        assert!(c.note(8000, 222), "a new holder is news again");
        c.clear(8000);
        assert!(c.note(8000, 222));
        c.prune(&[18000]);
        assert!(c.note(8000, 222), "pruned note is forgotten");
    }

    // ---- run_reconcile_loop ----

    /// Counts passes; `ensure_forward` maps remote → remote+10000 (or refuses
    /// when `forward_ok` is false) and records what it was asked for.
    struct Counting {
        passes: Arc<AtomicUsize>,
        asked: Arc<Mutex<Vec<u16>>>,
        forward_ok: bool,
        /// true = identity mapping (local == remote, the OAuth case);
        /// false = translated (remote + 10000, exercises the rewrite path).
        exact: bool,
        pin_deadline: Option<tokio::time::Instant>,
    }

    impl Counting {
        fn new(passes: Arc<AtomicUsize>) -> Self {
            Self {
                passes,
                asked: Arc::new(Mutex::new(Vec::new())),
                forward_ok: true,
                exact: false,
                pin_deadline: None,
            }
        }
    }

    #[async_trait::async_trait]
    impl Reconciler for Counting {
        fn next_pin_deadline(&self) -> Option<tokio::time::Instant> {
            self.pin_deadline
        }
        async fn reconcile(&mut self) {
            self.passes.fetch_add(1, Ordering::SeqCst);
        }
        async fn ensure_forward(&mut self, remote: u16) -> Option<u16> {
            self.asked.lock().unwrap().push(remote);
            self.forward_ok
                .then(|| if self.exact { remote } else { remote + 10000 })
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
                let mut r = Counting::new(n);
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
        let asked = Arc::new(Mutex::new(Vec::new()));

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            let asked = asked.clone();
            async move {
                let mut r = Counting::new(n);
                r.asked = asked;
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
        // Public URL: opened untouched, and no forward requested.
        assert_eq!(urx.try_recv().unwrap(), "https://example.com");
        assert!(asked.lock().unwrap().is_empty());
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

    /// The bug this whole path exists for: a box-side loopback callback URL
    /// must be forwarded and REWRITTEN to the local port, never opened with
    /// the box's port number.
    #[tokio::test(start_paused = true)]
    async fn loop_translates_loopback_callback_url() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let (utx, mut urx) = mpsc::channel::<String>(4);
        let cancel = CancellationToken::new();
        let asked = Arc::new(Mutex::new(Vec::new()));

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            let asked = asked.clone();
            async move {
                let mut r = Counting::new(n);
                r.asked = asked;
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
            url: "http://localhost:53219/callback?code=xyz".into(),
        })
        .await
        .unwrap();
        settle().await;

        assert_eq!(
            urx.try_recv().unwrap(),
            "http://127.0.0.1:63219/callback?code=xyz",
            "must open the LOCAL port, preserving the query"
        );
        assert_eq!(
            *asked.lock().unwrap(),
            vec![53219],
            "must have requested a forward for the box's port"
        );

        cancel.cancel();
        task.await.unwrap();
    }

    /// A loopback URL we could not forward must NOT reach the browser. The
    /// local port is either dead (connection refused) or bound by an unrelated
    /// service, and handing an OAuth `code` to the wrong listener is a real
    /// failure, not a cosmetic one. Fail closed and log.
    #[tokio::test(start_paused = true)]
    async fn loop_refuses_to_open_unmappable_loopback_url() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let (utx, mut urx) = mpsc::channel::<String>(4);
        let cancel = CancellationToken::new();

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            async move {
                let mut r = Counting::new(n);
                r.forward_ok = false;
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
            url: "http://localhost:53219/callback".into(),
        })
        .await
        .unwrap();
        settle().await;
        assert!(
            urx.try_recv().is_err(),
            "must not open a loopback URL whose port was never forwarded"
        );

        cancel.cancel();
        task.await.unwrap();
    }

    /// Non-loopback URLs need no forward, so a refusing reconciler must not
    /// suppress them — only loopback translation can fail closed.
    #[tokio::test(start_paused = true)]
    async fn loop_still_opens_remote_urls_when_forwarding_is_unavailable() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let (utx, mut urx) = mpsc::channel::<String>(4);
        let cancel = CancellationToken::new();

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            async move {
                let mut r = Counting::new(n);
                r.forward_ok = false;
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
            url: "https://github.com/login/device".into(),
        })
        .await
        .unwrap();
        settle().await;
        assert_eq!(
            urx.try_recv().unwrap(),
            "https://github.com/login/device",
            "remote URLs are already correct and always open"
        );

        cancel.cancel();
        task.await.unwrap();
    }

    /// The aws-sso shape: PUBLIC authorize URL carrying a loopback
    /// redirect_uri. The URL must open UNCHANGED (the provider page is
    /// correct as-is) while the embedded port gets a same-port forward — the
    /// provider will redirect the browser to that literal port after login.
    #[tokio::test(start_paused = true)]
    async fn loop_forwards_embedded_redirect_target_same_port() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let (utx, mut urx) = mpsc::channel::<String>(4);
        let cancel = CancellationToken::new();
        let asked = Arc::new(Mutex::new(Vec::new()));

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            let asked = asked.clone();
            async move {
                let mut r = Counting::new(n);
                r.asked = asked;
                r.exact = true; // identity mapping succeeds
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

        let raw = "https://oidc.us-east-1.amazonaws.com/authorize?client_id=abc&\
                   redirect_uri=http%3A%2F%2F127.0.0.1%3A55555%2Foauth%2Fcallback&state=xyz";
        etx.send(Event::OpenUrl { url: raw.into() }).await.unwrap();
        settle().await;

        assert_eq!(
            urx.try_recv().unwrap(),
            raw,
            "public authorize URL must open byte-identical"
        );
        assert_eq!(
            *asked.lock().unwrap(),
            vec![55555],
            "embedded redirect target must get a forward"
        );

        cancel.cancel();
        task.await.unwrap();
    }

    /// Same shape, but the identity port is unavailable: the public URL must
    /// STILL open (it is valid; only the eventual redirect is doomed, which
    /// is logged) — unlike the top-level-loopback case, which fails closed.
    #[tokio::test(start_paused = true)]
    async fn loop_opens_public_url_even_when_redirect_target_unforwardable() {
        let n = Arc::new(AtomicUsize::new(0));
        let (etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let (utx, mut urx) = mpsc::channel::<String>(4);
        let cancel = CancellationToken::new();

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            async move {
                let mut r = Counting::new(n);
                r.forward_ok = false;
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

        let raw = "https://p.example/auth?redirect_uri=http%3A%2F%2F127.0.0.1%3A55555%2Fcb";
        etx.send(Event::OpenUrl { url: raw.into() }).await.unwrap();
        settle().await;
        assert_eq!(urx.try_recv().unwrap(), raw);

        cancel.cancel();
        task.await.unwrap();
    }

    /// A lapsed pin must be collected when it actually expires, not whenever
    /// the next unrelated event happens to arrive. The loop sleeps on
    /// `next_pin_deadline`, so with no deltas, no kicks, and the 60s safety
    /// tick still far off, a 30s deadline alone must drive a pass.
    #[tokio::test(start_paused = true)]
    async fn loop_wakes_on_pin_deadline() {
        let n = Arc::new(AtomicUsize::new(0));
        let (_etx, mut erx) = mpsc::channel::<Event>(4);
        let (_ktx, mut krx) = mpsc::channel::<()>(1);
        let cancel = CancellationToken::new();
        let deadline = tokio::time::Instant::now() + Duration::from_secs(30);

        let task = tokio::spawn({
            let n = n.clone();
            let cancel = cancel.clone();
            async move {
                let mut r = Counting::new(n);
                r.pin_deadline = Some(deadline);
                run_reconcile_loop(
                    &mut erx,
                    &mut krx,
                    None,
                    LoopConfig::default(),
                    cancel,
                    &mut r,
                )
                .await;
            }
        });
        settle().await;
        assert_eq!(n.load(Ordering::SeqCst), 1, "initial pass only");

        tokio::time::advance(Duration::from_secs(20)).await;
        settle().await;
        assert_eq!(
            n.load(Ordering::SeqCst),
            1,
            "no wakeup before the deadline (safety tick is 60s out)"
        );

        tokio::time::advance(Duration::from_secs(11)).await;
        settle().await;
        assert!(
            n.load(Ordering::SeqCst) >= 2,
            "pin deadline must drive a reconcile pass on its own"
        );

        cancel.cancel();
        task.await.unwrap();
    }
}
