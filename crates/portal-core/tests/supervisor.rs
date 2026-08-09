//! Supervisor composition test: a full BoxStack (agent client + reconcile
//! loop + clipsync publisher + notify routing) wired to a FakeTransport
//! playing a scripted portald over real v4 frames, plus a FakeForwarder.
//! This is the "all links joined in one process" test.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use portal_clip::ClipKind;
use portal_clip::watcher::WatchEvent;
use portal_core::bootstrap::EmbeddedAgent;
use portal_core::config::Config;
use portal_core::supervisor::{Deps, NotifyEvent, Supervisor};
use portal_proto::codec::asynchronous::{read_frame, write_frame};
use portal_proto::envelope::Envelope;
use portal_proto::messages::{
    ClipSyncAck, ClipSyncUpdate, HelloAck, Msg, Notify, Port, Snapshot, SubscribeAck,
    marshal_payload,
};
use portal_transport::testing::{FakeForwarder, FakeTransport, duplex_session};
use portal_transport::{ForwardSpec, PortForwarder};
use tokio_util::sync::CancellationToken;

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
            ("clipsync".to_string(), 1),
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

const CONFIG: &str = r#"
[[boxes]]
name = "devbox1"
host = "devbox1"
index = 1
"#;

struct Rig {
    supervisor: Arc<tokio::sync::Mutex<Supervisor>>,
    transport: Arc<FakeTransport>,
    forwarder: Arc<FakeForwarder>,
    notifications: Arc<Mutex<Vec<NotifyEvent>>>,
    urls: Arc<Mutex<Vec<String>>>,
    agent_task: tokio::task::JoinHandle<Vec<Msg>>,
}

/// Compose a supervisor over one box whose "portald" is a scripted task on
/// the far end of duplex pipes. The fake agent:
/// 1. answers the handshake (SHA "cafe", advertising clipsync+notify),
/// 2. sends a Snapshot with ports 8000+3000,
/// 3. sends a Notify frame,
/// 4. acks clipsync updates: first content ack says have_blob=false (forcing
///    the blob-push path), the re-send gets have_blob=true,
/// 5. records every client→agent Msg frame and returns them at Bye.
async fn rig() -> Rig {
    let transport = FakeTransport::new("devbox1");
    let forwarder = Arc::new(FakeForwarder::default());

    // Script the exec calls the stack makes BEFORE the session starts:
    // uname probe, agent probe (hit), shim probe (OK). Later execs (blob put)
    // default to empty success and are asserted via exec_calls().
    let agent_bytes = b"fake-agent";
    let digest = {
        use sha2::{Digest, Sha256};
        hex::encode(Sha256::digest(agent_bytes))
    };
    transport.push_exec_ok("Linux x86_64\n");
    transport.push_exec_ok(&format!("{} {}", agent_bytes.len(), digest));
    transport.push_exec_ok("OK\n");

    let (session, mut agent) = duplex_session(256 * 1024);
    transport.push_session(session);

    let agent_task = tokio::spawn(async move {
        let mut sent_frames = Vec::new();
        // Handshake.
        let hello = read_frame(&mut agent.stdin).await.unwrap().hello.unwrap();
        assert_eq!(hello.box_name.as_deref(), Some("devbox1"));
        assert!(hello.services.as_ref().unwrap().contains_key("clipsync"));
        write_frame(&mut agent.stdout, &Envelope::of_hello_ack(ack("cafe")))
            .await
            .unwrap();
        let sub = read_frame(&mut agent.stdin)
            .await
            .unwrap()
            .subscribe
            .unwrap();
        assert!(sub.deny.contains(&22), "default denies applied");
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
                ports: vec![port(8000), port(3000)],
            }),
        )
        .await
        .unwrap();
        // A notification from the box.
        write_frame(
            &mut agent.stdout,
            &Envelope::of_msg(Msg {
                service: "notify".into(),
                kind: "event".into(),
                seq: Some(1),
                payload: Some(
                    marshal_payload(&Notify {
                        title: "build done".into(),
                        body: Some("42 tests".into()),
                        subtitle: None,
                        urgency: Some(1),
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

        // clipsync: ack update #1 with have_blob=false, its re-send with true.
        let mut content_acks = 0u32;
        loop {
            let env =
                match tokio::time::timeout(Duration::from_secs(10), read_frame(&mut agent.stdin))
                    .await
                {
                    Ok(Ok(env)) => env,
                    _ => break,
                };
            let Some(msg) = env.msg else { continue };
            if msg.service == "clipsync" && msg.kind == "update" {
                let u: ClipSyncUpdate =
                    portal_proto::messages::unmarshal_payload(msg.payload.as_ref().unwrap())
                        .unwrap();
                content_acks += 1;
                let have_blob = content_acks > 1 || u.inline.is_some();
                write_frame(
                    &mut agent.stdout,
                    &Envelope::of_msg(Msg {
                        service: "clipsync".into(),
                        kind: "ack".into(),
                        seq: Some(u64::from(content_acks)),
                        payload: Some(
                            marshal_payload(&ClipSyncAck {
                                change_id: u.change_id,
                                have_blob,
                            })
                            .unwrap(),
                        ),
                    }),
                )
                .await
                .unwrap();
                sent_frames.push(msg);
                if content_acks >= 2 {
                    break;
                }
            } else {
                sent_frames.push(msg);
            }
        }
        sent_frames
    });

    let notifications: Arc<Mutex<Vec<NotifyEvent>>> = Arc::default();
    let urls: Arc<Mutex<Vec<String>>> = Arc::default();
    let deps = Deps {
        agent: EmbeddedAgent {
            git_sha: "cafe".into(),
            linux_amd64: Some(Arc::from(&agent_bytes[..])),
            linux_arm64: None,
        },
        gates: Arc::new(|_| true),
        notify: {
            let sink = notifications.clone();
            Arc::new(move |ev| sink.lock().unwrap().push(ev))
        },
        open_url: {
            let sink = urls.clone();
            Arc::new(move |u| sink.lock().unwrap().push(u))
        },
        transport: {
            let transport = transport.clone();
            let forwarder = forwarder.clone();
            Arc::new(move |_cfg| {
                (
                    transport.clone() as Arc<dyn portal_transport::Transport>,
                    forwarder.clone() as Arc<dyn PortForwarder>,
                )
            })
        },
        cred: None,
        clipboard_writer: None,
    };

    let config = Config::parse(CONFIG).unwrap();
    let supervisor = Supervisor::start::<NoSource, NoGates>(
        &config,
        &deps,
        None, // no platform watcher; events injected via clip_sender()
        CancellationToken::new(),
    );
    Rig {
        supervisor: Arc::new(tokio::sync::Mutex::new(supervisor)),
        transport,
        forwarder,
        notifications,
        urls,
        agent_task,
    }
}

// Unused watcher type params for the None case.
struct NoSource;
impl portal_clip::watcher::SnapshotSource for NoSource {
    fn change_count(&self) -> i64 {
        0
    }
    fn observe(&self) -> Result<portal_clip::watcher::Observation, portal_clip::ClipError> {
        Ok(portal_clip::watcher::Observation::Empty)
    }
}
struct NoGates;
impl portal_clip::watcher::Gates for NoGates {
    fn text_enabled(&self) -> bool {
        true
    }
    fn image_enabled(&self) -> bool {
        true
    }
}

async fn wait_until(deadline: Duration, mut pred: impl AsyncFnMut() -> bool) -> bool {
    let start = tokio::time::Instant::now();
    while start.elapsed() < deadline {
        if pred().await {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    false
}

/// Status snapshot through the lock (async — never blocking_lock in a runtime).
async fn sup_status(rig: &Rig) -> Vec<portal_core::supervisor::BoxStatus> {
    rig.supervisor.lock().await.status()
}
async fn sup_stacks_len(rig: &Rig) -> usize {
    rig.supervisor.lock().await.stacks().len()
}
async fn sup_stack_names(rig: &Rig) -> Vec<String> {
    rig.supervisor
        .lock()
        .await
        .stacks()
        .iter()
        .map(|s| s.cfg.name.clone())
        .collect()
}
async fn sup_stack_hosts(rig: &Rig) -> Vec<String> {
    rig.supervisor
        .lock()
        .await
        .stacks()
        .iter()
        .map(|s| s.cfg.host.clone())
        .collect()
}
async fn sup_first_cfg(rig: &Rig) -> portal_core::config::BoxConfig {
    rig.supervisor.lock().await.stacks()[0].cfg.clone()
}

#[tokio::test]
async fn full_stack_forwards_notifies_and_syncs_clipboard() {
    let rig = rig().await;

    // 1. Forwards converge from the snapshot SAME-PORT (8000→8000, 3000→3000)
    //    so forwarded services see a truthful Host/Origin.
    let converged = wait_until(Duration::from_secs(5), async || {
        let f = rig.forwarder.forwards.lock().unwrap();
        f.contains(&ForwardSpec {
            local: 8000,
            remote: 8000,
        }) && f.contains(&ForwardSpec {
            local: 3000,
            remote: 3000,
        })
    })
    .await;
    assert!(converged, "forwards did not converge from the snapshot");

    // Status reflects them.
    let ok = wait_until(Duration::from_secs(2), async || {
        let st = sup_status(&rig).await;
        !st.is_empty()
            && st[0].connected
            && st[0].forwards.len() == 2
            && st[0].agent_sha.as_deref() == Some("cafe")
    })
    .await;
    assert!(ok, "status: {:?}", sup_status(&rig).await);

    // 2. Notification routed with box attribution.
    let ok = wait_until(Duration::from_secs(2), async || {
        !rig.notifications.lock().unwrap().is_empty()
    })
    .await;
    assert!(ok, "notification never arrived");
    {
        let n = &rig.notifications.lock().unwrap()[0];
        assert_eq!(n.box_name, "devbox1");
        assert_eq!(n.title, "build done");
        assert!(n.verified);
    }
    assert!(rig.urls.lock().unwrap().is_empty());

    // 3. clipsync: an IMAGE copy (blob path) → update → ack(no blob) →
    //    blob put exec → re-send → ack(have) → synced.
    let img = vec![0x89u8; 4096];
    rig.supervisor
        .lock()
        .await
        .clip_sender()
        .send(WatchEvent::Changed {
            change_id: 1,
            kind: ClipKind::Image,
            data: img.clone(),
        })
        .unwrap();

    let synced = wait_until(Duration::from_secs(5), async || {
        let st = sup_status(&rig).await;
        !st.is_empty() && st[0].clipsync_synced && st[0].clipsync_change_id == 1
    })
    .await;
    assert!(
        synced,
        "clipsync never converged: {:?}",
        sup_status(&rig).await
    );

    // The agent saw both updates; the daemon exec'd `portald blob put`.
    let frames = rig.agent_task.await.unwrap();
    let updates: Vec<_> = frames
        .iter()
        .filter(|m| m.service == "clipsync" && m.kind == "update")
        .collect();
    assert_eq!(updates.len(), 2, "update + post-push re-send");

    let blob_put_call = rig
        .transport
        .exec_calls()
        .into_iter()
        .find(|(argv, _)| argv.iter().any(|a| a == "blob"))
        .expect("blob put exec happened");
    let (argv, stdin) = blob_put_call;
    assert!(argv.iter().any(|a| a == "put"));
    assert!(argv.contains(&img.len().to_string()));
    assert_eq!(stdin, img, "blob bytes ride stdin");

    {
        rig.supervisor.lock().await.cancel_all();
    }
}

/// The callback-URL flow end-to-end through the real stack: scripted agent
/// relays an OpenUrl for an EPHEMERAL box port (never in any snapshot — the
/// exact shape that broke in the field), and the daemon must (1) establish an
/// on-demand forward, (2) open the REWRITTEN local URL, (3) re-Subscribe with
/// the pinned port allowlisted so the agent starts reporting the listener.
#[tokio::test]
async fn callback_url_is_forwarded_rewritten_and_allowlisted() {
    let transport = FakeTransport::new("devbox1");
    let forwarder = Arc::new(FakeForwarder::default());
    let agent_bytes = b"fake-agent";
    let digest = {
        use sha2::{Digest, Sha256};
        hex::encode(Sha256::digest(agent_bytes))
    };
    transport.push_exec_ok("Linux x86_64\n");
    transport.push_exec_ok(&format!("{} {}", agent_bytes.len(), digest));
    transport.push_exec_ok("OK\n");

    let (session, mut agent) = duplex_session(256 * 1024);
    transport.push_session(session);

    let agent_task = tokio::spawn(async move {
        // Handshake + initial Subscribe + empty snapshot (the callback
        // listener is ephemeral: the agent does NOT report it).
        let _hello = read_frame(&mut agent.stdin).await.unwrap().hello.unwrap();
        write_frame(&mut agent.stdout, &Envelope::of_hello_ack(ack("cafe")))
            .await
            .unwrap();
        let sub = read_frame(&mut agent.stdin)
            .await
            .unwrap()
            .subscribe
            .unwrap();
        assert!(
            !sub.allow.contains(&53219),
            "pin must not be allowlisted before the callback"
        );
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
                ports: vec![],
            }),
        )
        .await
        .unwrap();

        // The box relays a callback URL (as `portald open` would).
        write_frame(
            &mut agent.stdout,
            &Envelope::of_msg(Msg {
                service: "openurl".into(),
                kind: "event".into(),
                seq: Some(1),
                payload: Some(
                    marshal_payload(&portal_proto::messages::OpenUrl {
                        url: "http://localhost:53219/callback?code=abc&state=xyz".into(),
                        seq: 1,
                    })
                    .unwrap(),
                ),
            }),
        )
        .await
        .unwrap();

        // The pin must trigger a re-Subscribe whose allowlist carries 53219
        // (allow wins over the agent's ephemeral cut — this is what makes
        // the listener observable and lets the pin retire on its death).
        loop {
            let env =
                match tokio::time::timeout(Duration::from_secs(5), read_frame(&mut agent.stdin))
                    .await
                {
                    Ok(Ok(env)) => env,
                    _ => panic!("no re-Subscribe with the pinned port arrived"),
                };
            if let Some(sub) = env.subscribe
                && sub.allow.contains(&53219)
            {
                return Vec::<Msg>::new();
            }
        }
    });

    let notifications: Arc<Mutex<Vec<NotifyEvent>>> = Arc::default();
    let urls: Arc<Mutex<Vec<String>>> = Arc::default();
    let deps = Deps {
        agent: EmbeddedAgent {
            git_sha: "cafe".into(),
            linux_amd64: Some(Arc::from(&agent_bytes[..])),
            linux_arm64: None,
        },
        gates: Arc::new(|_| true),
        notify: {
            let sink = notifications.clone();
            Arc::new(move |ev| sink.lock().unwrap().push(ev))
        },
        open_url: {
            let sink = urls.clone();
            Arc::new(move |u| sink.lock().unwrap().push(u))
        },
        transport: {
            let transport = transport.clone();
            let forwarder = forwarder.clone();
            Arc::new(move |_cfg| {
                (
                    transport.clone() as Arc<dyn portal_transport::Transport>,
                    forwarder.clone() as Arc<dyn PortForwarder>,
                )
            })
        },
        cred: None,
        clipboard_writer: None,
    };

    let config = Config::parse(CONFIG).unwrap();
    let supervisor =
        Supervisor::start::<NoSource, NoGates>(&config, &deps, None, CancellationToken::new());

    // The browser must be pointed at the LOCAL end — with the identity-first
    // policy that is the SAME port number the box listener uses.
    let opened = wait_until(Duration::from_secs(5), async || {
        !urls.lock().unwrap().is_empty()
    })
    .await;
    assert!(opened, "callback URL was never opened");

    let spec = {
        let forwards = forwarder.forwards.lock().unwrap();
        forwards
            .iter()
            .find(|f| f.remote == 53219)
            .copied()
            .expect("on-demand forward for the callback port")
    };
    assert_eq!(
        spec.local, 53219,
        "callback pins take the IDENTITY mapping (v1 parity) so absolute \
         redirects to the original port keep working"
    );
    assert_eq!(
        urls.lock().unwrap()[0],
        format!(
            "http://127.0.0.1:{}/callback?code=abc&state=xyz",
            spec.local
        ),
        "must open the local end of the forward, query preserved"
    );

    // And the agent saw the allowlisted re-Subscribe (the task asserts it).
    agent_task.await.unwrap();
    supervisor.cancel_all();
}

/// The aws-sso shape through the real stack: a PUBLIC authorize URL whose
/// redirect_uri names a loopback port. The URL must open byte-identical, the
/// redirect port must get a SAME-PORT forward (the provider redirects to the
/// literal port — rewriting cannot help), and the pin must be allowlisted.
#[tokio::test]
async fn oauth_authorize_url_gets_same_port_forward_for_redirect_target() {
    let transport = FakeTransport::new("devbox1");
    let forwarder = Arc::new(FakeForwarder::default());
    let agent_bytes = b"fake-agent";
    let digest = {
        use sha2::{Digest, Sha256};
        hex::encode(Sha256::digest(agent_bytes))
    };
    transport.push_exec_ok("Linux x86_64\n");
    transport.push_exec_ok(&format!("{} {}", agent_bytes.len(), digest));
    transport.push_exec_ok("OK\n");

    let (session, mut agent) = duplex_session(256 * 1024);
    transport.push_session(session);

    const AUTHORIZE: &str = "https://oidc.us-east-1.amazonaws.com/authorize?\
        response_type=code&client_id=abc&\
        redirect_uri=http%3A%2F%2F127.0.0.1%3A53777%2Foauth%2Fcallback&\
        state=xyz&code_challenge=cc&code_challenge_method=S256";

    let agent_task = tokio::spawn(async move {
        let _hello = read_frame(&mut agent.stdin).await.unwrap().hello.unwrap();
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
                ports: vec![],
            }),
        )
        .await
        .unwrap();

        // aws sso login → xdg-open → portald open → this frame.
        write_frame(
            &mut agent.stdout,
            &Envelope::of_msg(Msg {
                service: "openurl".into(),
                kind: "event".into(),
                seq: Some(1),
                payload: Some(
                    marshal_payload(&portal_proto::messages::OpenUrl {
                        url: AUTHORIZE.into(),
                        seq: 1,
                    })
                    .unwrap(),
                ),
            }),
        )
        .await
        .unwrap();

        // The redirect target must be allowlisted like any pin.
        loop {
            let env =
                match tokio::time::timeout(Duration::from_secs(5), read_frame(&mut agent.stdin))
                    .await
                {
                    Ok(Ok(env)) => env,
                    _ => panic!("no re-Subscribe with the redirect port arrived"),
                };
            if let Some(sub) = env.subscribe
                && sub.allow.contains(&53777)
            {
                return;
            }
        }
    });

    let urls: Arc<Mutex<Vec<String>>> = Arc::default();
    let deps = Deps {
        agent: EmbeddedAgent {
            git_sha: "cafe".into(),
            linux_amd64: Some(Arc::from(&agent_bytes[..])),
            linux_arm64: None,
        },
        gates: Arc::new(|_| true),
        notify: Arc::new(|_| {}),
        open_url: {
            let sink = urls.clone();
            Arc::new(move |u| sink.lock().unwrap().push(u))
        },
        transport: {
            let transport = transport.clone();
            let forwarder = forwarder.clone();
            Arc::new(move |_cfg| {
                (
                    transport.clone() as Arc<dyn portal_transport::Transport>,
                    forwarder.clone() as Arc<dyn PortForwarder>,
                )
            })
        },
        cred: None,
        clipboard_writer: None,
    };

    let config = Config::parse(CONFIG).unwrap();
    let supervisor =
        Supervisor::start::<NoSource, NoGates>(&config, &deps, None, CancellationToken::new());

    let opened = wait_until(Duration::from_secs(5), async || {
        !urls.lock().unwrap().is_empty()
    })
    .await;
    assert!(opened, "authorize URL was never opened");
    assert_eq!(
        urls.lock().unwrap()[0],
        AUTHORIZE,
        "public authorize URL must open byte-identical — the provider page is \
         already correct and its query carries single-use OAuth state"
    );
    assert!(
        forwarder.forwards.lock().unwrap().contains(&ForwardSpec {
            local: 53777,
            remote: 53777
        }),
        "redirect target must be forwarded SAME-PORT: the provider redirects \
         to the literal redirect_uri port after login"
    );

    agent_task.await.unwrap();
    supervisor.cancel_all();
}

#[tokio::test]
async fn config_hot_reload_adds_updates_and_removes_stacks() {
    use portal_core::agentclient::session::Filter;
    use portal_core::config::BoxConfig;
    use portal_core::supervisor::filter_for;

    let rig = rig().await; // one box "devbox1" @ "devbox1"
    assert_eq!(sup_stacks_len(&rig).await, 1);
    let claimed = wait_until(Duration::from_secs(5), async || {
        let ports = rig.supervisor.lock().await.taken_ports();
        ports.contains(&3000) && ports.contains(&8000)
    })
    .await;
    assert!(claimed, "initial stack never claimed its identity ports");

    // 1. In-place update: allowlist change applies WITHOUT respawning and
    // publishes one event-driven local API invalidation (no status poller).
    let mut state_changes = rig.supervisor.lock().await.subscribe_state_changes();
    let mut cfg2 = Config::parse(
        r#"
[[boxes]]
name = "devbox1"
host = "devbox1"
index = 1
allow = [9000]
"#,
    )
    .unwrap();
    rig.supervisor_apply(&cfg2).await;
    tokio::time::timeout(Duration::from_secs(1), state_changes.recv())
        .await
        .expect("config reconcile did not publish state invalidation")
        .expect("state invalidation channel closed");
    assert_eq!(sup_stacks_len(&rig).await, 1);
    assert_eq!(filter_for(&sup_first_cfg(&rig).await).allow, vec![9000]);
    // 2. Add a box → new stack spawns.
    cfg2.boxes.push(BoxConfig {
        name: "gpu-box".into(),
        host: "gpu.internal".into(),
        index: 2,
        allow: vec![],
        deny: vec![],
        enabled: true,
    });
    rig.supervisor_apply(&cfg2).await;
    assert_eq!(sup_stacks_len(&rig).await, 2);
    let names = sup_stack_names(&rig).await;
    assert!(names.iter().any(|n| n == "devbox1") && names.iter().any(|n| n == "gpu-box"));

    // 3. Host change = replacement, not in-place (fresh connection).
    let cfg3 = Config::parse(
        r#"
[[boxes]]
name = "devbox1"
host = "devbox1-NEW"
index = 1
[[boxes]]
name = "gpu-box"
host = "gpu.internal"
index = 2
"#,
    )
    .unwrap();
    rig.supervisor_apply(&cfg3).await;
    assert_eq!(sup_stacks_len(&rig).await, 2);
    assert!(
        rig.supervisor.lock().await.taken_ports().is_empty(),
        "replacing a stack releases its local-port claims so re-enable can reuse identity ports"
    );
    let hosts = sup_stack_hosts(&rig).await;
    assert!(hosts.iter().any(|h| h == "devbox1-NEW"));

    // 4. Remove a box → stack tears down.
    let cfg4 = Config::parse(
        r#"
[[boxes]]
name = "devbox1"
host = "devbox1-NEW"
index = 1
"#,
    )
    .unwrap();
    rig.supervisor_apply(&cfg4).await;
    assert_eq!(sup_stacks_len(&rig).await, 1);
    assert_eq!(sup_first_cfg(&rig).await.name, "devbox1");

    // Filter derivation sanity (defaults always present).
    let f: Filter = filter_for(&rig.supervisor.lock().await.stacks()[0].cfg);
    assert!(f.deny.contains(&22));

    {
        rig.supervisor.lock().await.cancel_all();
    }
}

impl Rig {
    async fn supervisor_apply(&self, cfg: &Config) {
        self.supervisor.lock().await.reconcile(cfg).await;
    }
}

/// reconcile's return contract: it reports (and broadcasts) exactly when a
/// running stack changed. Disabled-only configs — added, edited, or removed
/// — change the rendered state without touching a stack, so reconcile has
/// nothing to say; the daemon's mutation and hot-reload paths rely on that
/// `false` to emit the one invalidation themselves.
#[tokio::test]
async fn reconcile_reports_invalidation_only_when_stacks_change() {
    // No boxes at all: the transport factory must never fire.
    let deps = Deps {
        agent: EmbeddedAgent {
            git_sha: "quiet".into(),
            linux_amd64: None,
            linux_arm64: None,
        },
        gates: Arc::new(|_| true),
        notify: Arc::new(|_| {}),
        open_url: Arc::new(|_| {}),
        transport: Arc::new(|_| panic!("no enabled boxes: no stack may spawn")),
        cred: None,
        clipboard_writer: None,
    };
    let mut supervisor = Supervisor::start::<NoSource, NoGates>(
        &Config::default(),
        &deps,
        None,
        CancellationToken::new(),
    );
    let mut changes = supervisor.subscribe_state_changes();

    let paused = Config::parse(
        r#"
[[boxes]]
name = "paused"
host = "paused.internal"
index = 1
enabled = false
allow = [3000]
"#,
    )
    .unwrap();
    assert!(
        !supervisor.reconcile(&paused).await,
        "adding a disabled box touches no stack"
    );

    let paused_edited = Config::parse(
        r#"
[[boxes]]
name = "paused"
host = "paused.internal"
index = 1
enabled = false
allow = [3000, 8080]
"#,
    )
    .unwrap();
    assert!(
        !supervisor.reconcile(&paused_edited).await,
        "editing a disabled box's allowlist touches no stack"
    );

    assert!(
        !supervisor.reconcile(&Config::default()).await,
        "removing a disabled box touches no stack"
    );
    assert!(
        changes.try_recv().is_err(),
        "disabled-only changes broadcast nothing from reconcile"
    );
    supervisor.cancel_all();
}
