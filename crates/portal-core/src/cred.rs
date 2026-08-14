//! The Mac-side credential handler task: drains the dedicated cred channel,
//! runs portal-cred's (blocking, tested) policy core on a blocking task, and
//! sends the CredResponse up the pipe. All boxes share one fair prompt mutex:
//! credential requests wait FIFO instead of racing modal UI or being denied
//! merely because another request is in progress.

use std::sync::Arc;
use std::time::Instant;

use portal_cred::cooldown::Cooldown;
use portal_cred::keychain::Keychain;
use portal_cred::prompt::{Biometry, Prompter};
use portal_cred::serve::{ServeDeps, serve_cred_request};
use portal_proto::messages::{CredRequest, CredResponse};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::agentclient::ServiceRequest;
use crate::agentclient::session::Outbound;

/// The production dependencies for serving credential requests. Built once by
/// the daemon; shared by every box's handler (ONE keychain, ONE cooldown map —
/// a denial on box A cools the label down for box B too, matching the intent
/// that cooldown fights prompt fatigue at the human, not per connection).
pub struct CredDeps {
    pub prompter: Box<dyn Prompter>,
    pub biometry: Option<Box<dyn Biometry>>,
    pub keychain: Box<dyn Keychain>,
    pub cooldown: Cooldown,
    /// Tokio's mutex grants locks in call order, making this the global FIFO
    /// admission gate for modal credential UI across every configured box.
    pub prompt_queue: Arc<tokio::sync::Mutex<()>>,
}

pub struct CredHandler {
    pub deps: Option<Arc<CredDeps>>,
    pub gates: Arc<dyn Fn(&str) -> bool + Send + Sync>,
    pub box_name: String,
    pub host: String,
    pub outbound: mpsc::Sender<Outbound>,
}

impl CredHandler {
    pub async fn run(
        &self,
        cred_rx: &mut mpsc::Receiver<ServiceRequest>,
        cancel: CancellationToken,
    ) {
        loop {
            let req = tokio::select! {
                _ = cancel.cancelled() => return,
                r = cred_rx.recv() => match r { Some(r) => r, None => return },
            };
            let ServiceRequest::Cred(req) = req else {
                continue;
            };
            let resp = self.serve_one(req).await;
            if let Ok(out) = Outbound::cred_response(&resp) {
                let _ = self.outbound.send(out).await;
            }
        }
    }

    /// One request end-to-end. The policy core (dialog + Touch ID) blocks for
    /// up to ~2 minutes, so it runs on a blocking task. The shared fair mutex
    /// queues requests across boxes and guarantees only one modal prompt.
    async fn serve_one(&self, req: CredRequest) -> CredResponse {
        let (nonce, epoch) = (req.nonce, req.epoch);
        let deny = |reason: &str| CredResponse {
            nonce,
            epoch,
            ok: false,
            secret: None,
            err: Some(reason.to_string()),
        };
        let Some(deps) = self.deps.clone() else {
            return deny("gui-unavailable");
        };
        let _turn = deps.prompt_queue.clone().lock_owned().await;
        let gates = self.gates.clone();
        let host = self.host.clone();
        let box_name = self.box_name.clone();
        let result = tokio::task::spawn_blocking(move || {
            let features = |name: &str| gates(name);
            let now = Instant::now;
            // Requester shown in the dialog carries the box attribution.
            let mut req = req;
            let requester = req.requester.take().unwrap_or_default();
            req.requester = Some(if requester.is_empty() {
                box_name.clone()
            } else {
                format!("{requester} on {box_name}")
            });
            let serve_deps = ServeDeps {
                prompter: &*deps.prompter,
                biometry: deps.biometry.as_deref(),
                keychain: Some(&*deps.keychain),
                features: &features,
                cooldown: &deps.cooldown,
                host: &host,
                now: &now,
            };
            serve_cred_request(&serve_deps, &req)
        })
        .await;
        match result {
            Ok(resp) => resp,
            Err(err) => {
                tracing::error!(target: "portal::cred", %err, "cred serve task panicked");
                deny("gui-unavailable")
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_cred::keychain::MemoryKeychain;
    use portal_cred::prompt::{Decision, Outcome, Request};

    struct AllowOnce;
    impl Prompter for AllowOnce {
        fn prompt(&self, req: &Request) -> Decision {
            // Box attribution must reach the dialog.
            assert!(req.requester.contains("on devbox1"), "{:?}", req.requester);
            Decision {
                outcome: Outcome::AllowOnce,
                secret: b"pw".to_vec(),
            }
        }
    }

    fn req() -> CredRequest {
        CredRequest {
            nonce: 9,
            epoch: 2,
            label: "sudo".into(),
            requester: Some("pid 42: sudo".into()),
            mode: "askpass".into(),
            target: None,
        }
    }

    fn handler(deps: Option<Arc<CredDeps>>) -> (CredHandler, mpsc::Receiver<Outbound>) {
        let (tx, rx) = mpsc::channel(4);
        (
            CredHandler {
                deps,
                gates: Arc::new(|_| true),
                box_name: "devbox1".into(),
                host: "devbox1.internal".into(),
                outbound: tx,
            },
            rx,
        )
    }

    #[tokio::test]
    async fn serves_through_the_policy_core_with_box_attribution() {
        let deps = Arc::new(CredDeps {
            prompter: Box::new(AllowOnce),
            biometry: None,
            keychain: Box::new(MemoryKeychain::default()),
            cooldown: Cooldown::default(),
            prompt_queue: Arc::new(tokio::sync::Mutex::new(())),
        });
        let (h, _rx) = handler(Some(deps));
        let resp = h.serve_one(req()).await;
        assert!(resp.ok);
        assert_eq!(resp.secret.as_ref().unwrap().as_slice(), b"pw");
        assert_eq!((resp.nonce, resp.epoch), (9, 2));
    }

    #[tokio::test]
    async fn no_deps_denies_gui_unavailable() {
        let (h, _rx) = handler(None);
        let resp = h.serve_one(req()).await;
        assert!(!resp.ok);
        assert_eq!(resp.err.as_deref(), Some("gui-unavailable"));
    }

    struct QueueProbe {
        entered: std::sync::mpsc::Sender<String>,
        release: std::sync::Mutex<std::sync::mpsc::Receiver<()>>,
    }

    impl Prompter for QueueProbe {
        fn prompt(&self, req: &Request) -> Decision {
            self.entered.send(req.requester.clone()).unwrap();
            self.release.lock().unwrap().recv().unwrap();
            Decision {
                outcome: Outcome::AllowOnce,
                secret: b"pw".to_vec(),
            }
        }
    }

    async fn next_prompt(rx: &std::sync::mpsc::Receiver<String>) -> String {
        tokio::time::timeout(std::time::Duration::from_secs(2), async {
            loop {
                match rx.try_recv() {
                    Ok(value) => return value,
                    Err(std::sync::mpsc::TryRecvError::Empty) => tokio::task::yield_now().await,
                    Err(std::sync::mpsc::TryRecvError::Disconnected) => {
                        panic!("prompt probe disconnected")
                    }
                }
            }
        })
        .await
        .expect("prompt did not start")
    }

    #[tokio::test]
    async fn shared_prompt_gate_serializes_requests_across_boxes() {
        let (entered_tx, entered_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let deps = Arc::new(CredDeps {
            prompter: Box::new(QueueProbe {
                entered: entered_tx,
                release: std::sync::Mutex::new(release_rx),
            }),
            biometry: None,
            keychain: Box::new(MemoryKeychain::default()),
            cooldown: Cooldown::default(),
            prompt_queue: Arc::new(tokio::sync::Mutex::new(())),
        });
        let (first_handler, _first_outbound) = handler(Some(deps.clone()));
        let (mut second_handler, _second_outbound) = handler(Some(deps));
        second_handler.box_name = "devbox2".into();
        second_handler.host = "devbox2.internal".into();

        let mut first_req = req();
        first_req.nonce = 1;
        let first = tokio::spawn(async move { first_handler.serve_one(first_req).await });
        assert!(next_prompt(&entered_rx).await.contains("on devbox1"));

        let mut second_req = req();
        second_req.nonce = 2;
        let second = tokio::spawn(async move { second_handler.serve_one(second_req).await });
        tokio::time::sleep(std::time::Duration::from_millis(75)).await;
        let early_second = entered_rx.try_recv();

        // Always release both blocking probes before asserting so a failure
        // cannot strand a spawn_blocking thread during runtime shutdown.
        release_tx.send(()).unwrap();
        release_tx.send(()).unwrap();
        let second_seen = match &early_second {
            Ok(value) => value.clone(),
            Err(std::sync::mpsc::TryRecvError::Empty) => next_prompt(&entered_rx).await,
            Err(std::sync::mpsc::TryRecvError::Disconnected) => {
                panic!("prompt probe disconnected")
            }
        };

        assert!(first.await.unwrap().ok);
        assert!(second.await.unwrap().ok);
        assert!(second_seen.contains("on devbox2"));
        assert!(
            matches!(early_second, Err(std::sync::mpsc::TryRecvError::Empty)),
            "a second box must wait instead of presenting concurrent modal UI"
        );
    }
}
