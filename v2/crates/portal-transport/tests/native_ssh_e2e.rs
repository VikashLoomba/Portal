//! End-to-end tests: NativeSsh against an in-process russh sshd whose exec
//! runs `sh -c` locally and whose direct-tcpip dials real sockets. This
//! exercises the full stack — handshake, strict known_hosts, pubkey auth,
//! exec/stdin/stderr/exit codes, long-lived streams, and the
//! ListenerForwarder splicing through direct-tcpip channels.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::process::Stdio;
use std::sync::{Arc, Mutex};

use portal_transport::forwarder::{Dialer, ListenerForwarder};
use portal_transport::native_ssh::{HostKeyPolicy, NativeSsh, ResolvedTarget};
use portal_transport::runner::OsRunner;
use portal_transport::{PortForwarder, Transport, TransportError, conformance};
use russh::keys::ssh_key;
use russh::server::{self, Msg, Session};
use russh::{Channel, ChannelId, ChannelMsg};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

// ---------------------------------------------------------------------------
// Test sshd
// ---------------------------------------------------------------------------

struct TestSshd {
    authorized: ssh_key::PublicKey,
    sessions: Arc<Mutex<HashMap<ChannelId, Channel<Msg>>>>,
}

impl server::Handler for TestSshd {
    type Error = russh::Error;

    async fn auth_publickey(
        &mut self,
        _user: &str,
        key: &ssh_key::PublicKey,
    ) -> Result<server::Auth, Self::Error> {
        if *key == self.authorized {
            Ok(server::Auth::Accept)
        } else {
            Ok(server::Auth::reject())
        }
    }

    async fn channel_open_session(
        &mut self,
        channel: Channel<Msg>,
        reply: server::ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        self.sessions.lock().unwrap().insert(channel.id(), channel);
        reply.accept().await;
        Ok(())
    }

    async fn exec_request(
        &mut self,
        channel_id: ChannelId,
        data: &[u8],
        session: &mut Session,
    ) -> Result<(), Self::Error> {
        let cmd = String::from_utf8_lossy(data).into_owned();
        let Some(channel) = self.sessions.lock().unwrap().remove(&channel_id) else {
            return Ok(());
        };
        let handle = session.handle();
        tokio::spawn(async move {
            let _ = handle.channel_success(channel_id).await;
            run_command(cmd, channel).await;
        });
        Ok(())
    }

    async fn channel_open_direct_tcpip(
        &mut self,
        channel: Channel<Msg>,
        host_to_connect: &str,
        port_to_connect: u32,
        _originator_address: &str,
        _originator_port: u32,
        reply: server::ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        let host = host_to_connect.to_string();
        reply.accept().await;
        tokio::spawn(async move {
            match TcpStream::connect((host.as_str(), port_to_connect as u16)).await {
                Ok(mut tcp) => {
                    let mut stream = channel.into_stream();
                    let _ = tokio::io::copy_bidirectional(&mut tcp, &mut stream).await;
                }
                Err(_) => {
                    let _ = channel.close().await; // like sshd: dead target = closed channel
                }
            }
        });
        Ok(())
    }
}

/// Run `sh -c <cmd>` and wire its stdio to the channel (a 60-line sshd).
async fn run_command(cmd: String, channel: Channel<Msg>) {
    let mut child = tokio::process::Command::new("sh")
        .arg("-c")
        .arg(&cmd)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .expect("spawn sh");
    let mut child_stdin = child.stdin.take().unwrap();
    let mut child_stdout = child.stdout.take().unwrap();
    let mut child_stderr = child.stderr.take().unwrap();

    let (mut read_half, write_half) = channel.split();
    let stdin_pump = tokio::spawn(async move {
        while let Some(msg) = read_half.wait().await {
            match msg {
                ChannelMsg::Data { data } => {
                    if child_stdin.write_all(&data).await.is_err() {
                        break;
                    }
                }
                ChannelMsg::Eof | ChannelMsg::Close => break,
                _ => {}
            }
        }
        // Dropping child_stdin delivers EOF to the child.
    });

    let mut out_w = write_half.make_writer();
    let mut err_w = write_half.make_writer_ext(Some(1));
    let _ = tokio::join!(
        tokio::io::copy(&mut child_stdout, &mut out_w),
        tokio::io::copy(&mut child_stderr, &mut err_w),
    );
    let status = child.wait().await.expect("child wait");
    let _ = write_half
        .exit_status(status.code().unwrap_or(1) as u32)
        .await;
    let _ = write_half.eof().await;
    let _ = write_half.close().await;
    stdin_pump.abort();
}

/// Start the sshd on an ephemeral port. Returns (addr, host public key).
async fn start_sshd(authorized: ssh_key::PublicKey) -> (SocketAddr, ssh_key::PublicKey) {
    let host_key =
        russh::keys::PrivateKey::random(&mut rand::rng(), ssh_key::Algorithm::Ed25519).unwrap();
    let host_pub = host_key.public_key().clone();
    let config = Arc::new(server::Config {
        keys: vec![host_key],
        auth_rejection_time: std::time::Duration::from_millis(1),
        auth_rejection_time_initial: Some(std::time::Duration::ZERO),
        inactivity_timeout: None,
        ..Default::default()
    });
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        loop {
            let Ok((socket, _)) = listener.accept().await else {
                return;
            };
            let handler = TestSshd {
                authorized: authorized.clone(),
                sessions: Arc::new(Mutex::new(HashMap::new())),
            };
            let config = config.clone();
            tokio::spawn(async move {
                if let Ok(running) = server::run_stream(config, socket, handler).await {
                    let _ = running.await;
                }
            });
        }
    });
    (addr, host_pub)
}

// ---------------------------------------------------------------------------
// Fixture: client key on disk + known_hosts with the server's key
// ---------------------------------------------------------------------------

struct Fixture {
    _dir: tempfile::TempDir,
    ssh: NativeSsh,
}

async fn fixture() -> Fixture {
    let client_key =
        russh::keys::PrivateKey::random(&mut rand::rng(), ssh_key::Algorithm::Ed25519).unwrap();
    let (addr, host_pub) = start_sshd(client_key.public_key().clone()).await;

    let dir = tempfile::tempdir().unwrap();
    let key_path = dir.path().join("id_ed25519");
    std::fs::write(
        &key_path,
        client_key.to_openssh(ssh_key::LineEnding::LF).unwrap(),
    )
    .unwrap();
    let known_hosts = dir.path().join("known_hosts");
    russh::keys::known_hosts::learn_known_hosts_path(
        "127.0.0.1",
        addr.port(),
        &host_pub,
        &known_hosts,
    )
    .unwrap();

    let ssh = NativeSsh::new("testbox", Arc::new(OsRunner))
        .with_agent(false)
        .with_policy(HostKeyPolicy::KnownHostsPath(known_hosts))
        .with_target(ResolvedTarget {
            hostname: "127.0.0.1".into(),
            user: "test".into(),
            port: addr.port(),
            identity_files: vec![key_path],
            proxy_jump: vec![],
            proxy_command: None,
        });
    Fixture { _dir: dir, ssh }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[tokio::test]
async fn passes_transport_conformance_over_real_ssh() {
    let f = fixture().await;
    assert!(f.ssh.ensure().await.unwrap(), "first ensure builds");
    assert!(!f.ssh.ensure().await.unwrap(), "second ensure is a no-op");
    conformance::exercise(&f.ssh).await;
    assert!(f.ssh.close().await.unwrap());
    let h = f.ssh.health().await.unwrap();
    assert!(!h.up, "closed connection reports down");
}

#[tokio::test]
async fn stream_pipes_stdin_eof_and_exit() {
    let f = fixture().await;
    let mut s = f.ssh.stream(&["cat".to_string()]).await.unwrap();
    s.stdin.write_all(b"over the channel").await.unwrap();
    s.stdin.shutdown().await.unwrap(); // must translate to channel EOF → cat exits
    drop(s.stdin);
    let mut out = Vec::new();
    s.stdout.read_to_end(&mut out).await.unwrap();
    assert_eq!(out, b"over the channel");
    s.wait.await.unwrap().unwrap();
}

#[tokio::test]
async fn stream_nonzero_exit_reported_via_wait() {
    let f = fixture().await;
    let mut s = f
        .ssh
        .stream(&["sh".to_string(), "-c".to_string(), "'exit 7'".to_string()])
        .await
        .unwrap();
    let mut out = Vec::new();
    let _ = s.stdout.read_to_end(&mut out).await;
    match s.wait.await.unwrap() {
        Err(TransportError::Exit { code, .. }) => assert_eq!(code, 7),
        other => panic!("expected Exit(7), got {other:?}"),
    }
}

#[tokio::test]
async fn forwards_splice_through_direct_tcpip() {
    // Real chain: local TCP client -> ListenerForwarder -> direct-tcpip
    // channel -> test sshd -> echo server.
    let echo = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let echo_port = echo.local_addr().unwrap().port();
    tokio::spawn(async move {
        loop {
            let Ok((mut c, _)) = echo.accept().await else {
                return;
            };
            tokio::spawn(async move {
                let (mut r, mut w) = c.split();
                let _ = tokio::io::copy(&mut r, &mut w).await;
            });
        }
    });

    let f = fixture().await;
    let ssh = Arc::new(f.ssh);
    let fwd = ListenerForwarder::new(ssh.clone());

    // Find a free local port by probing forward() itself.
    let mut spec = None;
    for candidate in 21000..21100 {
        match fwd
            .forward(portal_transport::ForwardSpec {
                local: candidate,
                remote: echo_port,
            })
            .await
        {
            Ok(()) => {
                spec = Some(candidate);
                break;
            }
            Err(TransportError::PortInUse { .. }) => continue,
            Err(e) => panic!("forward failed: {e}"),
        }
    }
    let local = spec.expect("no free local port");

    let mut conn = TcpStream::connect(("127.0.0.1", local)).await.unwrap();
    conn.write_all(b"ping through two hops").await.unwrap();
    conn.shutdown().await.unwrap();
    let mut buf = Vec::new();
    conn.read_to_end(&mut buf).await.unwrap();
    assert_eq!(buf, b"ping through two hops");
}

#[tokio::test]
async fn dial_to_dead_remote_port_yields_closed_stream() {
    let f = fixture().await;
    // Port 1 on localhost: nothing listens there.
    let mut stream = f.ssh.dial_remote(1).await.expect("channel opens");
    let mut buf = [0u8; 1];
    let n = stream.read(&mut buf).await.unwrap_or(0);
    assert_eq!(n, 0, "dead target must read as EOF/closed");
}

#[tokio::test]
async fn unknown_host_key_is_rejected_strictly() {
    let client_key =
        russh::keys::PrivateKey::random(&mut rand::rng(), ssh_key::Algorithm::Ed25519).unwrap();
    let (addr, _host_pub) = start_sshd(client_key.public_key().clone()).await;

    let dir = tempfile::tempdir().unwrap();
    let key_path = dir.path().join("id_ed25519");
    std::fs::write(
        &key_path,
        client_key.to_openssh(ssh_key::LineEnding::LF).unwrap(),
    )
    .unwrap();
    let empty_known_hosts = dir.path().join("known_hosts"); // never written

    let ssh = NativeSsh::new("testbox", Arc::new(OsRunner))
        .with_agent(false)
        .with_policy(HostKeyPolicy::KnownHostsPath(empty_known_hosts))
        .with_target(ResolvedTarget {
            hostname: "127.0.0.1".into(),
            user: "test".into(),
            port: addr.port(),
            identity_files: vec![key_path],
            proxy_jump: vec![],
            proxy_command: None,
        });
    let err = ssh.ensure().await.unwrap_err();
    let msg = err.to_string().to_lowercase();
    assert!(
        msg.contains("key") || msg.contains("ssh"),
        "unknown host key must fail closed, got: {msg}"
    );
}

#[tokio::test]
async fn wrong_client_key_fails_auth_loudly() {
    let authorized =
        russh::keys::PrivateKey::random(&mut rand::rng(), ssh_key::Algorithm::Ed25519).unwrap();
    let wrong =
        russh::keys::PrivateKey::random(&mut rand::rng(), ssh_key::Algorithm::Ed25519).unwrap();
    let (addr, host_pub) = start_sshd(authorized.public_key().clone()).await;

    let dir = tempfile::tempdir().unwrap();
    let key_path = dir.path().join("id_ed25519");
    std::fs::write(
        &key_path,
        wrong.to_openssh(ssh_key::LineEnding::LF).unwrap(),
    )
    .unwrap();
    let known_hosts = dir.path().join("known_hosts");
    russh::keys::known_hosts::learn_known_hosts_path(
        "127.0.0.1",
        addr.port(),
        &host_pub,
        &known_hosts,
    )
    .unwrap();

    let ssh = NativeSsh::new("testbox", Arc::new(OsRunner))
        .with_agent(false)
        .with_policy(HostKeyPolicy::KnownHostsPath(known_hosts))
        .with_target(ResolvedTarget {
            hostname: "127.0.0.1".into(),
            user: "test".into(),
            port: addr.port(),
            identity_files: vec![key_path],
            proxy_jump: vec![],
            proxy_command: None,
        });
    let err = ssh.ensure().await.unwrap_err();
    assert!(
        err.to_string().contains("authentication failed"),
        "got: {err}"
    );
}
