//! Credential sharing — the `portal keychain` / askpass / transparent-sudo
//! feature (port of cmd/portal/run_cred.go, internal/keychain, and the
//! cred halves of pkg/agent + cmd/portald).
//!
//! The guarantee carried over from v1: the secret never enters the agent's
//! context window or transcript, process argv, portal logs, or the box's
//! disk; it travels in memory from the Mac Keychain/consent dialog down the
//! existing pipe to the consumer process. Consent (dialog or Touch ID) and
//! the audit log are the control points.
//!
//! Layout:
//! - [`serve`]    — the Mac-side policy core (gates → cooldown → biometry →
//!   prompt → keychain), fully ported and unit-tested against fakes;
//! - [`cooldown`] — per-label denial cooldown;
//! - [`prompt`]   — consent dialog + Touch ID seams (backends pending);
//! - [`keychain`] — remembered-secret storage seam (backend pending);
//! - this module — wire limits, modes, and deny reasons (must match the Go
//!   peers byte-for-byte; they cross the v4 protocol).

pub mod cooldown;
pub mod helper;
pub mod keychain;
#[cfg(target_os = "macos")]
pub mod macos;
pub mod prompt;
pub mod serve;

use std::fmt;
use std::str::FromStr;
use std::time::Duration;

/// Wire caps (SPEC/DESIGN-cred): enforced on BOTH endpoints.
pub const LABEL_MAX_BYTES: usize = 200;
pub const CONTEXT_MAX_BYTES: usize = 300; // requester and target, each
pub const SECRET_MAX_BYTES: usize = 4096;

/// Policy timings (cmd/portal/run_cred.go).
pub const DENY_COOLDOWN: Duration = Duration::from_secs(10);
pub const DIALOG_BUDGET: Duration = Duration::from_secs(115);
pub const DIALOG_MIN_SECS: u64 = 5;
pub const DIALOG_MAX_SECS: u64 = 120;

/// Capability-gate keys (files under ~/.config/portal/, same as v1).
pub const FEATURE_CRED: &str = "cred";
pub const FEATURE_CRED_TOUCHID: &str = "cred-touchid";

/// How the secret reaches the consumer process on the box.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    /// Injected as an environment variable of the wrapped child.
    Env,
    /// Written to the wrapped child's stdin.
    Stdin,
    /// Answering a sudo SUDO_ASKPASS prompt.
    Askpass,
}

impl Mode {
    pub fn as_str(self) -> &'static str {
        match self {
            Mode::Env => "env",
            Mode::Stdin => "stdin",
            Mode::Askpass => "askpass",
        }
    }
}

impl FromStr for Mode {
    type Err = ();
    fn from_str(s: &str) -> Result<Self, ()> {
        match s {
            "env" => Ok(Mode::Env),
            "stdin" => Ok(Mode::Stdin),
            "askpass" => Ok(Mode::Askpass),
            _ => Err(()),
        }
    }
}

/// Machine-readable denial reasons — the EXACT wire vocabulary of
/// `CredResponse.err` (protocol/messages.go). The box-side keychain runner
/// maps these to user-facing hints, so tokens must never drift.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DenyReason {
    Denied,
    Timeout,
    Disabled,
    Cooldown,
    GuiUnavailable,
    LabelInvalid,
    NoClient,
    Busy,
}

impl DenyReason {
    pub fn as_str(self) -> &'static str {
        match self {
            DenyReason::Denied => "denied",
            DenyReason::Timeout => "timeout",
            DenyReason::Disabled => "disabled",
            DenyReason::Cooldown => "cooldown",
            DenyReason::GuiUnavailable => "gui-unavailable",
            DenyReason::LabelInvalid => "label-invalid",
            DenyReason::NoClient => "no-client",
            DenyReason::Busy => "busy",
        }
    }
}

impl fmt::Display for DenyReason {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl FromStr for DenyReason {
    type Err = ();
    fn from_str(s: &str) -> Result<Self, ()> {
        Ok(match s {
            "denied" => DenyReason::Denied,
            "timeout" => DenyReason::Timeout,
            "disabled" => DenyReason::Disabled,
            "cooldown" => DenyReason::Cooldown,
            "gui-unavailable" => DenyReason::GuiUnavailable,
            "label-invalid" => DenyReason::LabelInvalid,
            "no-client" => DenyReason::NoClient,
            "busy" => DenyReason::Busy,
            _ => return Err(()),
        })
    }
}

/// Strip control characters (port of stripCredControls): the label/requester/
/// target render inside a native dialog, so escape sequences and newlines
/// must never survive into it.
pub fn strip_controls(s: &str) -> String {
    s.chars().filter(|c| !c.is_control()).collect()
}

/// Truncate to a byte budget on a char boundary (port of truncateCredText).
pub fn truncate_text(s: &str, max_bytes: usize) -> &str {
    if s.len() <= max_bytes {
        return s;
    }
    let mut end = max_bytes;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    &s[..end]
}

/// A label is valid iff, after control-stripping, it is non-empty and within
/// the wire cap.
pub fn valid_label(stripped: &str) -> bool {
    !stripped.is_empty() && stripped.len() <= LABEL_MAX_BYTES
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deny_reasons_roundtrip_wire_tokens() {
        for r in [
            DenyReason::Denied,
            DenyReason::Timeout,
            DenyReason::Disabled,
            DenyReason::Cooldown,
            DenyReason::GuiUnavailable,
            DenyReason::LabelInvalid,
            DenyReason::NoClient,
            DenyReason::Busy,
        ] {
            assert_eq!(r.as_str().parse::<DenyReason>().unwrap(), r);
        }
        assert!("nonsense".parse::<DenyReason>().is_err());
    }

    #[test]
    fn strips_controls_and_truncates_on_char_boundary() {
        assert_eq!(strip_controls("a\x07b\r\nc\td"), "abcd");
        assert_eq!(truncate_text("héllo", 3), "h\u{e9}"); // é is 2 bytes
        assert_eq!(truncate_text("short", 200), "short");
    }

    #[test]
    fn label_validation() {
        assert!(valid_label("staging admin"));
        assert!(!valid_label(""));
        assert!(!valid_label(&"x".repeat(LABEL_MAX_BYTES + 1)));
        assert!(valid_label(&"x".repeat(LABEL_MAX_BYTES)));
    }

    #[test]
    fn modes_parse() {
        assert_eq!("askpass".parse::<Mode>().unwrap(), Mode::Askpass);
        assert!("root".parse::<Mode>().is_err());
    }
}
