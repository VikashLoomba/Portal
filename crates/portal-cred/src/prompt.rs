//! Consent surfaces: the native secure-input dialog and the Touch ID release
//! gate. Traits only — backends land in the cred phase.
//!
//! Backend plan (cgo-free):
//! - Dialog: a `display dialog … with hidden answer` prompt showing WHICH
//!   process asked, WHICH box it came from, and HOW the secret will be
//!   delivered. Buttons: Allow Once / Allow & Remember / Deny (+ Forget for
//!   remembered labels). Default button: Allow & Remember for
//!   askpass-with-biometrics, Allow Once for direct --env/--stdin.
//! - Biometry: v1 invokes macOS LocalAuthentication through
//!   `osascript -l JavaScript` (the sheet shows as "osascript"); Rust can
//!   either port that verbatim or call LocalAuthentication via objc2 for a
//!   properly-attributed sheet. Touch ID gates the RELEASE decision only; it
//!   does not re-bind Keychain items to biometrics (v1 caveat carried over).

use std::time::Duration;

/// What the user decided in the dialog. `Forget` only appears for remembered
/// labels; `secret` accompanies AllowOnce/AllowRemember for fresh prompts
/// (the user typed it into the hidden-answer field).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    AllowOnce,
    AllowRemember,
    Forget,
    Deny,
    Timeout,
    Unavailable,
}

/// Zeroized on drop (secrets must not linger in freed heap).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Decision {
    pub outcome: Outcome,
    pub secret: Vec<u8>,
}

impl Drop for Decision {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.secret.zeroize();
    }
}

impl Decision {
    pub fn of(outcome: Outcome) -> Self {
        Self {
            outcome,
            secret: Vec::new(),
        }
    }
}

/// The dialog request. Structured delivery description (v1 renders it into
/// the dialog body) so the UI layer owns the exact wording.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Request {
    pub label: String,
    pub requester: String,
    pub host: String,
    pub mode: crate::Mode,
    /// env-var name or askpass prompt line (already sanitized + truncated).
    pub target: String,
    pub remembered: bool,
    /// Offer "type once, remember, gate future releases behind Touch ID".
    pub touch_id_enroll: bool,
    pub timeout: Duration,
}

/// Native consent dialog. Blocking (the daemon calls it from a blocking
/// task); MUST return `Outcome::Timeout` no later than `req.timeout`.
pub trait Prompter: Send + Sync {
    fn prompt(&self, req: &Request) -> Decision;
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BiometryOutcome {
    Approved,
    Canceled,
    Timeout,
}

/// Touch ID / Apple Watch release gate.
pub trait Biometry: Send + Sync {
    /// Whether biometrics can currently evaluate (enrolled, not locked out).
    fn available(&self) -> bool;
    /// Show the system sheet with `reason`; block until decided or `timeout`.
    /// `Err` means the evaluation itself failed (fall back to the dialog).
    fn approve(&self, reason: &str, timeout: Duration) -> Result<BiometryOutcome, String>;
}
