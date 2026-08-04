//! The seam between portal's command/forwarding logic and the concrete
//! mechanism that reaches a dev box.
//!
//! v2 has exactly ONE production transport: [`native_ssh`] (built-in SSH).
//! The v1 system-ssh/ControlMaster implementation is deliberately gone —
//! owning the connection makes forwards in-process ground truth (see
//! [`forwarder`]) and gives clipsync a flow-controlled blob channel.
//! [`localexec`] remains for tests/dev.
//!
//! Composition rules carried over from v1:
//! - `Transport` is EXACTLY the six core methods; forwarding lives ONLY on
//!   [`PortForwarder`] (acquired separately at the composition root).
//! - Liveness gates use `Health.up`, never a pid.
//! - Uploads are composed OVER `exec` (binary stdin + hardened scripts);
//!   there is deliberately no Uploader capability.
//!
//! v2 CHANGE: forwards are (local, remote) PAIRS — see [`ForwardSpec`]. v1
//! forwarded same-port-to-same-port; multi-box support maps remote ports into
//! per-box local ranges (see portal-core's `portmap`).

pub mod conformance;
pub mod forwarder;
pub mod localexec;
pub mod lsof;
pub mod native_ssh;
pub mod runner;
pub mod testing;

use std::fmt;

use async_trait::async_trait;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::task::JoinHandle;

/// Identifies the concrete transport implementation for status/log rendering.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TransportImpl {
    NativeSsh,
    LocalExec,
    Unavailable,
}

impl fmt::Display for TransportImpl {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            TransportImpl::NativeSsh => "native-ssh",
            TransportImpl::LocalExec => "localexec",
            TransportImpl::Unavailable => "unavailable",
        })
    }
}

/// Identifying metadata for status/log rendering.
#[derive(Debug, Clone)]
pub struct Desc {
    pub impl_kind: TransportImpl,
    pub host: String,
    pub endpoint: String,
}

/// Liveness snapshot. `up` is the sole liveness signal callers may gate on.
#[derive(Debug, Clone, Default)]
pub struct Health {
    pub up: bool,
    /// Impl-specific detail for status rendering (e.g. connection age /
    /// keepalive state).
    pub detail: String,
}

/// Captured output of a completed remote command.
#[derive(Debug, Clone, Default)]
pub struct ExecOutput {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}

impl ExecOutput {
    pub fn stdout_lossy(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }
    pub fn stderr_lossy(&self) -> String {
        String::from_utf8_lossy(&self.stderr).into_owned()
    }
}

#[derive(Debug, thiserror::Error)]
pub enum TransportError {
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    /// Remote command exited non-zero. `stderr` is trimmed for the message;
    /// full output is preserved.
    #[error("remote command exited with code {code}: {}", .output.stderr_lossy().trim())]
    Exit { code: i32, output: ExecOutput },
    /// Forward failure with a real cause (channel-open failure, bad target).
    #[error("forward {local}->{remote} failed: {stderr}")]
    Forward {
        local: u16,
        remote: u16,
        stderr: String,
    },
    /// The LOCAL port is already bound by another process — the v2 conflict
    /// signal (v1 needed an lsof pre-check; here the bind itself tells us).
    #[error("local port {local} is already in use")]
    PortInUse { local: u16 },
    /// SSH-level failure (handshake, auth, channel, resolution).
    #[error("ssh: {0}")]
    Ssh(String),
    #[error("empty argv")]
    EmptyArgv,
    #[error("transport not implemented: {0}")]
    Unimplemented(&'static str),
}

/// One live streaming session (the long-lived portald RPC pipe rides this).
/// The caller closes/drops `stdin` to signal EOF, drains `stdout`/`stderr`,
/// then awaits `wait` for the exit status.
pub struct StreamSession {
    pub stdin: Box<dyn AsyncWrite + Send + Unpin>,
    pub stdout: Box<dyn AsyncRead + Send + Unpin>,
    pub stderr: Box<dyn AsyncRead + Send + Unpin>,
    pub wait: JoinHandle<Result<(), TransportError>>,
}

/// The transport-agnostic core. EXACTLY these six methods; forwarding never
/// grows here (a compile error on a forwarding call is resolved by routing it
/// through [`PortForwarder`], not by widening this trait).
///
/// ARGV CONTRACT (identical to v1): `argv` is joined with single ASCII spaces
/// into ONE command string that a shell on the TARGET executes — exactly an
/// ssh session's command semantics. Callers needing multiple tokens,
/// redirection, globbing, or any shell metacharacter preserved MUST pre-quote
/// them into a single argv element (see [`shell_quote`]).
#[async_trait]
pub trait Transport: Send + Sync {
    /// Bring the transport up if it is down (idempotent). Returns `true` iff
    /// THIS call performed the (re)build.
    async fn ensure(&self) -> Result<bool, TransportError>;

    /// Liveness. Callers gate on `Health.up`, never on `pid`.
    async fn health(&self) -> Result<Health, TransportError>;

    /// Run a command on the target and capture its output. A non-zero exit
    /// returns `TransportError::Exit` carrying the full output.
    async fn exec(&self, stdin: &[u8], argv: &[String]) -> Result<ExecOutput, TransportError>;

    /// Run argv on the target with live stdio pipes.
    async fn stream(&self, argv: &[String]) -> Result<StreamSession, TransportError>;

    /// Tear the transport down. Returns `true` iff there was something to stop.
    async fn close(&self) -> Result<bool, TransportError>;

    fn describe(&self) -> Desc;
}

/// One local→remote forward pair. v2: local ≠ remote in general (multi-box
/// indexed mapping); v1's same-port assumption is gone.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct ForwardSpec {
    pub local: u16,
    pub remote: u16,
}

/// Optional local-port-forwarding capability, acquired at the composition
/// root. localexec does NOT implement it (forwarding to yourself is
/// meaningless). `list_forwards` returns ground truth from the live master —
/// never an in-process cache (the reconcile engine depends on this).
#[async_trait]
pub trait PortForwarder: Send + Sync {
    async fn forward(&self, spec: ForwardSpec) -> Result<(), TransportError>;
    async fn cancel(&self, spec: ForwardSpec) -> Result<(), TransportError>;
    async fn list_forwards(&self) -> Result<Vec<ForwardSpec>, TransportError>;
}

/// Join argv per the shell-join contract (single ASCII spaces).
pub fn shell_join(argv: &[String]) -> String {
    argv.join(" ")
}

/// Wrap a string in single quotes for safe remote execution via ssh (which
/// joins argv with spaces and runs the result through the login shell).
pub fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_quote_escapes_single_quotes() {
        assert_eq!(shell_quote("echo 'hi'"), r"'echo '\''hi'\'''");
        assert_eq!(shell_quote("plain"), "'plain'");
    }
}
