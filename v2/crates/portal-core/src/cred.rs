//! The Mac-side credential handler task: drains the dedicated cred channel,
//! runs portal-cred's (blocking, tested) policy core on a blocking task, and
//! sends the CredResponse up the pipe. Serialization: ONE prompt at a time
//! per box (the policy core is modal by nature); requests arriving while one
//! is pending are denied "busy" (v1 semantics).

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
    /// up to ~2 minutes, so it runs on a blocking task; the channel (cap 2)
    /// plus this sequential loop bound concurrent prompts at one-per-box.
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
}
