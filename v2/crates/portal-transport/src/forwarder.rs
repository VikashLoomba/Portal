//! In-process port forwarding: the daemon OWNS the local listeners.
//!
//! This is the piece that removes v1's fragility wholesale: with ssh -L
//! (ControlMaster) the forwards lived inside the ssh process, so "what is
//! forwarded right now" had to be reconstructed from lsof, forward failures
//! were stderr-substring sniffing, and v2's local≠remote pairs would have
//! needed a persisted mapping table. Here the daemon binds 127.0.0.1/::1
//! itself and splices each accepted connection onto a [`Dialer`] stream
//! (production: a native-ssh direct-tcpip channel; tests: plain TCP):
//!
//! - `list_forwards` is exact in-process ground truth;
//! - a port conflict is a bind error at `forward` time ([`TransportError::
//!   PortInUse`]), not an lsof heuristic;
//! - a daemon restart re-derives everything from the agent snapshot — no
//!   state files, nothing to seed.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::net::TcpListener;
use tokio::task::JoinHandle;

use crate::{ForwardSpec, PortForwarder, TransportError};

/// A bidirectional byte stream to the box-side target.
pub trait ForwardStream: AsyncRead + AsyncWrite + Send + Unpin {}
impl<T: AsyncRead + AsyncWrite + Send + Unpin> ForwardStream for T {}

/// Opens a stream to `localhost:<remote>` ON THE BOX. Production is the
/// native-ssh transport's direct-tcpip channel opener.
#[async_trait]
pub trait Dialer: Send + Sync {
    async fn dial_remote(&self, remote: u16) -> Result<Box<dyn ForwardStream>, TransportError>;
}

struct Active {
    remote: u16,
    tasks: Vec<JoinHandle<()>>,
}

/// The production `PortForwarder`: local listeners + splice-per-connection.
pub struct ListenerForwarder {
    dialer: Arc<dyn Dialer>,
    active: Mutex<HashMap<u16, Active>>,
}

impl ListenerForwarder {
    pub fn new(dialer: Arc<dyn Dialer>) -> Self {
        Self {
            dialer,
            active: Mutex::new(HashMap::new()),
        }
    }

    async fn bind(local: u16) -> Result<(TcpListener, Option<TcpListener>), TransportError> {
        // IPv4 loopback is required; ::1 is best-effort — matches `ssh -L`
        // binding both loopbacks when available.
        let v4 = match TcpListener::bind(("127.0.0.1", local)).await {
            Ok(l) => l,
            // Both mean "this local port is not available to us": AddrInUse is
            // another holder, PermissionDenied is a privileged port we cannot
            // bind as a user agent. Callers treat them the same way — pick a
            // different local port — so don't make them special-case Io.
            Err(e)
                if matches!(
                    e.kind(),
                    std::io::ErrorKind::AddrInUse | std::io::ErrorKind::PermissionDenied
                ) =>
            {
                return Err(TransportError::PortInUse { local });
            }
            Err(e) => return Err(TransportError::Io(e)),
        };
        let v6 = TcpListener::bind(("::1", local)).await.ok();
        Ok((v4, v6))
    }

    fn spawn_accept_loop(
        listener: TcpListener,
        dialer: Arc<dyn Dialer>,
        remote: u16,
    ) -> JoinHandle<()> {
        tokio::spawn(async move {
            loop {
                let Ok((mut conn, _peer)) = listener.accept().await else {
                    return; // listener closed
                };
                let dialer = dialer.clone();
                tokio::spawn(async move {
                    match dialer.dial_remote(remote).await {
                        Ok(mut stream) => {
                            // Errors mid-splice just end the connection —
                            // identical to what ssh does with a broken channel.
                            let _ = tokio::io::copy_bidirectional(&mut conn, &mut stream).await;
                        }
                        Err(err) => {
                            // Channel-open failure: drop the client conn (RST),
                            // like ssh answering channel-open-failure.
                            tracing::debug!(target: "portal::forward", remote, %err,
                                "remote dial failed; dropping local connection");
                        }
                    }
                });
            }
        })
    }
}

#[async_trait]
impl PortForwarder for ListenerForwarder {
    async fn forward(&self, spec: ForwardSpec) -> Result<(), TransportError> {
        {
            let active = self.active.lock().unwrap();
            if let Some(a) = active.get(&spec.local) {
                if a.remote == spec.remote {
                    return Ok(()); // idempotent re-add
                }
                return Err(TransportError::Forward {
                    local: spec.local,
                    remote: spec.remote,
                    stderr: format!("local port already mapped to remote {}", a.remote),
                });
            }
        }
        let (v4, v6) = Self::bind(spec.local).await?;
        let mut tasks = vec![Self::spawn_accept_loop(
            v4,
            self.dialer.clone(),
            spec.remote,
        )];
        if let Some(v6) = v6 {
            tasks.push(Self::spawn_accept_loop(
                v6,
                self.dialer.clone(),
                spec.remote,
            ));
        }
        self.active.lock().unwrap().insert(
            spec.local,
            Active {
                remote: spec.remote,
                tasks,
            },
        );
        Ok(())
    }

    async fn cancel(&self, spec: ForwardSpec) -> Result<(), TransportError> {
        if let Some(a) = self.active.lock().unwrap().remove(&spec.local) {
            for t in a.tasks {
                t.abort(); // drops the listener; in-flight splices finish alone
            }
        }
        Ok(())
    }

    async fn list_forwards(&self) -> Result<Vec<ForwardSpec>, TransportError> {
        let mut out: Vec<ForwardSpec> = self
            .active
            .lock()
            .unwrap()
            .iter()
            .map(|(&local, a)| ForwardSpec {
                local,
                remote: a.remote,
            })
            .collect();
        out.sort();
        Ok(out)
    }
}

impl Drop for ListenerForwarder {
    fn drop(&mut self) {
        for (_, a) in self.active.lock().unwrap().drain() {
            for t in a.tasks {
                t.abort();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpStream;

    /// Test dialer: "the box" is this machine — dial loopback directly.
    struct LoopbackDialer;

    #[async_trait]
    impl Dialer for LoopbackDialer {
        async fn dial_remote(&self, remote: u16) -> Result<Box<dyn ForwardStream>, TransportError> {
            let s = TcpStream::connect(("127.0.0.1", remote)).await?;
            Ok(Box::new(s))
        }
    }

    async fn spawn_echo_server() -> u16 {
        let l = TcpListener::bind(("127.0.0.1", 0)).await.unwrap();
        let port = l.local_addr().unwrap().port();
        tokio::spawn(async move {
            loop {
                let Ok((mut c, _)) = l.accept().await else {
                    return;
                };
                tokio::spawn(async move {
                    let (mut r, mut w) = c.split();
                    let _ = tokio::io::copy(&mut r, &mut w).await;
                });
            }
        });
        port
    }

    /// Forward `remote` on some free local port, dodging the concurrent-test
    /// ephemeral reuse race by retrying on PortInUse.
    async fn forward_on_free_port(fwd: &ListenerForwarder, remote: u16) -> ForwardSpec {
        let mut seed = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .subsec_nanos() as u16;
        for _ in 0..64 {
            let local = 20000 + (seed % 40000);
            seed = seed.wrapping_mul(31).wrapping_add(17);
            let spec = ForwardSpec { local, remote };
            match fwd.forward(spec).await {
                Ok(()) => return spec,
                Err(TransportError::PortInUse { .. }) => continue,
                Err(e) => panic!("forward failed: {e}"),
            }
        }
        panic!("no free local port found");
    }

    #[tokio::test]
    async fn forwards_and_splices_bytes() {
        let echo = spawn_echo_server().await;
        let fwd = ListenerForwarder::new(Arc::new(LoopbackDialer));
        let spec = forward_on_free_port(&fwd, echo).await;
        let local = spec.local;
        assert_eq!(fwd.list_forwards().await.unwrap(), vec![spec]);

        let mut conn = TcpStream::connect(("127.0.0.1", local)).await.unwrap();
        conn.write_all(b"through the tunnel").await.unwrap();
        conn.shutdown().await.unwrap();
        let mut buf = Vec::new();
        conn.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"through the tunnel");

        // Idempotent re-add of the same pair.
        fwd.forward(spec).await.unwrap();
        assert_eq!(fwd.list_forwards().await.unwrap().len(), 1);

        fwd.cancel(spec).await.unwrap();
        assert!(fwd.list_forwards().await.unwrap().is_empty());
        // The listener is gone: a fresh connect must fail.
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        assert!(TcpStream::connect(("127.0.0.1", local)).await.is_err());
    }

    #[tokio::test]
    async fn bind_conflict_is_port_in_use() {
        let holder = TcpListener::bind(("127.0.0.1", 0)).await.unwrap();
        let held = holder.local_addr().unwrap().port();
        let fwd = ListenerForwarder::new(Arc::new(LoopbackDialer));
        match fwd
            .forward(ForwardSpec {
                local: held,
                remote: 8000,
            })
            .await
        {
            Err(TransportError::PortInUse { local }) => assert_eq!(local, held),
            other => panic!("expected PortInUse, got {other:?}"),
        }
        assert!(fwd.list_forwards().await.unwrap().is_empty());
    }

    /// A dialer whose channel-open always fails (dead remote), without
    /// port-reuse hazards.
    struct FailingDialer;

    #[async_trait]
    impl Dialer for FailingDialer {
        async fn dial_remote(&self, remote: u16) -> Result<Box<dyn ForwardStream>, TransportError> {
            Err(TransportError::Forward {
                local: 0,
                remote,
                stderr: "channel open failed".into(),
            })
        }
    }

    #[tokio::test]
    async fn remote_dial_failure_drops_connection_but_keeps_forward() {
        let fwd = ListenerForwarder::new(Arc::new(FailingDialer));
        let spec = forward_on_free_port(&fwd, 59999).await;

        let mut conn = TcpStream::connect(("127.0.0.1", spec.local)).await.unwrap();
        let mut buf = [0u8; 1];
        // Connection is accepted then dropped (dial failed) → EOF/reset.
        let n = conn.read(&mut buf).await.unwrap_or(0);
        assert_eq!(n, 0);
        assert_eq!(fwd.list_forwards().await.unwrap(), vec![spec]);
    }
}
