//! Shared NSApplication activation for Portal's AppKit processes (the
//! tray/management window and the `portal _prompt` credential helper).
//!
//! `-[NSApplication activate]` exists only on macOS 14+; Portal deploys to
//! macOS 13, where sending it would be an unrecognized-selector crash. Every
//! AppKit entry point must activate through [`activate_app`], which probes
//! the selector at runtime and falls back to `activateIgnoringOtherApps:` —
//! deprecated on 14+, but functional and the only activation API macOS 13
//! has.

use objc2::sel;
use objc2_app_kit::NSApplication;
use objc2_foundation::NSObjectProtocol;

/// The activation API the running macOS supports, decided by the runtime
/// selector probe in [`activate_app`]. Pure so both branches — including the
/// macOS 13 fallback a macOS 14+ dev machine never exercises — are
/// unit-testable.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ActivationApi {
    /// `-[NSApplication activate]` — macOS 14 and later.
    Activate,
    /// `-[NSApplication activateIgnoringOtherApps:]` — deprecated on
    /// macOS 14, but the only activation API macOS 13 has.
    ActivateIgnoringOtherApps,
}

pub(crate) fn activation_api(supports_activate: bool) -> ActivationApi {
    if supports_activate {
        ActivationApi::Activate
    } else {
        ActivationApi::ActivateIgnoringOtherApps
    }
}

/// Bring Portal's windows and alerts forward on every macOS Portal supports.
pub(crate) fn activate_app(app: &NSApplication) {
    match activation_api(app.respondsToSelector(sel!(activate))) {
        ActivationApi::Activate => app.activate(),
        ActivationApi::ActivateIgnoringOtherApps => {
            #[allow(deprecated)]
            app.activateIgnoringOtherApps(true);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gate_prefers_modern_activate_where_it_exists() {
        assert_eq!(activation_api(true), ActivationApi::Activate);
    }

    #[test]
    fn gate_falls_back_on_macos_13() {
        // The blocker this pins: macOS 13 has no `activate` selector, so the
        // probe there MUST select the legacy API — never the 14+ one.
        assert_eq!(
            activation_api(false),
            ActivationApi::ActivateIgnoringOtherApps
        );
    }

    #[test]
    fn helper_keeps_the_runtime_gate_and_fallback() {
        // Only the production half of this file: the test module's own
        // assertion strings must not satisfy the check.
        let production = include_str!("activation.rs")
            .split("#[cfg(test)]")
            .next()
            .expect("production source");
        assert!(
            production.contains("respondsToSelector(sel!(activate))"),
            "activation stays gated on the runtime selector probe"
        );
        assert!(
            production.contains("activateIgnoringOtherApps(true)"),
            "the macOS 13 fallback stays wired"
        );
    }

    /// The regression this pins: `portal _prompt` brought its alert forward
    /// with a bare `app.activate()` — a selector that exists only on
    /// macOS 14 — while Portal deploys to macOS 13. Every AppKit entry point
    /// in this crate must go through this module's gate instead.
    #[test]
    fn no_entry_point_bypasses_the_gate() {
        let src_dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
        for entry in std::fs::read_dir(&src_dir).expect("read portal-cli src/") {
            let path = entry.expect("dir entry").path();
            if path.extension().and_then(|e| e.to_str()) != Some("rs") {
                continue;
            }
            if path.file_name().and_then(|n| n.to_str()) == Some("activation.rs") {
                continue; // the gate itself
            }
            let text = std::fs::read_to_string(&path).expect("read source file");
            assert!(
                !text.contains(".activate()"),
                "{} sends the macOS 14-only activate selector directly; \
                 use crate::activation::activate_app",
                path.display()
            );
        }
    }
}
