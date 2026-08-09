//! The agent serve loop: speaks v4 frames on stdin/stdout, watches loopback
//! listeners, applies clipsync updates to the store, and relays cmd-socket
//! events (notify/open) up the pipe.
//!
//! Contracts:
//! - stdout is EXCLUSIVELY protocol frames; logs go to stderr;
//! - Hello must be the first frame; a ProtoVersion mismatch answers a FATAL
//!   AgentError(code=1) and exits — the Mac's SHA-keyed bootstrap re-upload
//!   heals it (loud beats silent no-op);
//! - Subscribe swaps the filter; stale rsid (<= last) is ignored; every
//!   accepted Subscribe answers SubscribeAck THEN a full Snapshot (RESET);
//! - port events carry a monotonic seq strictly greater than the snapshot's;
//! - a Heartbeat goes out every interval of send-silence; Ping echoes its
//!   nonce in the next heartbeat immediately;
//! - Shutdown answers Bye then exits cleanly; stdin EOF exits cleanly;
//! - clipsync updates apply to the ClipStore; every update is answered with
//!   Ack{change_id, have_blob} — BlobMissing answers have_blob=false and the
//!   Mac pushes the blob out-of-band then re-sends the update.

pub mod filter;
pub mod watcher;

use std::collections::{BTreeMap, BTreeSet};
use std::time::Duration;

use portal_proto::codec::CodecError;
use portal_proto::codec::asynchronous::{read_frame, write_frame};
use portal_proto::envelope::Envelope;
use portal_proto::messages::{
    AgentError, Bye, ClipSyncAck, ClipSyncClear, ClipSyncUpdate, ClipWriteRequest,
    ClipWriteResponse, CredRequest, CredResponse, Heartbeat, HelloAck, Msg, Notify, OpenUrl, Port,
    PortAdded, PortRemoved, Snapshot, SubscribeAck, marshal_payload, unmarshal_payload,
};
use portal_proto::{MAX_FRAME_BYTES, PROTO_VERSION, code, removed_source};
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::mpsc;

use crate::store::{ClipKind, ClipStore, Manifest, StoreError};
use filter::PortFilter;
use watcher::ListenerSource;

/// Inbound cmd-socket events relayed up the pipe (see [`crate::cmdsock`]).
#[derive(Debug)]
pub enum Relay {
    Notify(Notify),
    OpenUrl(String),
    /// A credential request from `portald keychain` — request/response: the
    /// serve loop mints a nonce, forwards a CredRequest, and answers the
    /// oneshot when the correlated CredResponse (or a local denial) lands.
    Cred {
        req: crate::cred::CredShimReq,
        reply: tokio::sync::oneshot::Sender<Result<Vec<u8>, String>>,
    },
    /// A clipboard write from `portald clip copy` (box → Mac). The blob is
    /// already in the local store; the Mac pulls it by sha, sets its
    /// pasteboard, and the oneshot resolves ok/deny.
    ClipWrite {
        req: portal_proto::messages::ClipWriteRequest,
        reply: tokio::sync::oneshot::Sender<Result<(), String>>,
    },
}

impl PartialEq for Relay {
    fn eq(&self, other: &Self) -> bool {
        match (self, other) {
            (Relay::Notify(a), Relay::Notify(b)) => a == b,
            (Relay::OpenUrl(a), Relay::OpenUrl(b)) => a == b,
            _ => false,
        }
    }
}

pub struct AgentConfig {
    pub git_sha: String,
    pub kernel: String,
    pub boot_id: String,
    pub poll_interval: Duration,
    pub heartbeat_interval: Duration,
    pub ephem: (u16, u16),
}

impl Default for AgentConfig {
    fn default() -> Self {
        Self {
            git_sha: "dev".into(),
            kernel: String::new(),
            boot_id: String::new(),
            poll_interval: Duration::from_millis(75),
            heartbeat_interval: Duration::from_secs(5),
            ephem: (32768, 60999),
        }
    }
}

/// Service families this production agent actually handles. Negotiation is
/// symmetric: a service must be advertised by both peers before either side
/// accepts its frames, regardless of which side originates the request.
pub fn advertised_services() -> BTreeMap<String, u32> {
    BTreeMap::from([
        ("clipsync".to_string(), 1),
        ("notify".to_string(), 1),
        ("openurl".to_string(), 1),
        ("cred".to_string(), 1),
        ("clipwrite".to_string(), 1),
    ])
}

pub struct Agent<S: ListenerSource> {
    cfg: AgentConfig,
    source: S,
    store: ClipStore,
    /// Relay events from the cmd socket (notify/open).
    relay: mpsc::Receiver<Relay>,
}

#[derive(Debug, thiserror::Error)]
pub enum AgentExit {
    #[error("codec: {0}")]
    Codec(#[from] portal_proto::codec::CodecError),
    #[error("client spoke proto v{0}, agent speaks v{PROTO_VERSION}")]
    ProtoMismatch(u32),
    #[error("first frame was not Hello")]
    NotHello,
}

impl<S: ListenerSource> Agent<S> {
    pub fn new(
        cfg: AgentConfig,
        source: S,
        store: ClipStore,
        relay: mpsc::Receiver<Relay>,
    ) -> Self {
        Self {
            cfg,
            source,
            store,
            relay,
        }
    }

    /// Run one session on the given pipes. `Ok(())` is a clean exit (EOF /
    /// Shutdown); Err is fatal (the Mac reconnects).
    ///
    /// stdin is consumed by a dedicated reader task: `read_frame`'s
    /// `read_exact` is NOT cancel-safe, and the session loop `select!`s the
    /// read against poll/heartbeat ticks — a tick winning the race mid-frame
    /// would drop the partially-read bytes and desync the stream. Frames
    /// arrive over an mpsc channel whose `recv()` IS cancel-safe (the same
    /// discipline as the Mac-side session client).
    pub async fn serve<R, W>(&mut self, stdin: R, stdout: &mut W) -> Result<(), AgentExit>
    where
        R: AsyncRead + Send + Unpin + 'static,
        W: AsyncWrite + Send + Unpin,
    {
        let (frame_tx, mut frames) = mpsc::channel::<Result<Envelope, CodecError>>(1);
        let reader = tokio::spawn(async move {
            let mut stdin = stdin;
            loop {
                let r = read_frame(&mut stdin).await;
                let failed = r.is_err();
                if frame_tx.send(r).await.is_err() || failed {
                    return;
                }
            }
        });
        let result = self.session(&mut frames, stdout).await;
        reader.abort();
        result
    }

    async fn session<W>(
        &mut self,
        frames: &mut mpsc::Receiver<Result<Envelope, CodecError>>,
        stdout: &mut W,
    ) -> Result<(), AgentExit>
    where
        W: AsyncWrite + Send + Unpin,
    {
        // ---- handshake ----
        let first = match frames.recv().await {
            Some(r) => r?,
            // Reader gone before any frame: EOF-before-Hello, an error —
            // same as the direct read_frame would have surfaced.
            None => {
                return Err(AgentExit::Codec(CodecError::Io(
                    std::io::ErrorKind::UnexpectedEof.into(),
                )));
            }
        };
        let Some(hello) = first.hello else {
            // Default-deny: anything but Hello first is a fatal protocol error.
            let _ = write_frame(
                stdout,
                &Envelope::of_agent_error(AgentError {
                    code: code::BAD_SUBSCRIBE,
                    msg: "first frame must be Hello".into(),
                    fatal: true,
                }),
            )
            .await;
            return Err(AgentExit::NotHello);
        };
        if hello.proto_version != PROTO_VERSION {
            let _ = write_frame(
                stdout,
                &Envelope::of_agent_error(AgentError {
                    code: code::PROTOCOL_MISMATCH,
                    msg: format!(
                        "agent speaks v{PROTO_VERSION}, client sent v{}",
                        hello.proto_version
                    ),
                    fatal: true,
                }),
            )
            .await;
            return Err(AgentExit::ProtoMismatch(hello.proto_version));
        }
        if let Some(name) = &hello.box_name {
            tracing::info!(target: "portald", box_name = %name, "client connected");
        }
        let client_services = hello.services.unwrap_or_default();
        write_frame(
            stdout,
            &Envelope::of_hello_ack(HelloAck {
                proto_version: PROTO_VERSION,
                agent_git_sha: self.cfg.git_sha.clone(),
                agent_pid: std::process::id() as i64,
                kernel: self.cfg.kernel.clone(),
                boot_id: self.cfg.boot_id.clone(),
                ephem_min: self.cfg.ephem.0,
                ephem_max: self.cfg.ephem.1,
                now_unix_nano: now_nano(),
                services: Some(advertised_services()),
            }),
        )
        .await?;

        // ---- steady state ----
        let mut filter: Option<PortFilter> = None; // no Subscribe yet = no events
        let mut last_rsid: u64 = 0;
        let mut seq: u64 = 0;
        let mut current: BTreeSet<u16> = BTreeSet::new();
        let mut msg_seq: u64 = 0;
        let mut pending_nonce: Option<u64> = None;
        // Pending credential waiters, keyed by nonce (epoch = this pid).
        // Cap 2: same DoS bound as the v1 agent's maxInflight discipline.
        let mut cred_waiters: std::collections::HashMap<
            u64,
            tokio::sync::oneshot::Sender<Result<Vec<u8>, String>>,
        > = Default::default();
        let mut clipwrite_waiters: std::collections::HashMap<
            u64,
            tokio::sync::oneshot::Sender<Result<(), String>>,
        > = Default::default();
        let mut cred_nonce: u64 = 0;
        let epoch = u64::from(std::process::id());

        let mut poll = tokio::time::interval(self.cfg.poll_interval);
        poll.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let mut hb = tokio::time::interval(self.cfg.heartbeat_interval);
        hb.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

        loop {
            tokio::select! {
                frame = frames.recv() => {
                    let env = match frame {
                        Some(Ok(env)) => env,
                        Some(Err(CodecError::Io(e)))
                            if e.kind() == std::io::ErrorKind::UnexpectedEof =>
                        {
                            return Ok(()); // client gone: clean exit
                        }
                        Some(Err(e)) => return Err(e.into()),
                        None => return Ok(()), // reader task gone: clean exit
                    };
                    if let Some(sub) = env.subscribe {
                        if sub.resubscribe_id <= last_rsid {
                            continue; // stale retry
                        }
                        last_rsid = sub.resubscribe_id;
                        filter = Some(PortFilter::new(
                            sub.deny, sub.allow, sub.exclude_ephemeral, self.cfg.ephem,
                        ));
                        write_frame(stdout, &Envelope::of_subscribe_ack(SubscribeAck {
                            resubscribe_id: last_rsid,
                        })).await?;
                        // Snapshot = RESET, from ground truth.
                        current = self.desired(filter.as_ref().unwrap());
                        seq += 1;
                        write_frame(stdout, &Envelope::of_snapshot(Snapshot {
                            seq,
                            generated_at: now_nano(),
                            ports: current.iter().map(|&p| port4(p)).collect(),
                        })).await?;
                        hb.reset();
                    } else if let Some(ping) = env.ping {
                        pending_nonce = Some(ping.nonce);
                        hb.reset_immediately();
                    } else if env.req_snap.is_some() {
                        if let Some(f) = &filter {
                            current = self.desired(f);
                            seq += 1;
                            write_frame(stdout, &Envelope::of_snapshot(Snapshot {
                                seq,
                                generated_at: now_nano(),
                                ports: current.iter().map(|&p| port4(p)).collect(),
                            })).await?;
                            hb.reset();
                        }
                    } else if let Some(msg) = env.msg {
                        if msg.service == "cred" && msg.kind == "resp" {
                            if let Some(payload) = &msg.payload
                                && let Ok(resp) = unmarshal_payload::<CredResponse>(payload)
                                && resp.epoch == epoch
                                && let Some(waiter) = cred_waiters.remove(&resp.nonce)
                            {
                                let answer = if resp.ok {
                                    Ok(resp.secret.map(|s| s.into_vec()).unwrap_or_default())
                                } else {
                                    Err(resp.err.unwrap_or_else(|| "denied".into()))
                                };
                                let _ = waiter.send(answer);
                            }
                        } else if msg.service == "clipwrite" && msg.kind == "resp" {
                            if let Some(payload) = &msg.payload
                                && let Ok(resp) = unmarshal_payload::<ClipWriteResponse>(payload)
                                && resp.epoch == epoch
                                && let Some(waiter) = clipwrite_waiters.remove(&resp.nonce)
                            {
                                let answer = if resp.ok {
                                    Ok(())
                                } else {
                                    Err(resp.err.unwrap_or_else(|| "denied".into()))
                                };
                                let _ = waiter.send(answer);
                            }
                        } else if let Some(reply) = self.handle_msg(&msg, &mut msg_seq) {
                            write_frame(stdout, &reply).await?;
                            hb.reset();
                        }
                    } else if env.shutdown.is_some() {
                        let _ = write_frame(stdout, &Envelope::of_bye(Bye { reason: None })).await;
                        return Ok(());
                    }
                    // Hello twice / server-only frames: ignore (v1 logs+drops).
                }
                _ = poll.tick() => {
                    let Some(f) = &filter else { continue };
                    let desired = self.desired(f);
                    let churn = desired != current;
                    for &added in desired.difference(&current) {
                        seq += 1;
                        write_frame(stdout, &Envelope::of_port_added(PortAdded {
                            seq, port: port4(added), at: now_nano(),
                        })).await?;
                    }
                    for &removed in current.difference(&desired) {
                        seq += 1;
                        write_frame(stdout, &Envelope::of_port_removed(PortRemoved {
                            seq, port: removed, family: 4, at: now_nano(),
                            source: removed_source::DUMP_DIFF,
                        })).await?;
                    }
                    if churn {
                        hb.reset();
                    }
                    current = desired;
                }
                relay = self.relay.recv() => {
                    let Some(relay) = relay else { continue };
                    // Only relay when the client advertised the service (S4).
                    msg_seq += 1;
                    let frame = match relay {
                        Relay::Notify(n) if client_services.contains_key("notify") => {
                            Envelope::of_msg(Msg {
                                service: "notify".into(), kind: "event".into(),
                                seq: Some(msg_seq),
                                payload: marshal_payload(&n).ok(),
                            })
                        }
                        Relay::OpenUrl(url) if client_services.contains_key("openurl") => {
                            Envelope::of_msg(Msg {
                                service: "openurl".into(), kind: "event".into(),
                                seq: Some(msg_seq),
                                payload: marshal_payload(&OpenUrl { url, seq: msg_seq }).ok(),
                            })
                        }
                        Relay::Cred { req, reply } => {
                            if !client_services.contains_key("cred") {
                                let _ = reply.send(Err("no-client".into()));
                                continue;
                            }
                            if cred_waiters.len() >= 2 {
                                let _ = reply.send(Err("busy".into()));
                                continue;
                            }
                            cred_nonce += 1;
                            cred_waiters.insert(cred_nonce, reply);
                            Envelope::of_msg(Msg {
                                service: "cred".into(), kind: "req".into(),
                                seq: Some(msg_seq),
                                payload: marshal_payload(&CredRequest {
                                    nonce: cred_nonce,
                                    epoch,
                                    label: req.label,
                                    requester: (!req.requester.is_empty()).then_some(req.requester),
                                    mode: req.mode,
                                    target: (!req.target.is_empty()).then_some(req.target),
                                }).ok(),
                            })
                        }
                        Relay::ClipWrite { req, reply } => {
                            if !client_services.contains_key("clipwrite") {
                                let _ = reply.send(Err("no-client".into()));
                                continue;
                            }
                            if clipwrite_waiters.len() >= 4 {
                                let _ = reply.send(Err("busy".into()));
                                continue;
                            }
                            cred_nonce += 1; // shared nonce space is fine: maps are per-service
                            clipwrite_waiters.insert(cred_nonce, reply);
                            Envelope::of_msg(Msg {
                                service: "clipwrite".into(), kind: "req".into(),
                                seq: Some(msg_seq),
                                payload: marshal_payload(&ClipWriteRequest {
                                    nonce: cred_nonce,
                                    epoch,
                                    kind: req.kind,
                                    format: req.format,
                                    sha: req.sha,
                                    size: req.size,
                                }).ok(),
                            })
                        }
                        _ => continue,
                    };
                    write_frame(stdout, &frame).await?;
                    hb.reset();
                }
                _ = hb.tick() => {
                    // Every send resets this timer, so a tick == a full
                    // interval of send-silence: heartbeat unconditionally.
                    // (A skip-flag scheme allows ~2x interval of silence —
                    // too close to the client's watchdog.)
                    seq += 1;
                    write_frame(stdout, &Envelope::of_heartbeat(Heartbeat {
                        seq,
                        uptime_nano: 0,
                        now: now_nano(),
                        nonce: pending_nonce.take(),
                    })).await?;
                    hb.reset();
                }
            }
        }
    }

    fn desired(&mut self, filter: &PortFilter) -> BTreeSet<u16> {
        self.source
            .listening()
            .into_iter()
            .map(|p| p.port)
            .filter(|&p| filter.admits(p))
            .collect()
    }

    /// Handle an inbound service frame. clipsync is the only request/response
    /// service the agent serves today; unknown services are logged drops.
    fn handle_msg(&mut self, msg: &Msg, msg_seq: &mut u64) -> Option<Envelope> {
        if msg.service != "clipsync" {
            tracing::warn!(target: "portald", service = %msg.service, kind = %msg.kind,
                "no handler for inbound service frame; dropping");
            return None;
        }
        let payload = msg.payload.as_ref()?;
        let ack = match msg.kind.as_str() {
            "update" => {
                let u: ClipSyncUpdate = match unmarshal_payload(payload) {
                    Ok(u) => u,
                    Err(e) => {
                        tracing::warn!(target: "portald", %e, "clipsync update decode failed");
                        return None;
                    }
                };
                self.apply_update(&u)
            }
            "clear" => {
                let c: ClipSyncClear = match unmarshal_payload(payload) {
                    Ok(c) => c,
                    Err(e) => {
                        tracing::warn!(target: "portald", %e, "clipsync clear decode failed");
                        return None;
                    }
                };
                let m = Manifest {
                    change_id: c.change_id,
                    kind: ClipKind::Clear,
                    format: None,
                    sha: None,
                    size: None,
                    received_at: now_unix(),
                };
                match self.store.apply(&m) {
                    Ok(()) | Err(StoreError::Stale { .. }) => ClipSyncAck {
                        change_id: c.change_id,
                        have_blob: true,
                    },
                    Err(e) => {
                        tracing::warn!(target: "portald", %e, "clipsync clear apply failed");
                        return None;
                    }
                }
            }
            _ => return None,
        };
        *msg_seq += 1;
        Some(Envelope::of_msg(Msg {
            service: "clipsync".into(),
            kind: "ack".into(),
            seq: Some(*msg_seq),
            payload: marshal_payload(&ack).ok(),
        }))
    }

    fn apply_update(&mut self, u: &ClipSyncUpdate) -> ClipSyncAck {
        let nack = ClipSyncAck {
            change_id: u.change_id,
            have_blob: false,
        };
        let ok = ClipSyncAck {
            change_id: u.change_id,
            have_blob: true,
        };
        let kind = match u.kind.as_str() {
            "text" => ClipKind::Text,
            "image" => ClipKind::Image,
            other => {
                tracing::warn!(target: "portald", kind = %other, "unknown clipsync kind");
                return nack;
            }
        };
        // Inline bytes install the blob right here (still sha-verified —
        // and inline may not exceed the frame budget it arrived under).
        if let Some(inline) = &u.inline
            && inline.len() <= MAX_FRAME_BYTES
            && let Some(sha) = &u.sha
            && let Err(e) = self.store.put_blob(sha, inline)
        {
            tracing::warn!(target: "portald", %e, "inline blob install failed");
            return nack;
        }
        let m = Manifest {
            change_id: u.change_id,
            kind,
            format: u.format.clone(),
            sha: u.sha.clone(),
            size: u.size.map(|s| s as u64),
            received_at: now_unix(),
        };
        match self.store.apply(&m) {
            Ok(()) => ok,
            // Idempotent/stale: the box already moved past this generation.
            Err(StoreError::Stale { .. }) => ok,
            Err(StoreError::BlobMissing { .. }) => nack,
            Err(e) => {
                tracing::warn!(target: "portald", %e, "clipsync apply failed");
                nack
            }
        }
    }
}

fn port4(p: u16) -> Port {
    Port {
        port: p,
        family: 4,
        addr: "127.0.0.1".into(),
        inode_ns: 0,
    }
}

fn now_nano() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

fn now_unix() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}
