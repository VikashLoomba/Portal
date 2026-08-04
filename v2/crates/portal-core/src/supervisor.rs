//! The multi-box supervisor — the composition root that turns the tested
//! parts into a running daemon. One daemon process owns N independent
//! per-box stacks plus the shared pasteboard watcher:
//!
//! ```text
//! Supervisor
//! ├── pasteboard watcher task (ONE changeCount poller; fans WatchEvents
//! │     out to every box's publisher — DESIGN-clipsync §2.1)
//! ├── BoxStack "devbox1"  (index 1)
//! │   ├── NativeSsh (owns the connection; Dialer for forwards; exec for
//! │   │     bootstrap/blob-push)
//! │   ├── agent client task (bootstrap → handshake → shim deploy →
//! │   │     demux; reconnect with backoff)
//! │   ├── reconcile loop task (event-driven debounce + safety tick →
//! │   │     reconcile_once over the ListenerForwarder)
//! │   ├── clipsync publisher task (watch events + acks → updates/blob push)
//! │   └── notify/open-url handler task
//! └── BoxStack "gpu-box"   (index 2) …
//! ```
//!
//! Cross-box shared state:
//! - the LOCAL-PORT taken-set (portmap fallback probing must see all boxes'
//!   claims — two boxes' fallback allocations may never collide);
//! - the watcher broadcast (one pasteboard, N consumers).
//!
//! Failure isolation: every box task tree hangs off the box's OWN
//! CancellationToken (child of the daemon root). One box's ssh being down
//! never affects another box's stack.

use std::collections::BTreeSet;
use std::sync::{Arc, Mutex};

use tokio::sync::{broadcast, mpsc, watch};
use tokio_util::sync::CancellationToken;

use portal_clip::watcher::{Gates, POLL_INTERVAL, SnapshotSource, WatchEvent, Watcher};
use portal_transport::lsof::LsofPorts;
use portal_transport::runner::OsRunner;
use portal_transport::{PortForwarder, Transport};

use crate::agentclient::session::{Bootstrapper, Client, ClientConfig, Filter, Outbound};
use crate::agentclient::{Event, EventChannels, ServiceRequest, SnapshotCache};
use crate::bootstrap::{EmbeddedAgent, Manager, REMOTE_DIR};
use crate::clipsync::{ExecBlobPusher, Publisher};
use crate::config::{BoxConfig, Config, DEFAULT_DENY_PORTS};
use crate::cred::CredHandler;
use crate::engine::{
    BoxState, ConflictSet, LoopConfig, Reconciler, reconcile_once, run_reconcile_loop,
};
use crate::portmap::PortMap;

/// A point-in-time status snapshot for one box (feeds `portal status`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BoxStatus {
    pub name: String,
    pub host: String,
    pub index: u8,
    pub connected: bool,
    pub agent_sha: Option<String>,
    /// (local, remote) pairs currently forwarded.
    pub forwards: Vec<(u16, u16)>,
    pub clipsync_synced: bool,
    pub clipsync_change_id: u64,
}

/// Handle to one running per-box stack.
pub struct BoxStack {
    pub cfg: BoxConfig,
    cancel: CancellationToken,
    tasks: Vec<tokio::task::JoinHandle<()>>,
    status: watch::Receiver<BoxStatus>,
    /// Held for the stack's lifetime: dropping it would kill live filter
    /// updates; publish a new Filter here to re-Subscribe (allow/unallow).
    filter_tx: watch::Sender<Filter>,
}

impl BoxStack {
    pub fn status(&self) -> BoxStatus {
        self.status.borrow().clone()
    }

    /// Update the port filter live (re-sends Subscribe on the live session).
    pub fn set_filter(&self, filter: Filter) {
        let _ = self.filter_tx.send(filter);
    }

    /// Re-derive the filter from a (possibly updated) box config.
    pub fn apply_config(&self, cfg: &BoxConfig) {
        self.set_filter(filter_for(cfg));
    }

    /// Tear the stack down (cancels every task; forwards drop with the
    /// listeners — by design, restart re-derives from the snapshot).
    pub async fn shutdown(self) {
        self.cancel.cancel();
        for t in self.tasks {
            let _ = t.await;
        }
    }
}

/// Everything a supervisor needs beyond the config. The watcher source and
/// gates are injected so the whole composition is testable off-Mac.
#[derive(Clone)]
pub struct Deps {
    pub agent: EmbeddedAgent,
    /// Feature-gate lookup (production: config-dir feature files, live).
    pub gates: Arc<dyn Fn(&str) -> bool + Send + Sync>,
    /// Notification sink (production: macOS notification; tests: channel).
    pub notify: Arc<dyn Fn(NotifyEvent) + Send + Sync>,
    /// URL opener (production: `open <url>`; tests: channel).
    pub open_url: Arc<dyn Fn(String) + Send + Sync>,
    /// Transport factory (production: [`native_transport`]; tests: fakes).
    /// Returns (transport, forwarder) — usually the same object twice, but
    /// tests substitute an in-memory forwarder.
    pub transport: Arc<TransportFactory>,
    /// Credential serve dependencies (None = cred requests denied
    /// "gui-unavailable"; production wires the helper prompter, LAContext
    /// biometry, and the macOS keychain).
    pub cred: Option<Arc<crate::cred::CredDeps>>,
    /// Pasteboard writer for the clip-write relay (None = writes denied
    /// "unavailable"; production: NativePasteboard).
    pub clipboard_writer: Option<Arc<dyn portal_clip::ClipboardWriter>>,
}

pub type TransportFactory =
    dyn Fn(&BoxConfig) -> (Arc<dyn Transport>, Arc<dyn PortForwarder>) + Send + Sync;

/// The production transport factory: one NativeSsh per box (connection
/// shared by the agent pipe, exec, and forward dials) + a ListenerForwarder
/// splicing onto its direct-tcpip channels.
pub fn native_transport(cfg: &BoxConfig) -> (Arc<dyn Transport>, Arc<dyn PortForwarder>) {
    let ssh = Arc::new(portal_transport::native_ssh::NativeSsh::new(
        cfg.host.clone(),
        Arc::new(OsRunner),
    ));
    let forwarder = Arc::new(portal_transport::forwarder::ListenerForwarder::new(
        ssh.clone(),
    ));
    (ssh, forwarder)
}

/// A box-attributed notification (v1 semantics + box name).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NotifyEvent {
    pub box_name: String,
    pub title: String,
    pub body: Option<String>,
    pub urgency: u8,
    /// Unverified events render with the "[unverified] " prefix (v1).
    pub verified: bool,
}

pub struct Supervisor {
    stacks: Vec<BoxStack>,
    /// Cross-box local-port claims (portmap fallback correctness).
    taken: Arc<Mutex<BTreeSet<u16>>>,
    clip_tx: broadcast::Sender<WatchEvent>,
    watcher_task: Option<tokio::task::JoinHandle<()>>,
    cancel: CancellationToken,
    deps: Option<Deps>,
}

impl Supervisor {
    /// Compose and start every enabled box stack plus the shared watcher.
    /// `watcher` is None on non-Mac hosts (or in tests that drive
    /// [`Supervisor::clip_sender`] directly).
    pub fn start<S, G>(
        config: &Config,
        deps: &Deps,
        watcher: Option<(S, G)>,
        cancel: CancellationToken,
    ) -> Self
    where
        S: SnapshotSource + 'static,
        G: Gates + 'static,
    {
        let taken = Arc::new(Mutex::new(BTreeSet::new()));
        let (clip_tx, _) = broadcast::channel::<WatchEvent>(16);
        let mut sup = Self {
            stacks: Vec::new(),
            taken,
            clip_tx,
            watcher_task: None,
            cancel: cancel.clone(),
            deps: None,
        };
        sup.start_watcher(watcher, cancel.clone());
        sup.stacks = config
            .enabled_boxes()
            .map(|b| {
                spawn_box_stack(
                    b.clone(),
                    deps,
                    sup.taken.clone(),
                    sup.clip_tx.clone(),
                    &cancel,
                )
            })
            .collect();
        sup.deps = Some(deps.clone());
        sup
    }

    fn start_watcher<S, G>(&mut self, watcher: Option<(S, G)>, cancel: CancellationToken)
    where
        S: SnapshotSource + 'static,
        G: Gates + 'static,
    {
        let Some((source, gates)) = watcher else {
            return;
        };
        let tx = self.clip_tx.clone();
        self.watcher_task = Some(tokio::spawn(async move {
            let mut w = Watcher::new(source, gates);
            let mut tick = tokio::time::interval(POLL_INTERVAL);
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    _ = tick.tick() => {
                        if let Some(ev) = w.poll() {
                            let _ = tx.send(ev);
                        }
                    }
                }
            }
        }));
    }

    /// Config hot-reload (multi-box polish): apply a fresh config to the
    /// running stacks. Same-name + same-host boxes keep their stacks
    /// (index/allow/deny changes apply live via `apply_config`); removed or
    /// host-changed boxes are torn down; new boxes spawn. Stays same-type
    /// (no watcher rebuild) by construction.
    pub async fn reconcile(&mut self, config: &Config) {
        let mut desired: Vec<BoxConfig> = config.enabled_boxes().cloned().collect();
        let mut i = 0;
        while i < self.stacks.len() {
            let current = &self.stacks[i].cfg;
            let pos = desired
                .iter()
                .position(|b| b.name == current.name && b.host == current.host);
            match pos {
                Some(idx) => {
                    let new_cfg = desired.swap_remove(idx);
                    self.stacks[i].apply_config(&new_cfg);
                    self.stacks[i].cfg = new_cfg;
                    i += 1;
                }
                None => {
                    tracing::info!(box_name = %current.name, "removing box stack (config hot-reload)");
                    let stack = self.stacks.remove(i);
                    stack.shutdown().await;
                }
            }
        }
        let Some(deps) = &self.deps else {
            return;
        };
        for b in desired {
            tracing::info!(box_name = %b.name, "spawning box stack (config hot-reload)");
            self.stacks.push(spawn_box_stack(
                b,
                deps,
                self.taken.clone(),
                self.clip_tx.clone(),
                &self.cancel,
            ));
        }
    }

    pub fn stacks(&self) -> &[BoxStack] {
        &self.stacks
    }

    pub fn status(&self) -> Vec<BoxStatus> {
        self.stacks.iter().map(|s| s.status()).collect()
    }

    /// Inject watch events without a platform watcher (tests, and the
    /// future `portal clip push` debugging verb).
    pub fn clip_sender(&self) -> broadcast::Sender<WatchEvent> {
        self.clip_tx.clone()
    }

    /// Cross-box taken-set (exposed for status/diagnostics).
    pub fn taken_ports(&self) -> Vec<u16> {
        self.taken.lock().unwrap().iter().copied().collect()
    }

    pub async fn shutdown(self) {
        self.cancel.cancel();
        if let Some(t) = self.watcher_task {
            let _ = t.await;
        }
        for s in self.stacks {
            s.shutdown().await;
        }
    }

    /// Cancel every task (stacks + watcher). Idempotent; the caller then
    /// awaits the JoinHandles it holds (from `BoxStack::shutdown` and the
    /// watcher task — both remain owned by their holder after this call).
    /// This is the `&self` path for Arc/Mutex-wrapped supervisors (tests).
    pub fn cancel_all(&self) {
        self.cancel.cancel();
    }
}

/// Deny/allow/exclude-ephemeral from a box config (defaults + per-box).
pub fn filter_for(cfg: &BoxConfig) -> Filter {
    let mut deny: Vec<u16> = DEFAULT_DENY_PORTS.to_vec();
    deny.extend(&cfg.deny);
    deny.sort_unstable();
    deny.dedup();
    Filter {
        deny,
        allow: cfg.allow.clone(),
        exclude_ephemeral: true,
    }
}

/// Compose one box's task tree. Everything hangs off a child token.
fn spawn_box_stack(
    cfg: BoxConfig,
    deps: &Deps,
    taken: Arc<Mutex<BTreeSet<u16>>>,
    clip_tx: broadcast::Sender<WatchEvent>,
    parent: &CancellationToken,
) -> BoxStack {
    let cancel = parent.child_token();
    let box_name = cfg.name.clone();
    let box_index = cfg.index;

    // Transport + forwarder from the injected factory.
    let runner = Arc::new(OsRunner);
    let (transport, forwarder) = (deps.transport)(&cfg);
    let lsof = LsofPorts::new(portal_transport::lsof::DEFAULT_LSOF_PATH, runner);
    let bootstrap = Arc::new(Manager::new(transport.clone(), deps.agent.clone()));

    // Filter: deny = defaults + per-box extras; allow = per-box allowlist.
    let filter = filter_for(&cfg);
    let (filter_tx, filter_rx) = watch::channel(filter);

    let (outbound_tx, outbound_rx) = mpsc::channel::<Outbound>(32);
    let channels = EventChannels::new();
    let sinks = channels.sinks.clone();
    let EventChannels {
        engine: mut engine_rx,
        clip: _clip_rx, // v1 pull-path requests: not advertised, never arrive
        clip_write: clip_write_rx,
        notify: mut notify_rx,
        cred: cred_rx,
        clipsync: mut clipsync_ack_rx,
        ..
    } = channels;

    let client_cfg = ClientConfig {
        box_name: box_name.clone(),
        ..ClientConfig::default()
    };
    let mut client = Client::new(
        transport.clone(),
        bootstrap.clone() as Arc<dyn Bootstrapper>,
        client_cfg,
        sinks,
        filter_rx,
        outbound_rx,
    );
    let snapshot: Arc<SnapshotCache> = client.snapshot.clone();
    let hello_ack = client.hello_ack.clone();

    // Status publishing: tasks update this snapshot as things change.
    let (status_tx, status_rx) = watch::channel(BoxStatus {
        name: box_name.clone(),
        host: cfg.host.clone(),
        index: cfg.index,
        connected: false,
        agent_sha: None,
        forwards: Vec::new(),
        clipsync_synced: false,
        clipsync_change_id: 0,
    });
    let status_tx = Arc::new(status_tx);

    let mut tasks = Vec::new();

    // ---- agent client task (reconnect loop) ----
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        async move {
            client.run(cancel).await;
        }
    }));

    // ---- engine event fan-out ----
    // The reconcile loop wants Connected/Snapshot/Delta/Disconnected; the
    // clipsync publisher additionally needs Connected (replay); notify/url
    // events ride their own channels. One small fan-out task keeps the
    // loop's receiver contract (mpsc) unchanged.
    let (engine_ev_tx, engine_ev_rx) = mpsc::channel::<Event>(64);
    let (pub_connected_tx, pub_connected_rx) = mpsc::channel::<()>(4);
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        let status_tx = status_tx.clone();
        let hello_ack = hello_ack.clone();
        async move {
            loop {
                let ev = tokio::select! {
                    _ = cancel.cancelled() => return,
                    ev = engine_rx.recv() => match ev { Some(e) => e, None => return },
                };
                match &ev {
                    Event::Connected => {
                        let sha = hello_ack
                            .lock()
                            .unwrap()
                            .as_ref()
                            .map(|a| a.agent_git_sha.clone());
                        status_tx.send_modify(|s| {
                            s.connected = true;
                            s.agent_sha = sha;
                        });
                        let _ = pub_connected_tx.try_send(());
                    }
                    Event::Disconnected { .. } => {
                        status_tx.send_modify(|s| s.connected = false);
                    }
                    _ => {}
                }
                if engine_ev_tx.send(ev).await.is_err() {
                    return;
                }
            }
        }
    }));

    // ---- reconcile loop task ----
    let (url_tx, mut url_rx) = mpsc::channel::<String>(4);
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        let status_tx = status_tx.clone();
        let transport = transport.clone();
        let forwarder = forwarder.clone();
        let box_name = box_name.clone();
        async move {
            let mut engine_ev_rx = engine_ev_rx;
            // Kick channel: reserved for the status socket's future
            // "reconcile now" verb; unused today.
            let (_kick_tx, mut kick_rx) = mpsc::channel::<()>(1);
            let mut reconciler = StackReconciler {
                transport,
                forwarder,
                lsof,
                snapshot,
                state: BoxState {
                    box_name: box_name.clone(),
                    portmap: PortMap::new(box_name, box_index),
                    conflicts: ConflictSet::default(),
                    skip_local: Vec::new(),
                },
                taken,
                status_tx,
            };
            run_reconcile_loop(
                &mut engine_ev_rx,
                &mut kick_rx,
                Some(url_tx),
                LoopConfig::default(),
                cancel,
                &mut reconciler,
            )
            .await;
        }
    }));

    // ---- clipsync publisher task ----
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        let status_tx = status_tx.clone();
        let box_name = box_name.clone();
        let mut clip_rx = clip_tx.subscribe();
        let mut connected_rx = pub_connected_rx;
        let outbound_tx = outbound_tx.clone();
        let pusher = ExecBlobPusher {
            transport: transport.clone(),
            portald_path: format!("{REMOTE_DIR}/portald"),
        };
        async move {
            let mut publisher = Publisher::new(box_name, outbound_tx, pusher);
            loop {
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    ev = clip_rx.recv() => match ev {
                        Ok(ev) => publisher.on_event(ev).await,
                        Err(broadcast::error::RecvError::Lagged(_)) => continue, // latest-wins anyway
                        Err(broadcast::error::RecvError::Closed) => return,
                    },
                    _ = connected_rx.recv() => publisher.on_connected().await,
                    ack = clipsync_ack_rx.recv() => match ack {
                        Some(ServiceRequest::ClipSyncAck(a)) => publisher.on_ack(a).await,
                        Some(_) => continue,
                        None => return,
                    },
                }
                status_tx.send_modify(|s| {
                    s.clipsync_synced = publisher.synced();
                    s.clipsync_change_id = publisher.current_change_id();
                });
            }
        }
    }));

    // ---- clipboard-write handler task (box → Mac) ----
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        let handler = crate::clipwrite::ClipWriteHandler {
            writer: deps.clipboard_writer.clone(),
            transport: transport.clone(),
            gates: deps.gates.clone(),
            notify: deps.notify.clone(),
            box_name: box_name.clone(),
            outbound: outbound_tx.clone(),
        };
        let mut clip_write_rx = clip_write_rx;
        async move { handler.run(&mut clip_write_rx, cancel).await }
    }));

    // ---- credential handler task ----
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        let handler = CredHandler {
            deps: deps.cred.clone(),
            gates: deps.gates.clone(),
            box_name: box_name.clone(),
            host: cfg.host.clone(),
            outbound: outbound_tx.clone(),
        };
        let mut cred_rx = cred_rx;
        async move { handler.run(&mut cred_rx, cancel).await }
    }));

    // ---- notify / open-url handler task ----
    tasks.push(tokio::spawn({
        let cancel = cancel.clone();
        let notify = deps.notify.clone();
        let open_url = deps.open_url.clone();
        let gates = deps.gates.clone();
        let box_name = box_name.clone();
        async move {
            loop {
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    r = notify_rx.recv() => match r {
                        Some(ServiceRequest::Notify { notify: n, .. }) => {
                            if gates("notify") {
                                notify(NotifyEvent {
                                    box_name: box_name.clone(),
                                    title: n.title,
                                    body: n.body,
                                    urgency: n.urgency.unwrap_or(0),
                                    verified: n.verified.unwrap_or(false),
                                });
                            }
                        }
                        Some(_) => {}
                        None => return,
                    },
                    // OpenUrl events: routed by the reconcile loop's sink
                    // (they are not reconcile triggers). No feature gate —
                    // v1 had none for xdg-open relay.
                    u = url_rx.recv() => match u {
                        Some(u) => open_url(u),
                        None => return,
                    },
                }
            }
        }
    }));

    BoxStack {
        cfg,
        cancel,
        tasks,
        status: status_rx,
        filter_tx,
    }
}

/// The per-pass driver: owns everything reconcile_once touches for one box.
struct StackReconciler {
    transport: Arc<dyn Transport>,
    forwarder: Arc<dyn PortForwarder>,
    lsof: LsofPorts,
    snapshot: Arc<SnapshotCache>,
    state: BoxState,
    taken: Arc<Mutex<BTreeSet<u16>>>,
    status_tx: Arc<watch::Sender<BoxStatus>>,
}

#[async_trait::async_trait]
impl Reconciler for StackReconciler {
    async fn reconcile(&mut self) {
        let desired = self.snapshot.desired_ports();
        // The cross-box taken-set is cloned for the pass and written back
        // afterwards: reconcile_once mutates it synchronously, and holding
        // the std mutex across awaits is forbidden. Two boxes reconciling
        // concurrently still cannot collide: their indexed ranges are
        // disjoint, and fallback double-allocation is caught by the LISTENER
        // BIND (PortInUse) — the taken-set is an optimization, the bind is
        // the guarantee.
        let mut taken = self.taken.lock().unwrap().clone();
        let result = reconcile_once(
            &*self.transport,
            &*self.forwarder,
            &self.lsof,
            &mut self.state,
            &mut taken,
            desired.as_deref(),
        )
        .await;
        *self.taken.lock().unwrap() = taken;
        match result {
            Ok(_summary) => {
                let forwards: Vec<(u16, u16)> = self
                    .state
                    .portmap
                    .assignments()
                    .map(|(r, l)| (l, r))
                    .collect();
                self.status_tx.send_modify(|s| s.forwards = forwards);
            }
            Err(err) => {
                tracing::debug!(target: "portal::supervisor",
                    box_name = %self.state.box_name, %err, "reconcile pass failed");
            }
        }
    }
}
