//! Integration test: the agent client speaking real v4 frames against an
//! in-process fake agent over duplex pipes.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use portal_core::agentclient::session::{
    Bootstrapper, Client, ClientConfig, Filter, Outbound, SessionError,
};
use portal_core::agentclient::{Event, EventChannels, ServiceRequest};
use portal_proto::codec::asynchronous::{read_frame, write_frame};
use portal_proto::envelope::Envelope;
use portal_proto::messages::{
    ClipResponse, HelloAck, Msg, Notify, Port, PortAdded, Snapshot, SubscribeAck, marshal_payload,
};
use portal_transport::testing::{FakeTransport, duplex_session};
use tokio::sync::{mpsc, watch};

struct FakeBootstrap;

#[async_trait::async_trait]
impl Bootstrapper for FakeBootstrap {
    async fn ensure_uploaded(&self) -> Result<String, String> {
        Ok("~/.cache/portal/agent-cafe".into())
    }
    fn embedded_sha(&self) -> String {
        "cafe".into()
    }
    fn set_boot_id(&self, _id: &str) {}
    async fn ensure_box_converged(&self) -> Result<(), String> {
        Ok(())
    }
}

fn ack(sha: &str) -> HelloAck {
    HelloAck {
        proto_version: 4,
        agent_git_sha: sha.into(),
        agent_pid: 1,
        kernel: "Linux test".into(),
        boot_id: "boot-1".into(),
        ephem_min: 32768,
        ephem_max: 60999,
        now_unix_nano: 0,
        services: Some(BTreeMap::from([
            ("clip".to_string(), 1),
            ("clipwrite".to_string(), 1),
            ("cred".to_string(), 1),
            ("notify".to_string(), 1),
            ("openurl".to_string(), 1),
        ])),
    }
}

fn port(p: u16) -> Port {
    Port {
        port: p,
        family: 4,
        addr: "127.0.0.1".into(),
        inode_ns: 0,
    }
}

fn client_for(
    t: Arc<FakeTransport>,
    cfg: ClientConfig,
    filter: Filter,
) -> (
    Client,
    EventChannels,
    mpsc::Sender<Outbound>,
    watch::Sender<Filter>,
) {
    let ch = EventChannels::new();
    let (ftx, frx) = watch::channel(filter);
    let (otx, orx) = mpsc::channel(4);
    let client = Client::new(t, Arc::new(FakeBootstrap), cfg, ch.sinks.clone(), frx, orx);
    (client, ch, otx, ftx)
}

#[tokio::test]
async fn full_session_flow() {
    let t = FakeTransport::new("devbox1");
    let (sess, mut agent) = duplex_session(64 * 1024);
    t.push_session(sess);
    let cfg = ClientConfig {
        coalesce_window: Duration::from_millis(20),
        ..ClientConfig::default()
    };
    let filter = Filter {
        deny: vec![22],
        allow: vec![9000],
        exclude_ephemeral: true,
    };
    let (mut client, mut ch, otx, _ftx) = client_for(t.clone(), cfg, filter);

    // A handler response queued before the session: must ride the pipe.
    otx.send(
        Outbound::clip_response(&ClipResponse {
            nonce: 1,
            epoch: 1,
            ok: true,
            has: None,
            kind: None,
            sha: Some("ab".repeat(16)),
            err: None,
        })
        .unwrap(),
    )
    .await
    .unwrap();

    let agent_task = tokio::spawn(async move {
        let hello = read_frame(&mut agent.stdin).await.unwrap().hello.unwrap();
        write_frame(&mut agent.stdout, &Envelope::of_hello_ack(ack("cafe")))
            .await
            .unwrap();
        let sub = read_frame(&mut agent.stdin)
            .await
            .unwrap()
            .subscribe
            .unwrap();
        write_frame(
            &mut agent.stdout,
            &Envelope::of_subscribe_ack(SubscribeAck {
                resubscribe_id: sub.resubscribe_id,
            }),
        )
        .await
        .unwrap();
        write_frame(
            &mut agent.stdout,
            &Envelope::of_snapshot(Snapshot {
                seq: 10,
                generated_at: 1,
                ports: vec![port(8000)],
            }),
        )
        .await
        .unwrap();
        for (seq, p) in [(11, 5173), (12, 5174)] {
            write_frame(
                &mut agent.stdout,
                &Envelope::of_port_added(PortAdded {
                    seq,
                    port: port(p),
                    at: 2,
                }),
            )
            .await
            .unwrap();
        }
        // Stale event from a previous agent session: seq <= snapshot seq.
        write_frame(
            &mut agent.stdout,
            &Envelope::of_port_added(PortAdded {
                seq: 9,
                port: port(9999),
                at: 2,
            }),
        )
        .await
        .unwrap();
        // The queued clip response arrives after Subscribe.
        let msg = read_frame(&mut agent.stdin).await.unwrap().msg.unwrap();
        // Notify service frame → dedicated channel.
        write_frame(
            &mut agent.stdout,
            &Envelope::of_msg(Msg {
                service: "notify".into(),
                kind: "event".into(),
                seq: Some(7),
                payload: Some(
                    marshal_payload(&Notify {
                        title: "build done".into(),
                        body: None,
                        subtitle: None,
                        urgency: Some(0),
                        verified: Some(true),
                        source: Some("claude_hook".into()),
                        sound: None,
                    })
                    .unwrap(),
                ),
            }),
        )
        .await
        .unwrap();
        tokio::time::sleep(Duration::from_millis(120)).await; // let coalesce flush
        write_frame(&mut agent.stdout, &Envelope::of_bye(Default::default()))
            .await
            .unwrap();
        (hello, sub, msg)
    });

    let err = client.run_once().await.unwrap_err();
    assert!(matches!(err, SessionError::Bye), "got {err}");

    let (hello, sub, msg) = agent_task.await.unwrap();
    assert_eq!(hello.proto_version, portal_proto::PROTO_VERSION);
    assert_eq!(hello.client_git_sha, "cafe");
    assert!(hello.services.unwrap().contains_key("clipsync"));
    assert_eq!(sub.deny, vec![22]);
    assert_eq!(sub.allow, vec![9000]);
    assert!(sub.exclude_ephemeral);
    assert_eq!(sub.resubscribe_id, 1);
    assert_eq!((msg.service.as_str(), msg.kind.as_str()), ("clip", "resp"));

    // Engine events: Connected → SnapshotReplaced → ONE coalesced Delta.
    assert!(matches!(ch.engine.recv().await, Some(Event::Connected)));
    assert!(matches!(
        ch.engine.recv().await,
        Some(Event::SnapshotReplaced)
    ));
    match ch.engine.recv().await {
        Some(Event::Delta { added, removed }) => {
            assert_eq!(added, vec![5173, 5174]);
            assert!(removed.is_empty());
        }
        other => panic!("expected Delta, got {other:?}"),
    }

    // Notify rides its dedicated channel with the Msg seq attached.
    match ch.notify.recv().await {
        Some(ServiceRequest::Notify { notify, seq }) => {
            assert_eq!(notify.title, "build done");
            assert_eq!(seq, 7);
        }
        other => panic!("expected Notify, got {other:?}"),
    }

    // The snapshot cache reflects snapshot + fresh deltas (stale 9999 dropped).
    assert_eq!(
        client.snapshot.desired_ports(),
        Some(vec![5173, 5174, 8000])
    );
}

#[tokio::test(start_paused = true)]
async fn heartbeat_timeout_ends_session() {
    let t = FakeTransport::new("devbox1");
    let (sess, mut agent) = duplex_session(64 * 1024);
    t.push_session(sess);
    let (mut client, _ch, _otx, _ftx) =
        client_for(t.clone(), ClientConfig::default(), Filter::default());

    let agent_task = tokio::spawn(async move {
        let _hello = read_frame(&mut agent.stdin).await.unwrap();
        write_frame(&mut agent.stdout, &Envelope::of_hello_ack(ack("cafe")))
            .await
            .unwrap();
        let _sub = read_frame(&mut agent.stdin).await.unwrap();
        // Then: silence. The client's 12s watchdog must fire (auto-advanced
        // by the paused clock).
        std::future::pending::<()>().await;
    });

    let err = client.run_once().await.unwrap_err();
    assert!(matches!(err, SessionError::HeartbeatTimeout), "got {err}");
    agent_task.abort();
}

#[tokio::test]
async fn sha_mismatch_forces_remote_delete() {
    let t = FakeTransport::new("devbox1");
    let (sess, mut agent) = duplex_session(64 * 1024);
    t.push_session(sess);
    let (mut client, _ch, _otx, _ftx) =
        client_for(t.clone(), ClientConfig::default(), Filter::default());

    let agent_task = tokio::spawn(async move {
        let _hello = read_frame(&mut agent.stdin).await.unwrap();
        write_frame(&mut agent.stdout, &Envelope::of_hello_ack(ack("beef")))
            .await
            .unwrap();
    });

    let err = client.run_once().await.unwrap_err();
    assert!(
        matches!(err, SessionError::ShaMismatch { ref agent, .. } if agent == "beef"),
        "got {err}"
    );
    agent_task.await.unwrap();

    let calls = t.exec_calls();
    assert_eq!(calls.len(), 1, "one forced-delete exec");
    assert!(
        calls[0].0[2].contains("rm -f ~/.cache/portal/agent-beef"),
        "delete script: {:?}",
        calls[0].0
    );
}
