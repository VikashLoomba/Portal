//! The both-halves test: portald's agent serve loop (real store, fake port
//! source) driven by portal-core's session client over duplex pipes — the
//! same code paths a production Mac↔box session exercises, in one process.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use portal_proto::PROTO_VERSION;
use portal_proto::codec::asynchronous::{read_frame, write_frame};
use portal_proto::envelope::Envelope;
use portal_proto::messages::{
    ClipSyncAck, ClipSyncUpdate, Hello, Ping, Snapshot, Subscribe, marshal_payload,
    unmarshal_payload,
};
use portald::agent::watcher::ListenerSource;
use portald::agent::{Agent, AgentConfig, Relay};
use portald::store::{ClipKind, ClipStore};
use tokio::io::duplex;
use tokio::sync::mpsc;

#[derive(Clone, Default)]
struct FakePorts(Arc<Mutex<Vec<u16>>>);

impl ListenerSource for FakePorts {
    fn listening(&mut self) -> Vec<portal_proto::messages::Port> {
        self.0
            .lock()
            .unwrap()
            .iter()
            .map(|&p| portal_proto::messages::Port {
                port: p,
                family: 4,
                addr: "127.0.0.1".into(),
                inode_ns: 0,
            })
            .collect()
    }
}

struct Rig {
    ports: FakePorts,
    store_dir: tempfile::TempDir,
    relay: mpsc::Sender<Relay>,
    client_in: tokio::io::DuplexStream,
    client_out: tokio::io::DuplexStream,
    agent_task: tokio::task::JoinHandle<()>,
}

fn rig() -> Rig {
    let ports = FakePorts::default();
    let store_dir = tempfile::tempdir().unwrap();
    let store = ClipStore::new(store_dir.path().join("clip"));
    let (relay_tx, relay_rx) = mpsc::channel(8);
    let cfg = AgentConfig {
        git_sha: "cafe".into(),
        poll_interval: Duration::from_millis(10),
        heartbeat_interval: Duration::from_millis(200),
        ..AgentConfig::default()
    };
    let mut agent = Agent::new(cfg, ports.clone(), store, relay_rx);
    let (client_in, agent_stdin) = duplex(256 * 1024);
    let (mut agent_stdout, client_out) = duplex(256 * 1024);
    let agent_task = tokio::spawn(async move {
        let _ = agent.serve(agent_stdin, &mut agent_stdout).await;
    });
    Rig {
        ports,
        store_dir,
        relay: relay_tx,
        client_in,
        client_out,
        agent_task,
    }
}

async fn handshake(rig: &mut Rig) {
    write_frame(
        &mut rig.client_in,
        &Envelope::of_hello(Hello {
            proto_version: PROTO_VERSION,
            client_git_sha: "cafe".into(),
            client_pid: 1,
            poll_interval_ms: 0,
            want_destroy_mc: true,
            services: Some(
                [
                    ("clipsync".to_string(), 1u32),
                    ("notify".to_string(), 1),
                    ("openurl".to_string(), 1),
                    ("cred".to_string(), 1),
                ]
                .into_iter()
                .collect(),
            ),
            box_name: Some("devbox1".into()),
        }),
    )
    .await
    .unwrap();
    let ack = read_frame(&mut rig.client_out)
        .await
        .unwrap()
        .hello_ack
        .unwrap();
    assert_eq!(ack.agent_git_sha, "cafe");
    assert!(ack.services.unwrap().contains_key("clipsync"));
}

async fn subscribe(rig: &mut Rig, rsid: u64) -> Snapshot {
    write_frame(
        &mut rig.client_in,
        &Envelope::of_subscribe(Subscribe {
            deny: vec![22],
            allow: vec![],
            exclude_ephemeral: true,
            resubscribe_id: rsid,
        }),
    )
    .await
    .unwrap();
    // Heartbeats may interleave with the ack/snapshot; skip them.
    let ack = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(a) = env.subscribe_ack {
            break a;
        }
    };
    assert_eq!(ack.resubscribe_id, rsid);
    loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(s) = env.snapshot {
            break s;
        }
    }
}

#[tokio::test]
async fn ports_flow_snapshot_then_deltas() {
    let mut rig = rig();
    rig.ports.0.lock().unwrap().extend([8000, 22, 45000]); // 22 denied, 45000 ephemeral
    handshake(&mut rig).await;
    let snap = subscribe(&mut rig, 1).await;
    let ports: Vec<u16> = snap.ports.iter().map(|p| p.port).collect();
    assert_eq!(ports, vec![8000], "deny + ephemeral filtered");

    // A new listener appears → PortAdded with seq > snapshot.
    rig.ports.0.lock().unwrap().push(3000);
    let added = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(pa) = env.port_added {
            break pa;
        } // heartbeats interleave
    };
    assert_eq!(added.port.port, 3000);
    assert!(added.seq > snap.seq);

    // It disappears → PortRemoved.
    rig.ports.0.lock().unwrap().retain(|&p| p != 3000);
    let removed = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(pr) = env.port_removed {
            break pr;
        }
    };
    assert_eq!(removed.port, 3000);
    assert!(removed.seq > added.seq);
    rig.agent_task.abort();
}

#[tokio::test]
async fn clipsync_blob_roundtrip_through_the_agent() {
    let mut rig = rig();
    handshake(&mut rig).await;
    let _ = subscribe(&mut rig, 1).await;

    let img = {
        let mut v = portald::store::PNG_MAGIC.to_vec();
        v.extend_from_slice(b"image body");
        v
    };
    let sha = portald::store::sha256_hex(&img);

    // Update referencing a blob the box doesn't have → ack{have_blob:false}.
    let update = ClipSyncUpdate {
        change_id: 1,
        kind: "image".into(),
        format: Some("png".into()),
        sha: Some(sha.clone()),
        size: Some(img.len() as i64),
        inline: None,
    };
    send_msg(&mut rig, "update", marshal_payload(&update).unwrap()).await;
    let ack: ClipSyncAck = recv_ack(&mut rig).await;
    assert!(!ack.have_blob);

    // Blob arrives out-of-band (the Mac's exec path lands in the same store).
    let store = ClipStore::new(rig.store_dir.path().join("clip"));
    store.put_blob(&sha, &img).unwrap();

    // Re-send → applied → ack{have_blob:true}; paste now serves the image.
    send_msg(&mut rig, "update", marshal_payload(&update).unwrap()).await;
    let ack: ClipSyncAck = recv_ack(&mut rig).await;
    assert!(ack.have_blob);
    assert_eq!(store.paste(ClipKind::Image).unwrap(), img);

    // Inline text sails through in one frame.
    let update = ClipSyncUpdate {
        change_id: 2,
        kind: "text".into(),
        format: None,
        sha: Some(portald::store::sha256_hex(b"hello")),
        size: Some(5),
        inline: Some(serde_bytes::ByteBuf::from(&b"hello"[..])),
    };
    send_msg(&mut rig, "update", marshal_payload(&update).unwrap()).await;
    let ack: ClipSyncAck = recv_ack(&mut rig).await;
    assert!(ack.have_blob);
    assert_eq!(store.paste(ClipKind::Text).unwrap(), b"hello");
    rig.agent_task.abort();
}

#[tokio::test]
async fn heartbeats_arrive_without_pings() {
    let mut rig = rig(); // heartbeat_interval = 200ms in the rig
    handshake(&mut rig).await;
    let _ = subscribe(&mut rig, 1).await;
    // No pings, no port churn: heartbeats must arrive on their own.
    let mut heartbeats = 0;
    let deadline = tokio::time::Instant::now() + Duration::from_millis(900);
    while tokio::time::Instant::now() < deadline {
        match tokio::time::timeout(Duration::from_millis(300), read_frame(&mut rig.client_out)).await
        {
            Ok(Ok(env)) if env.heartbeat.is_some() => heartbeats += 1,
            Ok(Ok(_)) => {}
            Ok(Err(e)) => panic!("frame error: {e}"),
            Err(_) => break, // silence
        }
    }
    assert!(heartbeats >= 2, "expected periodic heartbeats, got {heartbeats}");
    rig.agent_task.abort();
}

#[tokio::test]
async fn dribbled_frames_survive_tick_races() {
    use tokio::io::AsyncWriteExt;
    let mut rig = rig(); // poll_interval = 10ms: ticks race every read
    handshake(&mut rig).await;
    let _ = subscribe(&mut rig, 1).await;

    // Dribble a re-Subscribe byte-by-byte with pauses long enough for
    // several poll/heartbeat ticks to win the serve loop's select! while
    // the frame is mid-read. With a bare `read_frame(stdin)` in the select
    // (not cancel-safe), the partial bytes die with the cancelled future
    // and the stream desyncs to BadMagic; the dedicated reader task must
    // hold frame state across races.
    let buf = portal_proto::codec::encode_frame(&Envelope::of_subscribe(Subscribe {
        deny: vec![],
        allow: vec![],
        exclude_ephemeral: false,
        resubscribe_id: 2,
    }))
    .unwrap();
    for byte in &buf {
        rig.client_in.write_all(std::slice::from_ref(byte)).await.unwrap();
        rig.client_in.flush().await.unwrap();
        tokio::time::sleep(Duration::from_millis(15)).await;
    }

    // The agent must still parse it: SubscribeAck{rsid=2}, then Snapshot.
    let ack = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(a) = env.subscribe_ack {
            break a;
        }
    };
    assert_eq!(ack.resubscribe_id, 2);
    loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if env.snapshot.is_some() {
            break;
        }
    }
    rig.agent_task.abort();
}

#[tokio::test]
async fn ping_echoes_and_relay_reaches_client() {
    let mut rig = rig();
    handshake(&mut rig).await;
    let _ = subscribe(&mut rig, 1).await;

    write_frame(
        &mut rig.client_in,
        &Envelope::of_ping(Ping { nonce: 0xBEEF }),
    )
    .await
    .unwrap();
    let hb = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(hb) = env.heartbeat
            && hb.nonce.is_some()
        {
            break hb;
        }
    };
    assert_eq!(hb.nonce, Some(0xBEEF));

    // A notify relayed from the cmd socket path reaches the client as a Msg.
    rig.relay
        .send(Relay::Notify(portal_proto::messages::Notify {
            title: "hi".into(),
            body: None,
            subtitle: None,
            urgency: Some(0),
            verified: Some(true),
            source: Some("claude_hook".into()),
            sound: None,
        }))
        .await
        .unwrap();
    let msg = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(m) = env.msg {
            break m;
        }
    };
    assert_eq!(
        (msg.service.as_str(), msg.kind.as_str()),
        ("notify", "event")
    );
    rig.agent_task.abort();
}

#[tokio::test]
async fn proto_mismatch_is_fatal_and_loud() {
    let mut rig = rig();
    write_frame(
        &mut rig.client_in,
        &Envelope::of_hello(Hello {
            proto_version: 3,
            client_git_sha: "old".into(),
            client_pid: 1,
            poll_interval_ms: 0,
            want_destroy_mc: false,
            services: None,
            box_name: None,
        }),
    )
    .await
    .unwrap();
    let err = read_frame(&mut rig.client_out)
        .await
        .unwrap()
        .agent_error
        .unwrap();
    assert_eq!(err.code, portal_proto::code::PROTOCOL_MISMATCH);
    assert!(err.fatal);
    let _ = rig.agent_task.await; // agent exits
}

async fn send_msg(rig: &mut Rig, kind: &str, payload: ciborium::Value) {
    write_frame(
        &mut rig.client_in,
        &Envelope::of_msg(portal_proto::messages::Msg {
            service: "clipsync".into(),
            kind: kind.into(),
            seq: None,
            payload: Some(payload),
        }),
    )
    .await
    .unwrap();
}

async fn recv_ack(rig: &mut Rig) -> ClipSyncAck {
    loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(m) = env.msg
            && m.service == "clipsync"
            && m.kind == "ack"
        {
            return unmarshal_payload(m.payload.as_ref().unwrap()).unwrap();
        }
    }
}

#[tokio::test]
async fn cred_flow_shim_to_mac_and_back() {
    let mut rig = rig();
    handshake(&mut rig).await; // client advertises "cred" in the rig? — no: add it there
    let _ = subscribe(&mut rig, 1).await;

    // Shim side: a keychain request arrives over the relay channel.
    let (reply_tx, reply_rx) = tokio::sync::oneshot::channel();
    rig.relay
        .send(Relay::Cred {
            req: portald::cred::CredShimReq {
                label: "sudo".into(),
                requester: "pid 7: sudo askpass".into(),
                mode: "askpass".into(),
                target: "Password:".into(),
            },
            reply: reply_tx,
        })
        .await
        .unwrap();

    // Mac side: the CredRequest lands as a Msg; answer it.
    let msg = loop {
        let env = read_frame(&mut rig.client_out).await.unwrap();
        if let Some(m) = env.msg
            && m.service == "cred"
        {
            break m;
        }
    };
    let req: portal_proto::messages::CredRequest =
        unmarshal_payload(msg.payload.as_ref().unwrap()).unwrap();
    assert_eq!(req.label, "sudo");
    assert_eq!(req.mode, "askpass");
    write_frame(
        &mut rig.client_in,
        &Envelope::of_msg(portal_proto::messages::Msg {
            service: "cred".into(),
            kind: "resp".into(),
            seq: None,
            payload: Some(
                marshal_payload(&portal_proto::messages::CredResponse {
                    nonce: req.nonce,
                    epoch: req.epoch,
                    ok: true,
                    secret: Some(serde_bytes::ByteBuf::from(&b"s3kr3t"[..])),
                    err: None,
                })
                .unwrap(),
            ),
        }),
    )
    .await
    .unwrap();

    // Shim side: the secret comes back through the oneshot.
    let secret = tokio::time::timeout(Duration::from_secs(5), reply_rx)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert_eq!(secret, b"s3kr3t");
    rig.agent_task.abort();
}
