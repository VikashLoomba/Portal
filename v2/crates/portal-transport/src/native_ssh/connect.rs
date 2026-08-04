//! Connection building blocks: host-key policy, authentication, and the
//! ProxyCommand stream wrapper.

use std::path::PathBuf;
use std::process::Stdio;
use std::sync::Arc;
use std::task::{Context, Poll};

use russh::client;
use russh::keys::ssh_key;
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

use crate::TransportError;

/// How server host keys are verified. STRICT by default — v1 doctrine: the
/// daemon connects headlessly and must never learn keys implicitly.
#[derive(Debug, Clone, Default)]
pub enum HostKeyPolicy {
    /// ~/.ssh/known_hosts (standard location).
    #[default]
    KnownHosts,
    /// Explicit known_hosts file (tests, hermetic setups).
    KnownHostsPath(PathBuf),
    /// Accept anything. TEST-ONLY: never wire into production composition.
    DangerouslyAcceptAny,
}

pub(crate) struct PortalClientHandler {
    pub hostname: String,
    pub port: u16,
    pub policy: HostKeyPolicy,
}

impl client::Handler for PortalClientHandler {
    type Error = russh::Error;

    async fn check_server_key(&mut self, key: &ssh_key::PublicKey) -> Result<bool, Self::Error> {
        let checked = match &self.policy {
            HostKeyPolicy::DangerouslyAcceptAny => return Ok(true),
            HostKeyPolicy::KnownHosts => {
                russh::keys::check_known_hosts(&self.hostname, self.port, key)
            }
            HostKeyPolicy::KnownHostsPath(path) => {
                russh::keys::check_known_hosts_path(&self.hostname, self.port, key, path)
            }
        };
        match checked {
            Ok(true) => Ok(true),
            Ok(false) => {
                tracing::error!(target: "portal::ssh", host = %self.hostname, port = self.port,
                    "host key is not in known_hosts; connect once with `ssh {}` to learn it",
                    self.hostname);
                Ok(false)
            }
            Err(err) => {
                // KeyChanged lands here: LOUD, and never auto-accepted.
                tracing::error!(target: "portal::ssh", host = %self.hostname, port = self.port,
                    "HOST KEY VERIFICATION FAILED (possible MITM): {err}");
                Ok(false)
            }
        }
    }
}

/// Authenticate `handle`: ssh-agent identities first (SSH_AUTH_SOCK), then
/// on-disk identity files (unencrypted; encrypted keys are skipped with a
/// warning — the daemon is headless, `portal install` owns interactive
/// validation). Never prompts.
pub(crate) async fn authenticate(
    handle: &mut client::Handle<PortalClientHandler>,
    user: &str,
    identity_files: &[PathBuf],
    use_agent: bool,
) -> Result<(), TransportError> {
    let mut rsa_hash: Option<Option<ssh_key::HashAlg>> = None;
    let mut tried = 0usize;

    if use_agent && std::env::var_os("SSH_AUTH_SOCK").is_some() {
        match russh::keys::agent::client::AgentClient::connect_env().await {
            Ok(mut agent) => match agent.request_identities().await {
                Ok(identities) => {
                    for identity in identities {
                        // Certificates need authenticate_certificate_with;
                        // out of scope until a cert-based setup shows up.
                        let russh::keys::agent::AgentIdentity::PublicKey { key, .. } = identity
                        else {
                            continue;
                        };
                        tried += 1;
                        let hash = hash_for(handle, &key.algorithm(), &mut rsa_hash).await;
                        match handle
                            .authenticate_publickey_with(user, key, hash, &mut agent)
                            .await
                        {
                            Ok(res) if res.success() => return Ok(()),
                            Ok(_) => {}
                            Err(err) => {
                                tracing::debug!(target: "portal::ssh", %err, "agent key attempt failed");
                            }
                        }
                    }
                }
                Err(err) => {
                    tracing::warn!(target: "portal::ssh", %err, "ssh-agent identity listing failed");
                }
            },
            Err(err) => {
                tracing::warn!(target: "portal::ssh", %err, "ssh-agent connect failed");
            }
        }
    }

    for path in identity_files {
        if !path.exists() {
            continue;
        }
        let key = match russh::keys::load_secret_key(path, None) {
            Ok(k) => k,
            Err(err) => {
                tracing::warn!(target: "portal::ssh", path = %path.display(), %err,
                    "skipping identity file (encrypted or unreadable)");
                continue;
            }
        };
        tried += 1;
        let hash = hash_for(handle, &key.algorithm(), &mut rsa_hash).await;
        match handle
            .authenticate_publickey(
                user,
                russh::keys::PrivateKeyWithHashAlg::new(Arc::new(key), hash),
            )
            .await
        {
            Ok(res) if res.success() => return Ok(()),
            Ok(_) => {}
            Err(err) => {
                tracing::debug!(target: "portal::ssh", path = %path.display(), %err,
                    "identity file attempt failed");
            }
        }
    }

    Err(TransportError::Ssh(format!(
        "authentication failed for {user} ({tried} keys tried); \
         key-based passwordless ssh is required (ssh-copy-id)"
    )))
}

/// RSA needs a negotiated signature hash (rsa-sha2-*); other algorithms don't.
/// The server extension query waits up to 1s once — cache it per connection.
async fn hash_for(
    handle: &client::Handle<PortalClientHandler>,
    algorithm: &ssh_key::Algorithm,
    cache: &mut Option<Option<ssh_key::HashAlg>>,
) -> Option<ssh_key::HashAlg> {
    if !matches!(algorithm, ssh_key::Algorithm::Rsa { .. }) {
        return None;
    }
    if cache.is_none() {
        *cache = Some(
            handle
                .best_supported_rsa_hash()
                .await
                .ok()
                .flatten()
                .flatten(),
        );
    }
    cache.unwrap_or(None)
}

/// ProxyCommand transport: the spawned command's stdio IS the SSH byte
/// stream (`%h`/`%p` substituted like OpenSSH).
pub(crate) struct CommandStream {
    // Held for kill_on_drop.
    _child: tokio::process::Child,
    stdin: tokio::process::ChildStdin,
    stdout: tokio::process::ChildStdout,
}

impl CommandStream {
    pub fn spawn(command: &str, host: &str, port: u16) -> Result<Self, TransportError> {
        let cmd = command.replace("%h", host).replace("%p", &port.to_string());
        let mut child = tokio::process::Command::new("sh")
            .arg("-c")
            .arg(&cmd)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit()) // proxy tools print auth hints on stderr
            .kill_on_drop(true)
            .spawn()?;
        let stdin = child.stdin.take().expect("piped stdin");
        let stdout = child.stdout.take().expect("piped stdout");
        Ok(Self {
            _child: child,
            stdin,
            stdout,
        })
    }
}

impl AsyncRead for CommandStream {
    fn poll_read(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.stdout).poll_read(cx, buf)
    }
}

impl AsyncWrite for CommandStream {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        std::pin::Pin::new(&mut self.stdin).poll_write(cx, buf)
    }
    fn poll_flush(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.stdin).poll_flush(cx)
    }
    fn poll_shutdown(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.stdin).poll_shutdown(cx)
    }
}
