//! The Mac-side credential policy core.
//!
//! Decision order:
//! 1. sanitize label; `cred` gate off → `disabled`;
//! 2. invalid label → `label-invalid`; active cooldown → `cooldown`;
//! 3. remembered? (keychain probe) + Touch ID availability
//!    (remembered OR askpass, gated by `cred-touchid`);
//! 4. remembered + Touch ID: system sheet — Approved serves from Keychain,
//!    Canceled records cooldown and denies, Timeout denies `timeout`, an
//!    evaluation ERROR falls back to the dialog;
//! 5. dialog: fresh labels resolve AllowOnce / AllowRemember(+store) / Deny
//!    (+cooldown) / Timeout / Unavailable; remembered labels additionally
//!    support Forget (delete + re-prompt as fresh);
//! 6. every served secret is capped at [`SECRET_MAX_BYTES`] — an oversized
//!    secret fails CLOSED under the generic `denied` token (no new wire
//!    reason, C1).
//!
//! It NEVER returns an inconsistent response: nonce/epoch are echoed on every
//! path, and `ok=false` always carries a wire-vocabulary reason. Secrets are
//! never logged (only fixed context crosses tracing).

use std::time::{Duration, Instant};

use portal_proto::messages::{CredRequest, CredResponse};
use serde_bytes::ByteBuf;

use crate::cooldown::Cooldown;
use crate::keychain::Keychain;
use crate::prompt::{Biometry, BiometryOutcome, Decision, Outcome, Prompter, Request};
use crate::{
    CONTEXT_MAX_BYTES, DIALOG_BUDGET, DIALOG_MAX_SECS, DIALOG_MIN_SECS, DenyReason, FEATURE_CRED,
    FEATURE_CRED_TOUCHID, LABEL_MAX_BYTES, Mode, SECRET_MAX_BYTES, strip_controls, truncate_text,
    valid_label,
};

/// Injectable boundary for one credential request. `features` is consulted
/// LIVE per request (v1 re-reads the gate files each time). `now` is the
/// clock seam.
pub struct ServeDeps<'a> {
    pub prompter: &'a dyn Prompter,
    pub biometry: Option<&'a dyn Biometry>,
    pub keychain: Option<&'a dyn Keychain>,
    pub features: &'a dyn Fn(&str) -> bool,
    pub cooldown: &'a Cooldown,
    pub host: &'a str,
    pub now: &'a dyn Fn() -> Instant,
}

/// Serve one CredRequest. Blocking (dialog + biometry are modal); the daemon
/// runs it on a blocking task while holding its global FIFO prompt gate.
pub fn serve_cred_request(deps: &ServeDeps<'_>, req: &CredRequest) -> CredResponse {
    let resp = CredResponse {
        nonce: req.nonce,
        epoch: req.epoch,
        ok: false,
        secret: None,
        err: None,
    };

    let label = strip_controls(&req.label);
    let mode = req.mode.parse::<Mode>().unwrap_or(Mode::Env);

    if !(deps.features)(FEATURE_CRED) {
        return deny(resp, DenyReason::Disabled);
    }
    if !valid_label(&label) {
        return deny(resp, DenyReason::LabelInvalid);
    }
    let started = (deps.now)();
    if deps.cooldown.active(&label, started) {
        return deny(resp, DenyReason::Cooldown);
    }

    let requester = truncate_text(
        &strip_controls(req.requester.as_deref().unwrap_or("")),
        CONTEXT_MAX_BYTES,
    )
    .to_string();
    let target = truncate_text(
        &strip_controls(req.target.as_deref().unwrap_or("")),
        CONTEXT_MAX_BYTES,
    )
    .to_string();

    let deadline = started + DIALOG_BUDGET;
    let remembered = deps
        .keychain
        .and_then(|kc| kc.get(&label).ok().flatten())
        .is_some();
    let touch_id_available = (remembered || mode == Mode::Askpass)
        && (deps.features)(FEATURE_CRED_TOUCHID)
        && deps.biometry.is_some_and(|b| b.available());

    // Remembered + biometrics: the Touch ID sheet IS the consent gesture.
    if remembered && touch_id_available {
        let reason = format!(
            "portal: approve credential \"{label}\" for {}",
            truncate_text(&strip_controls(deps.host), CONTEXT_MAX_BYTES)
        );
        let budget = deadline.saturating_duration_since((deps.now)());
        match deps.biometry.unwrap().approve(&reason, budget) {
            Ok(BiometryOutcome::Approved) => {
                return match keychain_secret(deps, &label) {
                    Some(secret) => serve(resp, secret),
                    // Missing/oversized after approval: fail closed, generic token.
                    None => deny(resp, DenyReason::Denied),
                };
            }
            Ok(BiometryOutcome::Canceled) => {
                deps.cooldown.record(&label, (deps.now)());
                return deny(resp, DenyReason::Denied);
            }
            Ok(BiometryOutcome::Timeout) => return deny(resp, DenyReason::Timeout),
            Err(err) => {
                // Evaluation failure (lockout, sheet error) → dialog fallback.
                tracing::warn!(target: "portal::cred", "biometry evaluation failed; falling back to dialog: {err}");
            }
        }
    }

    let Some(timeout) = prompt_timeout(deps, deadline) else {
        return deny(resp, DenyReason::Timeout);
    };
    let prompt_req = Request {
        label: label.clone(),
        requester,
        host: deps.host.to_string(),
        mode,
        target,
        remembered,
        touch_id_enroll: !remembered && mode == Mode::Askpass && touch_id_available,
        timeout,
    };
    let decision = deps.prompter.prompt(&prompt_req);

    if !remembered {
        return resolve_fresh(deps, resp, &label, decision);
    }

    match decision.outcome {
        Outcome::AllowRemember => match keychain_secret(deps, &label) {
            Some(secret) => serve(resp, secret),
            None => deny(resp, DenyReason::Denied),
        },
        Outcome::Forget => {
            if let Some(kc) = deps.keychain {
                let _ = kc.delete(&label);
            }
            let Some(timeout) = prompt_timeout(deps, deadline) else {
                return deny(resp, DenyReason::Timeout);
            };
            let mut again = prompt_req.clone();
            again.remembered = false;
            again.touch_id_enroll = prompt_req.mode == Mode::Askpass && touch_id_available;
            again.timeout = timeout;
            let decision = deps.prompter.prompt(&again);
            resolve_fresh(deps, resp, &label, decision)
        }
        Outcome::Deny => {
            deps.cooldown.record(&label, (deps.now)());
            deny(resp, DenyReason::Denied)
        }
        Outcome::Timeout => deny(resp, DenyReason::Timeout),
        Outcome::AllowOnce | Outcome::Unavailable => deny(resp, DenyReason::GuiUnavailable),
    }
}

/// Port of resolveFreshCredDecision: the user typed (or declined to type) a
/// NEW secret into the hidden-answer dialog.
fn resolve_fresh(
    deps: &ServeDeps<'_>,
    resp: CredResponse,
    label: &str,
    mut decision: Decision,
) -> CredResponse {
    let secret = std::mem::take(&mut decision.secret); // Drop zeroizes the (now empty) rest
    match decision.outcome {
        Outcome::AllowOnce => {
            if secret.len() > SECRET_MAX_BYTES {
                return deny(resp, DenyReason::Denied);
            }
            serve(resp, secret)
        }
        Outcome::AllowRemember => {
            if secret.len() > SECRET_MAX_BYTES {
                return deny(resp, DenyReason::Denied);
            }
            if let Some(kc) = deps.keychain {
                // Remember-store failure must not block an approved delivery:
                // serve once, log the storage error.
                if let Err(err) = kc.set(label, &secret) {
                    tracing::warn!(target: "portal::cred", "keychain store failed (serving once): {err}");
                }
            }
            serve(resp, secret)
        }
        Outcome::Deny => {
            deps.cooldown.record(label, (deps.now)());
            deny(resp, DenyReason::Denied)
        }
        Outcome::Timeout => deny(resp, DenyReason::Timeout),
        Outcome::Forget | Outcome::Unavailable => deny(resp, DenyReason::GuiUnavailable),
    }
}

fn keychain_secret(deps: &ServeDeps<'_>, label: &str) -> Option<Vec<u8>> {
    let secret = deps.keychain?.get(label).ok().flatten()?;
    (secret.len() <= SECRET_MAX_BYTES).then_some(secret)
}

/// Port of credPromptTimeoutSecs: seconds remaining in the dialog budget,
/// clamped to [DIALOG_MIN_SECS, DIALOG_MAX_SECS]; None when the budget is
/// already below the minimum a dialog needs.
fn prompt_timeout(deps: &ServeDeps<'_>, deadline: Instant) -> Option<Duration> {
    let remaining = deadline.saturating_duration_since((deps.now)());
    if remaining < Duration::from_secs(DIALOG_MIN_SECS) {
        return None;
    }
    Some(remaining.min(Duration::from_secs(DIALOG_MAX_SECS)))
}

fn deny(mut resp: CredResponse, reason: DenyReason) -> CredResponse {
    resp.ok = false;
    resp.secret = None;
    resp.err = Some(reason.as_str().to_string());
    resp
}

fn serve(mut resp: CredResponse, secret: Vec<u8>) -> CredResponse {
    debug_assert!(secret.len() <= SECRET_MAX_BYTES);
    resp.ok = true;
    resp.err = None;
    resp.secret = Some(ByteBuf::from(secret));
    resp
}

// Suppress unused-import warning for LABEL_MAX_BYTES which documents the
// valid_label contract used above.
const _: usize = LABEL_MAX_BYTES;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::keychain::MemoryKeychain;
    use std::sync::Mutex;

    struct ScriptedPrompter {
        decisions: Mutex<Vec<Decision>>,
        seen: Mutex<Vec<Request>>,
    }

    impl ScriptedPrompter {
        fn new(decisions: Vec<Decision>) -> Self {
            Self {
                decisions: Mutex::new(decisions),
                seen: Mutex::new(Vec::new()),
            }
        }
        fn requests(&self) -> Vec<Request> {
            self.seen.lock().unwrap().clone()
        }
    }

    impl Prompter for ScriptedPrompter {
        fn prompt(&self, req: &Request) -> Decision {
            self.seen.lock().unwrap().push(req.clone());
            let mut d = self.decisions.lock().unwrap();
            if d.is_empty() {
                Decision::of(Outcome::Unavailable)
            } else {
                d.remove(0)
            }
        }
    }

    struct FakeBiometry {
        outcome: Result<BiometryOutcome, String>,
    }
    impl Biometry for FakeBiometry {
        fn available(&self) -> bool {
            true
        }
        fn approve(&self, _reason: &str, _timeout: Duration) -> Result<BiometryOutcome, String> {
            self.outcome.clone()
        }
    }

    fn req(label: &str, mode: &str) -> CredRequest {
        CredRequest {
            nonce: 7,
            epoch: 1,
            label: label.into(),
            requester: Some("pid 4242: sudo".into()),
            mode: mode.into(),
            target: Some("PW".into()),
        }
    }

    fn assert_denied(resp: &CredResponse, reason: DenyReason) {
        assert!(!resp.ok);
        assert_eq!(resp.err.as_deref(), Some(reason.as_str()));
        assert!(resp.secret.is_none());
        assert_eq!((resp.nonce, resp.epoch), (7, 1));
    }

    struct Env {
        prompter: ScriptedPrompter,
        keychain: MemoryKeychain,
        cooldown: Cooldown,
        cred_on: bool,
        touchid_on: bool,
    }

    impl Env {
        fn new(decisions: Vec<Decision>) -> Self {
            Self {
                prompter: ScriptedPrompter::new(decisions),
                keychain: MemoryKeychain::default(),
                cooldown: Cooldown::default(),
                cred_on: true,
                touchid_on: false,
            }
        }

        fn serve(&self, biometry: Option<&dyn Biometry>, r: &CredRequest) -> CredResponse {
            let features = |name: &str| match name {
                FEATURE_CRED => self.cred_on,
                FEATURE_CRED_TOUCHID => self.touchid_on,
                _ => true,
            };
            let now = Instant::now;
            let deps = ServeDeps {
                prompter: &self.prompter,
                biometry,
                keychain: Some(&self.keychain),
                features: &features,
                cooldown: &self.cooldown,
                host: "devbox1",
                now: &now,
            };
            serve_cred_request(&deps, r)
        }
    }

    #[test]
    fn disabled_gate_denies_without_prompting() {
        let mut env = Env::new(vec![]);
        env.cred_on = false;
        let resp = env.serve(None, &req("sudo", "askpass"));
        assert_denied(&resp, DenyReason::Disabled);
        assert!(env.prompter.requests().is_empty());
    }

    #[test]
    fn invalid_label_denies() {
        let env = Env::new(vec![]);
        assert_denied(
            &env.serve(None, &req("\x07\x08", "env")),
            DenyReason::LabelInvalid,
        );
        assert_denied(
            &env.serve(None, &req(&"x".repeat(201), "env")),
            DenyReason::LabelInvalid,
        );
    }

    #[test]
    fn allow_once_serves_typed_secret_without_storing() {
        let env = Env::new(vec![Decision {
            outcome: Outcome::AllowOnce,
            secret: b"s3kr3t".to_vec(),
        }]);
        let resp = env.serve(None, &req("staging admin", "env"));
        assert!(resp.ok);
        assert_eq!(resp.secret.as_ref().unwrap().as_slice(), b"s3kr3t");
        assert_eq!(env.keychain.list().unwrap().len(), 0);
        // dialog saw the sanitized/structured request
        let seen = &env.prompter.requests()[0];
        assert_eq!(seen.label, "staging admin");
        assert_eq!(seen.mode, Mode::Env);
        assert!(!seen.remembered);
    }

    #[test]
    fn allow_remember_stores_then_serves() {
        let env = Env::new(vec![Decision {
            outcome: Outcome::AllowRemember,
            secret: b"pw".to_vec(),
        }]);
        let resp = env.serve(None, &req("sudo", "askpass"));
        assert!(resp.ok);
        assert_eq!(env.keychain.get("sudo").unwrap().unwrap(), b"pw");
    }

    #[test]
    fn deny_records_cooldown_and_next_request_is_cooled() {
        let env = Env::new(vec![Decision::of(Outcome::Deny)]);
        let resp = env.serve(None, &req("sudo", "askpass"));
        assert_denied(&resp, DenyReason::Denied);
        let resp = env.serve(None, &req("sudo", "askpass"));
        assert_denied(&resp, DenyReason::Cooldown);
        assert_eq!(env.prompter.requests().len(), 1); // second never prompted
    }

    #[test]
    fn remembered_with_touchid_serves_from_keychain_without_dialog() {
        let mut env = Env::new(vec![]);
        env.touchid_on = true;
        env.keychain.set("sudo", b"stored").unwrap();
        let bio = FakeBiometry {
            outcome: Ok(BiometryOutcome::Approved),
        };
        let resp = env.serve(Some(&bio), &req("sudo", "askpass"));
        assert!(resp.ok);
        assert_eq!(resp.secret.as_ref().unwrap().as_slice(), b"stored");
        assert!(env.prompter.requests().is_empty());
    }

    #[test]
    fn touchid_cancel_denies_and_cools_down() {
        let mut env = Env::new(vec![]);
        env.touchid_on = true;
        env.keychain.set("sudo", b"stored").unwrap();
        let bio = FakeBiometry {
            outcome: Ok(BiometryOutcome::Canceled),
        };
        assert_denied(
            &env.serve(Some(&bio), &req("sudo", "askpass")),
            DenyReason::Denied,
        );
        assert_denied(
            &env.serve(Some(&bio), &req("sudo", "askpass")),
            DenyReason::Cooldown,
        );
    }

    #[test]
    fn touchid_error_falls_back_to_dialog() {
        let mut env = Env::new(vec![Decision::of(Outcome::AllowRemember)]);
        env.touchid_on = true;
        env.keychain.set("sudo", b"stored").unwrap();
        let bio = FakeBiometry {
            outcome: Err("lockout".into()),
        };
        let resp = env.serve(Some(&bio), &req("sudo", "askpass"));
        assert!(resp.ok, "dialog AllowRemember must serve the stored secret");
        assert_eq!(env.prompter.requests().len(), 1);
        assert!(env.prompter.requests()[0].remembered);
    }

    #[test]
    fn forget_deletes_then_reprompts_fresh() {
        let env = Env::new(vec![
            Decision::of(Outcome::Forget),
            Decision {
                outcome: Outcome::AllowOnce,
                secret: b"new".to_vec(),
            },
        ]);
        env.keychain.set("db pass", b"old").unwrap();
        let resp = env.serve(None, &req("db pass", "stdin"));
        assert!(resp.ok);
        assert_eq!(resp.secret.as_ref().unwrap().as_slice(), b"new");
        assert!(env.keychain.get("db pass").unwrap().is_none());
        let seen = env.prompter.requests();
        assert!(seen[0].remembered && !seen[1].remembered);
    }

    #[test]
    fn oversized_secret_fails_closed_as_denied() {
        let env = Env::new(vec![Decision {
            outcome: Outcome::AllowOnce,
            secret: vec![b'x'; SECRET_MAX_BYTES + 1],
        }]);
        assert_denied(&env.serve(None, &req("big", "env")), DenyReason::Denied);
    }

    #[test]
    fn prompt_unavailable_maps_to_gui_unavailable() {
        let env = Env::new(vec![Decision::of(Outcome::Unavailable)]);
        assert_denied(
            &env.serve(None, &req("sudo", "askpass")),
            DenyReason::GuiUnavailable,
        );
    }
}
