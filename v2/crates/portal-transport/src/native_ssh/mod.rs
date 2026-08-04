//! THE transport: a built-in SSH client (russh). v2 deliberately has no
//! system-ssh/ControlMaster implementation — owning the connection is what
//! makes the rest of the design sound:
//!
//! - forwards are [`crate::forwarder::ListenerForwarder`] state (this module
//!   provides the [`Dialer`] via direct-tcpip channels): exact in-process
//!   ground truth, conflicts as bind errors;
//! - exec exit codes come from the wire (exit-status), forward failures are
//!   real channel-open errors — no stderr substring sniffing;
//! - clipsync blobs get their own flow-controlled channel, so bulk transfer
//!   cannot starve the RPC pipe's heartbeats (v1's 8 MiB cap root cause).
//!
//! Semantics carried over from v1:
//! - host resolution honors ~/.ssh/config exactly (via `ssh -G`, incl.
//!   Include/Match/ProxyJump/ProxyCommand — see [`resolve`]);
//! - strict known_hosts, never interactive (BatchMode doctrine: `portal
//!   install` owns the one interactive validation pass);
//! - keepalive 15s / 3 strikes (ServerAliveInterval/CountMax equivalents);
//! - `ensure` is the (re)build point and reports `rebuilt` only for the call
//!   that actually built; ops on a dead connection rebuild lazily.

pub mod connect;
pub mod resolve;

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use russh::ChannelMsg;
use russh::client;
use tokio::io::{AsyncWrite, AsyncWriteExt};
use tokio::net::TcpStream;

pub use connect::HostKeyPolicy;
pub use resolve::ResolvedTarget;

use crate::forwarder::{Dialer, ForwardStream};
use crate::runner::Runner;
use crate::{
    Desc, ExecOutput, Health, StreamSession, Transport, TransportError, TransportImpl, shell_join,
};
use connect::{CommandStream, PortalClientHandler, authenticate};

/// One authenticated SSH connection (plus any ProxyJump hop connections that
/// must stay alive to keep the tunnel up).
pub struct Connection {
    handle: client::Handle<PortalClientHandler>,
    _hops: Vec<client::Handle<PortalClientHandler>>,
    established: std::time::Instant,
}

impl Connection {
    fn is_up(&self) -> bool {
        !self.handle.is_closed()
    }
}

pub struct NativeSsh {
    /// The destination as configured ([user@]host alias) — resolved via ssh -G.
    destination: String,
    runner: Arc<dyn Runner>,
    policy: HostKeyPolicy,
    use_agent: bool,
    keepalive_interval: Duration,
    keepalive_max: usize,
    /// Test seam: skip `ssh -G` and use this target directly.
    explicit_target: Option<ResolvedTarget>,
    /// Bound on one full connection build (resolve + dial + handshake + auth
    /// per hop). v1's ConnectTimeout=12 equivalent, with headroom for hops.
    connect_timeout: Duration,
    conn: tokio::sync::Mutex<Option<Arc<Connection>>>,
}

impl NativeSsh {
    pub fn new(destination: impl Into<String>, runner: Arc<dyn Runner>) -> Self {
        Self {
            destination: destination.into(),
            runner,
            policy: HostKeyPolicy::KnownHosts,
            use_agent: true,
            keepalive_interval: Duration::from_secs(15),
            keepalive_max: 3,
            explicit_target: None,
            connect_timeout: Duration::from_secs(20),
            conn: tokio::sync::Mutex::new(None),
        }
    }

    pub fn with_policy(mut self, policy: HostKeyPolicy) -> Self {
        self.policy = policy;
        self
    }

    pub fn with_agent(mut self, use_agent: bool) -> Self {
        self.use_agent = use_agent;
        self
    }

    /// Test seam: bypass `ssh -G` resolution entirely.
    pub fn with_target(mut self, target: ResolvedTarget) -> Self {
        self.explicit_target = Some(target);
        self
    }

    fn client_config(&self) -> Arc<client::Config> {
        Arc::new(client::Config {
            keepalive_interval: Some(self.keepalive_interval),
            keepalive_max: self.keepalive_max,
            nodelay: true, // forwarded connections are interactive
            // The agent's heartbeats/frames must flow continuously; a too-small
            // channel buffer or window would starve the RPC pipe. russh defaults
            // are 100 msgs / 2MB — keep them explicit so they're not lost to a
            // future default change, and bump the window for blob transfers.
            channel_buffer_size: 256,
            window_size: 8 * 1024 * 1024,
            ..client::Config::default()
        })
    }

    async fn resolve_target(&self) -> Result<ResolvedTarget, TransportError> {
        if let Some(t) = &self.explicit_target {
            return Ok(t.clone());
        }
        resolve::resolve(&*self.runner, &self.destination, None, None).await
    }

    async fn connect_hop(
        &self,
        via: Option<&client::Handle<PortalClientHandler>>,
        target: &ResolvedTarget,
    ) -> Result<client::Handle<PortalClientHandler>, TransportError> {
        let handler = PortalClientHandler {
            hostname: target.hostname.clone(),
            port: target.port,
            policy: self.policy.clone(),
        };
        let stream: Box<dyn ForwardStream> = match via {
            Some(prev) => {
                let ch = prev
                    .channel_open_direct_tcpip(
                        target.hostname.clone(),
                        u32::from(target.port),
                        "127.0.0.1",
                        0,
                    )
                    .await
                    .map_err(ssh_err)?;
                Box::new(ch.into_stream())
            }
            None => match &target.proxy_command {
                Some(cmd) => Box::new(CommandStream::spawn(cmd, &target.hostname, target.port)?),
                None => {
                    Box::new(TcpStream::connect((target.hostname.as_str(), target.port)).await?)
                }
            },
        };
        let mut handle = client::connect_stream(self.client_config(), stream, handler)
            .await
            .map_err(ssh_err)?;
        authenticate(
            &mut handle,
            &target.user,
            &target.identity_files,
            self.use_agent,
        )
        .await?;
        Ok(handle)
    }

    async fn build_connection(&self) -> Result<Connection, TransportError> {
        let target = self.resolve_target().await?;
        let mut hops: Vec<client::Handle<PortalClientHandler>> = Vec::new();
        for spec in &target.proxy_jump {
            let (user, host, port) = resolve::parse_jump_spec(spec);
            let jump = resolve::resolve(&*self.runner, host, port, user).await?;
            if !jump.proxy_jump.is_empty() {
                tracing::warn!(target: "portal::ssh", hop = %host,
                    "nested ProxyJump on a jump host is not supported; connecting directly to it");
            }
            let handle = self.connect_hop(hops.last(), &jump).await?;
            hops.push(handle);
        }
        let handle = self.connect_hop(hops.last(), &target).await?;
        tracing::info!(target: "portal::ssh", host = %target.hostname, port = target.port,
            hops = hops.len(), "ssh connection established");
        Ok(Connection {
            handle,
            _hops: hops,
            established: std::time::Instant::now(),
        })
    }

    /// Get the live connection, building one if absent/dead. `bool` is true
    /// iff THIS call built it (the `ensure` rebuilt contract). The mutex is
    /// held across the build — deliberate single-flight: concurrent callers
    /// during a rebuild wait for ONE connection instead of racing N.
    async fn get_or_build(&self) -> Result<(Arc<Connection>, bool), TransportError> {
        let mut guard = self.conn.lock().await;
        if let Some(c) = guard.as_ref()
            && c.is_up()
        {
            return Ok((c.clone(), false));
        }
        let built = tokio::time::timeout(self.connect_timeout, self.build_connection())
            .await
            .map_err(|_| {
                TransportError::Ssh(format!(
                    "connect to {} timed out after {:?}",
                    self.destination, self.connect_timeout
                ))
            })??;
        let built = Arc::new(built);
        *guard = Some(built.clone());
        Ok((built, true))
    }
}

fn ssh_err(e: russh::Error) -> TransportError {
    TransportError::Ssh(e.to_string())
}

#[async_trait]
impl Transport for NativeSsh {
    async fn ensure(&self) -> Result<bool, TransportError> {
        let (_, rebuilt) = self.get_or_build().await?;
        Ok(rebuilt)
    }

    async fn health(&self) -> Result<Health, TransportError> {
        // try_lock: a build in flight holds the connection mutex (single-
        // flight); health must not block behind it — report "connecting".
        let Ok(guard) = self.conn.try_lock() else {
            return Ok(Health {
                up: false,
                detail: "connecting".into(),
            });
        };
        match guard.as_ref() {
            Some(c) if c.is_up() => Ok(Health {
                up: true,
                detail: format!("connected {:?}", c.established.elapsed()),
            }),
            _ => Ok(Health::default()),
        }
    }

    /// argv joins with single spaces into ONE command the remote login shell
    /// re-splits (the shell-join contract) — identical to ssh exec semantics.
    async fn exec(&self, stdin: &[u8], argv: &[String]) -> Result<ExecOutput, TransportError> {
        if argv.is_empty() {
            return Err(TransportError::EmptyArgv);
        }
        let (conn, _) = self.get_or_build().await?;
        let mut channel = conn.handle.channel_open_session().await.map_err(ssh_err)?;
        channel
            .exec(true, shell_join(argv))
            .await
            .map_err(ssh_err)?;
        if !stdin.is_empty() {
            channel.data(stdin).await.map_err(ssh_err)?;
        }
        channel.eof().await.map_err(ssh_err)?;

        let mut output = ExecOutput::default();
        let mut code: Option<u32> = None;
        while let Some(msg) = channel.wait().await {
            match msg {
                ChannelMsg::Data { data } => output.stdout.extend_from_slice(&data),
                ChannelMsg::ExtendedData { data, ext: 1 } => output.stderr.extend_from_slice(&data),
                ChannelMsg::ExitStatus { exit_status } => code = Some(exit_status),
                ChannelMsg::Failure => {
                    return Err(TransportError::Ssh("exec request rejected".into()));
                }
                _ => {}
            }
        }
        match code {
            Some(0) | None => Ok(output),
            Some(c) => Err(TransportError::Exit {
                code: c as i32,
                output,
            }),
        }
    }

    async fn stream(&self, argv: &[String]) -> Result<StreamSession, TransportError> {
        if argv.is_empty() {
            return Err(TransportError::EmptyArgv);
        }
        let (conn, _) = self.get_or_build().await?;
        let channel = conn.handle.channel_open_session().await.map_err(ssh_err)?;
        channel
            .exec(true, shell_join(argv))
            .await
            .map_err(ssh_err)?;
        let (mut read_half, write_half) = channel.split();

        // stdin: shutdown() sends channel EOF + Close so the remote process
        // sees stdin end even though the CONNECTION stays up (multiplexed).
        let write_half = Arc::new(write_half);
        let stdin = Box::new(ChannelStdin {
            inner: write_half.make_writer(),
            write_half: Some(write_half.clone()),
        });

        let (mut stdout_w, stdout_r) = tokio::io::duplex(256 * 1024);
        let (mut stderr_w, stderr_r) = tokio::io::duplex(64 * 1024);
        let wait = tokio::spawn(async move {
            let mut code: Option<u32> = None;
            let (mut out_ok, mut err_ok) = (true, true);
            while let Some(msg) = read_half.wait().await {
                match msg {
                    ChannelMsg::Data { data } => {
                        // A dropped consumer must not kill the demux: keep
                        // draining so ExitStatus still lands.
                        if out_ok && stdout_w.write_all(&data).await.is_err() {
                            out_ok = false;
                        }
                    }
                    ChannelMsg::ExtendedData { data, ext: 1 } => {
                        if err_ok && stderr_w.write_all(&data).await.is_err() {
                            err_ok = false;
                        }
                    }
                    ChannelMsg::ExitStatus { exit_status } => code = Some(exit_status),
                    _ => {}
                }
            }
            let _ = write_half.close().await;
            match code {
                Some(c) if c != 0 => Err(TransportError::Exit {
                    code: c as i32,
                    output: ExecOutput::default(),
                }),
                _ => Ok(()),
            }
        });

        Ok(StreamSession {
            stdin,
            stdout: Box::new(stdout_r),
            stderr: Box::new(stderr_r),
            wait,
        })
    }

    async fn close(&self) -> Result<bool, TransportError> {
        let mut guard = self.conn.lock().await;
        let Some(conn) = guard.take() else {
            return Ok(false);
        };
        let _ = conn
            .handle
            .disconnect(russh::Disconnect::ByApplication, "portal stop", "en")
            .await;
        Ok(true)
    }

    fn describe(&self) -> Desc {
        Desc {
            impl_kind: TransportImpl::NativeSsh,
            host: self.destination.clone(),
            endpoint: "native".into(),
        }
    }
}

#[async_trait]
impl Dialer for NativeSsh {
    /// Open a direct-tcpip channel to `localhost:<remote>` ON THE BOX.
    /// "localhost" (not 127.0.0.1) reaches both v4- and v6-bound services —
    /// v1 `-L local:localhost:remote` semantics.
    async fn dial_remote(&self, remote: u16) -> Result<Box<dyn ForwardStream>, TransportError> {
        let (conn, _) = self.get_or_build().await?;
        let channel = conn
            .handle
            .channel_open_direct_tcpip("localhost", u32::from(remote), "127.0.0.1", 0)
            .await
            .map_err(|e| TransportError::Forward {
                local: 0,
                remote,
                stderr: e.to_string(),
            })?;
        Ok(Box::new(channel.into_stream()))
    }
}

/// The stdin handle returned by `stream()`. Wraps the channel writer so that
/// `shutdown()` (or drop) also sends channel EOF+Close — the remote command
/// must observe stdin EOF even though the multiplexed connection stays up.
struct ChannelStdin<W: AsyncWrite + Unpin + Send> {
    inner: W,
    write_half: Option<Arc<russh::ChannelWriteHalf<client::Msg>>>,
}

impl<W: AsyncWrite + Unpin + Send> ChannelStdin<W> {
    fn send_eof(&mut self) {
        if let Some(wh) = self.write_half.take() {
            // Drop can run during runtime teardown where spawn would panic;
            // losing the EOF there is fine — the connection is dying anyway.
            if let Ok(rt) = tokio::runtime::Handle::try_current() {
                rt.spawn(async move {
                    let _ = wh.eof().await;
                });
            }
        }
    }
}

impl<W: AsyncWrite + Unpin + Send> AsyncWrite for ChannelStdin<W> {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        std::pin::Pin::new(&mut self.inner).poll_write(cx, buf)
    }
    fn poll_flush(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.inner).poll_flush(cx)
    }
    fn poll_shutdown(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        let res = std::pin::Pin::new(&mut self.inner).poll_shutdown(cx);
        if matches!(res, std::task::Poll::Ready(_)) {
            self.send_eof();
        }
        res
    }
}

impl<W: AsyncWrite + Unpin + Send> Drop for ChannelStdin<W> {
    fn drop(&mut self) {
        self.send_eof();
    }
}
