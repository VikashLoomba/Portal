//! One protocol session + the reconnect supervisor loop (port of
//! agentclient.runOnce / Run / demuxLoop).
//!
//! Structure differs deliberately from Go in two ways:
//! - the write side is single-owner: handlers enqueue [`Outbound`] frames on
//!   an mpsc handle instead of sharing a mutex-latched encoder (no
//!   `recover()`-on-closed-channel hack — ownership makes it unrepresentable);
//! - frames are read by a dedicated task feeding a channel, because
//!   `read_exact` is not cancel-safe inside `select!` (Go used a reader
//!   goroutine for the same reason).

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use portal_proto::PROTO_VERSION;
use portal_proto::codec::CodecError;
use portal_proto::codec::asynchronous::{read_frame, write_frame};
use portal_proto::envelope::Envelope;
use portal_proto::messages::{
    ClipResponse, ClipSyncClear, ClipSyncUpdate, ClipWriteResponse, CredResponse, Hello, HelloAck,
    Msg, OpenUrl, Subscribe, marshal_payload,
};
use portal_transport::{Transport, TransportError};
use tokio::io::{AsyncBufReadExt, AsyncWrite, BufReader};
use tokio::sync::{mpsc, watch};
use tokio_util::sync::CancellationToken;

use super::{Event, EventSinks, ServiceRequest, SnapshotCache};

/// Owns this product's agent upload + embedded SHA (port of Bootstrapper).
#[async_trait::async_trait]
pub trait Bootstrapper: Send + Sync {
    /// Idempotent: probe, upload if missing/mismatched, return the remote path.
    async fn ensure_uploaded(&self) -> Result<String, String>;
    fn embedded_sha(&self) -> String;
    fn set_boot_id(&self, id: &str);

    /// Idempotent convergence of EVERYTHING this product installs box-side:
    /// the agent binary, the stable `portald` symlink, and the PATH shims.
    ///
    /// Grouped behind one call because they form a single dependency chain,
    /// and the v1→v2 upgrade failure ran straight down it: a probe hit
    /// returned early without converging the `portald` symlink, so the box had
    /// the binary but no stable path — `portald missing on the box`. Shim
    /// convergence runs only after a successful HelloAck, so a box that could
    /// not answer left the shims at their old version too, which is why
    /// `doctor` reported BOTH `outdated shim` and a missing portald from what
    /// is really one defect.
    async fn ensure_box_converged(&self) -> Result<(), String>;
}

#[derive(Debug, Clone)]
pub struct ClientConfig {
    pub heartbeat_timeout: Duration,
    pub coalesce_window: Duration,
    pub reconnect_min: Duration,
    pub reconnect_max: Duration,
    /// A session older than this resets backoff to reconnect_min — a short
    /// flap cluster must not pin a blip 6 hours later at reconnect_max.
    pub healthy_threshold: Duration,
    /// Box name advertised in Hello (box-attributed agent logs). Empty =
    /// not advertised.
    pub box_name: String,
}

impl Default for ClientConfig {
    fn default() -> Self {
        Self {
            heartbeat_timeout: Duration::from_secs(12),
            coalesce_window: Duration::from_millis(50),
            reconnect_min: Duration::from_millis(500),
            reconnect_max: Duration::from_secs(10),
            healthy_threshold: Duration::from_secs(5),
            box_name: String::new(),
        }
    }
}

/// Subscribe filter; publish a new value on the watch channel and the live
/// session re-sends Subscribe with a fresh rsid (allow/unallow propagation).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Filter {
    pub deny: Vec<u16>,
    pub allow: Vec<u16>,
    pub exclude_ephemeral: bool,
}

/// A client→agent service frame queued by a handler (clip/cred/… responses).
#[derive(Debug)]
pub struct Outbound {
    pub service: &'static str,
    pub kind: &'static str,
    pub payload: ciborium::Value,
}

impl Outbound {
    pub fn clip_response(resp: &ClipResponse) -> Result<Self, String> {
        Ok(Self {
            service: "clip",
            kind: "resp",
            payload: marshal_payload(resp).map_err(|e| e.to_string())?,
        })
    }
    pub fn clip_write_response(resp: &ClipWriteResponse) -> Result<Self, String> {
        Ok(Self {
            service: "clipwrite",
            kind: "resp",
            payload: marshal_payload(resp).map_err(|e| e.to_string())?,
        })
    }
    pub fn cred_response(resp: &CredResponse) -> Result<Self, String> {
        Ok(Self {
            service: "cred",
            kind: "resp",
            payload: marshal_payload(resp).map_err(|e| e.to_string())?,
        })
    }
    pub fn clipsync_update(update: &ClipSyncUpdate) -> Result<Self, String> {
        Ok(Self {
            service: "clipsync",
            kind: "update",
            payload: marshal_payload(update).map_err(|e| e.to_string())?,
        })
    }
    pub fn clipsync_clear(clear: &ClipSyncClear) -> Result<Self, String> {
        Ok(Self {
            service: "clipsync",
            kind: "clear",
            payload: marshal_payload(clear).map_err(|e| e.to_string())?,
        })
    }
}

#[derive(Debug, thiserror::Error)]
pub enum SessionError {
    #[error("bootstrap: {0}")]
    Bootstrap(String),
    #[error("stream: {0}")]
    Stream(#[from] TransportError),
    #[error("codec: {0}")]
    Codec(#[from] CodecError),
    #[error("expected HelloAck, got another frame")]
    NotHelloAck,
    #[error("agent error {code}: {msg}")]
    Agent { code: u16, msg: String },
    #[error("agent SHA mismatch: agent={agent} embedded={embedded}")]
    ShaMismatch { agent: String, embedded: String },
    #[error("heartbeat timeout")]
    HeartbeatTimeout,
    #[error("agent said Bye")]
    Bye,
    #[error("stream closed")]
    Eof,
}

pub struct Client {
    pub transport: Arc<dyn Transport>,
    pub bootstrap: Arc<dyn Bootstrapper>,
    pub cfg: ClientConfig,
    pub sinks: EventSinks,
    pub snapshot: Arc<SnapshotCache>,
    filter: watch::Receiver<Filter>,
    outbound: mpsc::Receiver<Outbound>,
    /// Monotonic across sessions (Subscribe replay ordering).
    rsid: AtomicU64,
    /// Msg.seq stamp — log correlation only, never the port-event seq.
    msg_seq: AtomicU64,
    /// Latched last HelloAck for status rendering (Arc: the supervisor keeps
    /// a handle after moving the Client into its task).
    pub hello_ack: Arc<std::sync::Mutex<Option<HelloAck>>>,
}

impl Client {
    pub fn new(
        transport: Arc<dyn Transport>,
        bootstrap: Arc<dyn Bootstrapper>,
        cfg: ClientConfig,
        sinks: EventSinks,
        filter: watch::Receiver<Filter>,
        outbound: mpsc::Receiver<Outbound>,
    ) -> Self {
        Self {
            transport,
            bootstrap,
            cfg,
            sinks,
            snapshot: Arc::new(SnapshotCache::default()),
            filter,
            outbound,
            rsid: AtomicU64::new(0),
            msg_seq: AtomicU64::new(0),
            hello_ack: Arc::new(std::sync::Mutex::new(None)),
        }
    }

    /// Supervisor loop: connect, run one session until it dies, backoff,
    /// reconnect. Returns only on cancellation.
    pub async fn run(&mut self, cancel: CancellationToken) {
        let mut backoff = self.cfg.reconnect_min;
        loop {
            if cancel.is_cancelled() {
                return;
            }
            let started = tokio::time::Instant::now();
            let err = tokio::select! {
                _ = cancel.cancelled() => return,
                r = self.run_once() => r.err(),
            };
            let session_dur = started.elapsed();
            let err_str = err.as_ref().map(|e| e.to_string());
            tracing::warn!(target: "portal::agent", host = %self.transport.describe().host,
                err = err_str.as_deref().unwrap_or("clean"), ?session_dur, "agent session ended");
            let _ = self
                .sinks
                .engine
                .try_send(Event::Disconnected { error: err_str });

            if session_dur >= self.cfg.healthy_threshold {
                backoff = self.cfg.reconnect_min;
            }
            tokio::select! {
                _ = cancel.cancelled() => return,
                _ = tokio::time::sleep(backoff) => {}
            }
            backoff = (backoff * 2).min(self.cfg.reconnect_max);
        }
    }

    /// One session: bootstrap → stream → handshake → subscribe → demux.
    pub async fn run_once(&mut self) -> Result<(), SessionError> {
        let remote_path = self
            .bootstrap
            .ensure_uploaded()
            .await
            .map_err(SessionError::Bootstrap)?;

        let session = self
            .transport
            .stream(&[remote_path, format!("--proto-version={PROTO_VERSION}")])
            .await?;
        let portal_transport::StreamSession {
            mut stdin,
            stdout,
            stderr,
            wait,
        } = session;

        // Tee agent stderr into the daemon log, "agent:"-prefixed (v1 shape).
        let stderr_task = tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                tracing::info!(target: "portal::agent", "agent: {line}");
            }
        });

        // Dedicated reader task: read_exact is NOT cancel-safe under select!,
        // so frames arrive over a channel whose recv() is.
        let (frame_tx, frame_rx) = mpsc::channel::<Result<Envelope, CodecError>>(1);
        let reader_task = tokio::spawn(async move {
            let mut stdout = stdout;
            loop {
                let r = read_frame(&mut stdout).await;
                let failed = r.is_err();
                if frame_tx.send(r).await.is_err() || failed {
                    return;
                }
            }
        });

        let result = self.drive_session(&mut stdin, frame_rx).await;

        // Teardown: closing stdin ends the remote agent; bounded wait, then
        // detach (the ssh child exits on its own once the pipes close).
        drop(stdin);
        stderr_task.abort();
        reader_task.abort();
        let _ = tokio::time::timeout(Duration::from_secs(5), wait).await;
        result
    }

    async fn drive_session<W: AsyncWrite + Send + Unpin>(
        &mut self,
        stdin: &mut W,
        mut frames: mpsc::Receiver<Result<Envelope, CodecError>>,
    ) -> Result<(), SessionError> {
        let embedded = self.bootstrap.embedded_sha();

        // Hello → HelloAck. v2 improvement over Go: the handshake read is
        // bounded by the heartbeat timeout instead of blocking indefinitely
        // on a wedged remote shell.
        write_frame(
            stdin,
            &Envelope::of_hello(Hello {
                proto_version: PROTO_VERSION,
                client_git_sha: embedded.clone(),
                client_pid: std::process::id() as i64,
                poll_interval_ms: 0, // agent default (75ms)
                want_destroy_mc: true,
                services: Some(super::client_services()),
                box_name: (!self.cfg.box_name.is_empty()).then(|| self.cfg.box_name.clone()),
            }),
        )
        .await?;
        let first = tokio::time::timeout(self.cfg.heartbeat_timeout, frames.recv())
            .await
            .map_err(|_| SessionError::HeartbeatTimeout)?
            .ok_or(SessionError::Eof)??;
        if let Some(ae) = first.agent_error {
            return Err(SessionError::Agent {
                code: ae.code,
                msg: ae.msg,
            });
        }
        let ack = first.hello_ack.ok_or(SessionError::NotHelloAck)?;
        if ack.agent_git_sha != embedded {
            // Stale/corrupt agent at the canonical path: force-delete so the
            // next connect's bootstrap re-uploads.
            tracing::error!(target: "portal::agent", agent = %ack.agent_git_sha,
                embedded = %embedded, "agent SHA mismatch — forcing re-upload");
            let rm = format!("rm -f ~/.cache/portal/agent-{}", ack.agent_git_sha);
            let _ = self
                .transport
                .exec(b"", &["bash".into(), "-c".into(), shell_quote(&rm)])
                .await;
            return Err(SessionError::ShaMismatch {
                agent: ack.agent_git_sha,
                embedded,
            });
        }
        self.bootstrap.set_boot_id(&ack.boot_id);
        let agent_services = ack.services.clone().unwrap_or_default();
        *self.hello_ack.lock().unwrap() = Some(ack);
        // Converge the box install now that the SHA matched (daemon-driven
        // re-convergence, v1 DESIGN §9.1): stable portald symlink + PATH
        // shims. Non-fatal but loud — forwarding must not be held hostage to
        // a shim write, but paste, askpass and callback-URL opening all
        // depend on it.
        if let Err(err) = self.bootstrap.ensure_box_converged().await {
            tracing::error!(target: "portal::agent",
                "box convergence failed — clipboard paste, sudo askpass and callback URLs will \
                 not work until this succeeds: {err}");
        }

        // Initial Subscribe with the latest filter, THEN announce Connected
        // (v1 ordering).
        let mut filter = self.filter.clone();
        {
            let f = filter.borrow_and_update().clone();
            self.send_subscribe(stdin, f).await?;
        }
        let _ = self.sinks.engine.try_send(Event::Connected);

        // Demux with coalescing + heartbeat watchdog.
        let mut pend_added: Vec<u16> = Vec::new();
        let mut pend_removed: Vec<u16> = Vec::new();
        let mut flush_at: Option<tokio::time::Instant> = None;
        let mut hb_deadline = tokio::time::Instant::now() + self.cfg.heartbeat_timeout;
        let mut warned: std::collections::HashSet<String> = Default::default();
        // Fused when the filter Sender is gone — selecting on a dead watch
        // channel would return Err instantly forever (busy loop).
        let mut filter_alive = true;

        loop {
            let flush_sleep = async {
                match flush_at {
                    Some(at) => tokio::time::sleep_until(at).await,
                    None => std::future::pending().await,
                }
            };
            tokio::select! {
                _ = tokio::time::sleep_until(hb_deadline) => {
                    return Err(SessionError::HeartbeatTimeout);
                }
                _ = flush_sleep => {
                    flush_at = None;
                    if !pend_added.is_empty() || !pend_removed.is_empty() {
                        let _ = self.sinks.engine.try_send(Event::Delta {
                            added: std::mem::take(&mut pend_added),
                            removed: std::mem::take(&mut pend_removed),
                        });
                    }
                }
                changed = filter.changed(), if filter_alive => {
                    match changed {
                        Ok(()) => {
                            let f = filter.borrow_and_update().clone();
                            self.send_subscribe(stdin, f).await?;
                        }
                        Err(_) => filter_alive = false, // sender gone: fuse the branch
                    }
                }
                out = self.outbound.recv() => {
                    let Some(out) = out else { return Ok(()) }; // daemon shutting down
                    let seq = self.msg_seq.fetch_add(1, Ordering::Relaxed) + 1;
                    write_frame(stdin, &Envelope::of_msg(Msg {
                        service: out.service.to_string(),
                        kind: out.kind.to_string(),
                        seq: Some(seq),
                        payload: Some(out.payload),
                    })).await?;
                }
                frame = frames.recv() => {
                    let env = frame.ok_or(SessionError::Eof)??;
                    hb_deadline = tokio::time::Instant::now() + self.cfg.heartbeat_timeout;
                    if let Some(s) = env.snapshot {
                        // Snapshot is a RESET: pending deltas are void.
                        self.snapshot.replace(s.seq, s.ports);
                        pend_added.clear();
                        pend_removed.clear();
                        flush_at = None;
                        let _ = self.sinks.engine.try_send(Event::SnapshotReplaced);
                    } else if let Some(pa) = env.port_added {
                        if self.snapshot.add(pa.seq, pa.port.clone()) {
                            pend_added.push(pa.port.port);
                            flush_at.get_or_insert(
                                tokio::time::Instant::now() + self.cfg.coalesce_window);
                        }
                    } else if let Some(pr) = env.port_removed {
                        if self.snapshot.remove(pr.seq, pr.port) {
                            pend_removed.push(pr.port);
                            flush_at.get_or_insert(
                                tokio::time::Instant::now() + self.cfg.coalesce_window);
                        }
                    } else if let Some(msg) = env.msg {
                        self.route_msg(msg, &agent_services, &mut warned);
                    } else if let Some(ae) = env.agent_error {
                        return Err(SessionError::Agent { code: ae.code, msg: ae.msg });
                    } else if env.bye.is_some() {
                        return Err(SessionError::Bye);
                    }
                    // Heartbeat/SubscribeAck: the watchdog bump above suffices.
                }
            }
        }
    }

    async fn send_subscribe<W: AsyncWrite + Send + Unpin>(
        &self,
        stdin: &mut W,
        f: Filter,
    ) -> Result<(), SessionError> {
        let rsid = self.rsid.fetch_add(1, Ordering::Relaxed) + 1;
        write_frame(
            stdin,
            &Envelope::of_subscribe(Subscribe {
                deny: f.deny,
                allow: f.allow,
                exclude_ephemeral: f.exclude_ephemeral,
                resubscribe_id: rsid,
            }),
        )
        .await?;
        Ok(())
    }

    /// Route an inbound service frame to its dedicated sink (registry-lite).
    /// Unknown services and decode failures are logged drops — the session
    /// lives (v1 contract).
    fn route_msg(
        &self,
        msg: Msg,
        agent_services: &std::collections::BTreeMap<String, u32>,
        warned: &mut std::collections::HashSet<String>,
    ) {
        // S4 symmetric advertisement: frames for a service the agent did not
        // advertise are dropped with one warning.
        if !agent_services.contains_key(&msg.service) {
            if warned.insert(msg.service.clone()) {
                tracing::warn!(target: "portal::agent", service = %msg.service,
                    "inbound frame for a service the agent did not advertise; dropping");
            }
            return;
        }
        let Some(payload) = msg.payload else { return };
        macro_rules! deliver {
            ($sink:expr, $wrap:expr) => {
                match payload.deserialized() {
                    Ok(v) => {
                        let _ = $sink.try_send($wrap(v));
                    }
                    Err(e) => tracing::warn!(target: "portal::agent",
                        service = %msg.service, err = %e, "payload decode failed; dropping"),
                }
            };
        }
        match (msg.service.as_str(), msg.kind.as_str()) {
            ("openurl", _) => match payload.deserialized::<OpenUrl>() {
                Ok(ou) => {
                    let _ = self.sinks.engine.try_send(Event::OpenUrl { url: ou.url });
                }
                Err(e) => tracing::warn!(target: "portal::agent", err = %e,
                    "openurl decode failed; dropping"),
            },
            ("notify", _) => {
                let seq = msg.seq.unwrap_or(0);
                deliver!(self.sinks.notify, |nf| ServiceRequest::Notify {
                    notify: nf,
                    seq
                });
            }
            ("clip", "req") => deliver!(self.sinks.clip, ServiceRequest::Clip),
            ("clipwrite", "req") => deliver!(self.sinks.clip_write, ServiceRequest::ClipWrite),
            ("cred", "req") => deliver!(self.sinks.cred, ServiceRequest::Cred),
            ("clipsync", "ack") => deliver!(self.sinks.clipsync, ServiceRequest::ClipSyncAck),
            (svc, kind) => {
                if warned.insert(format!("{svc}/{kind}")) {
                    tracing::warn!(target: "portal::agent", service = svc, kind = kind,
                        "no handler for inbound service frame; dropping");
                }
            }
        }
    }
}

fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}
