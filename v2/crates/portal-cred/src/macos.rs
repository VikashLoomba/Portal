//! macOS backends: Keychain (security-framework, in-process — no `security`
//! subprocess, secrets never near argv) and Touch ID (LAContext via objc2 —
//! correctly-attributed sheet, typed LAError, per TASKS.md design decision).
//!
//! The SecAccessControl/biometryCurrentSet item binding is RELEASE-GATED
//! (needs Developer ID signing; TASKS.md): until then items are ordinary
//! generic passwords and the in-process LAContext gate guards release — the
//! same guarantee v1 shipped, minus the osascript warts.

use security_framework::passwords as kc;

use crate::keychain::Keychain;
use crate::prompt::{Biometry, BiometryOutcome};

/// Keychain service namespace (one service, labels as accounts — mirrors
/// v1's portal-namespaced generic passwords).
pub const SERVICE: &str = "portal.credentials";

/// With the Developer ID signing lane (v2 release pipeline), newly-stored
/// items are bound with SecAccessControl biometryCurrentSet: the KEYCHAIN
/// ITSELF enforces user presence on read — closing v1's "does not re-bind
/// the item" caveat. On unsigned dev builds we skip the binding (no
/// entitlement → SecItemAdd would fail) and the in-process LAContext gate
/// guards release instead.
fn signed_binary() -> bool {
    std::env::var("PORTAL_SIGNED")
        .map(|v| v == "1")
        .unwrap_or(false)
}

#[derive(Debug, Default)]
pub struct MacKeychain;

impl Keychain for MacKeychain {
    fn get(&self, label: &str) -> Result<Option<Vec<u8>>, String> {
        match kc::get_generic_password(SERVICE, label) {
            Ok(secret) => Ok(Some(secret)),
            Err(e) if e.code() == security_framework_err_item_not_found() => Ok(None),
            Err(e) => Err(e.to_string()),
        }
    }

    fn set(&self, label: &str, secret: &[u8]) -> Result<(), String> {
        if signed_binary() {
            use security_framework::access_control::{ProtectionMode, SecAccessControl};
            use security_framework::passwords::PasswordOptions;
            use security_framework::passwords_options::AccessControlOptions;
            let mut options = PasswordOptions::new_generic_password(SERVICE, label);
            if let Ok(ac) = SecAccessControl::create_with_protection(
                Some(ProtectionMode::AccessibleWhenUnlockedThisDeviceOnly),
                AccessControlOptions::BIOMETRY_CURRENT_SET.bits(),
            ) {
                options.set_access_control(ac);
                return kc::set_generic_password_options(secret, options)
                    .map_err(|e| e.to_string());
            }
            // Binding creation failed on a signed build: fall back to the
            // plain item rather than refusing to remember.
        }
        kc::set_generic_password(SERVICE, label, secret).map_err(|e| e.to_string())
    }

    fn delete(&self, label: &str) -> Result<(), String> {
        match kc::delete_generic_password(SERVICE, label) {
            Ok(()) => Ok(()),
            Err(e) if e.code() == security_framework_err_item_not_found() => Ok(()), // tolerate absence
            Err(e) => Err(e.to_string()),
        }
    }

    fn list(&self) -> Result<Vec<String>, String> {
        // security-framework's password API has no enumeration; use ItemSearch.
        use security_framework::item::{ItemClass, ItemSearchOptions, SearchResult};
        let results = ItemSearchOptions::new()
            .class(ItemClass::generic_password())
            .service(SERVICE)
            .load_attributes(true)
            .limit(i32::MAX as i64)
            .search();
        match results {
            Ok(items) => {
                let mut labels: Vec<String> = items
                    .into_iter()
                    .filter_map(|r| match r {
                        SearchResult::Dict(_) => {
                            r.simplify_dict().and_then(|d| d.get("acct").cloned())
                        }
                        _ => None,
                    })
                    .collect();
                labels.sort();
                labels.dedup();
                Ok(labels)
            }
            Err(e) if e.code() == security_framework_err_item_not_found() => Ok(Vec::new()),
            Err(e) => Err(e.to_string()),
        }
    }
}

fn security_framework_err_item_not_found() -> i32 {
    // errSecItemNotFound
    -25300
}

/// Touch ID / Apple Watch via LAContext — in-process (objc2).
#[derive(Debug, Default)]
pub struct MacBiometry;

impl Biometry for MacBiometry {
    fn available(&self) -> bool {
        use objc2_local_authentication::{LAContext, LAPolicy};
        let ctx = unsafe { LAContext::new() };
        unsafe {
            ctx.canEvaluatePolicy_error(LAPolicy::DeviceOwnerAuthenticationWithBiometrics)
                .is_ok()
        }
    }

    /// Show the (portal-attributed) system sheet; block until decided or
    /// `timeout`. evaluatePolicy calls back on a private queue; we bridge to
    /// sync with a channel and invalidate() the context on timeout so the
    /// sheet is programmatically dismissed (impossible under v1's osascript).
    fn approve(
        &self,
        reason: &str,
        timeout: std::time::Duration,
    ) -> Result<BiometryOutcome, String> {
        use block2::RcBlock;
        use objc2_foundation::NSString;
        use objc2_local_authentication::{LAContext, LAError, LAPolicy};
        use std::sync::mpsc;

        let ctx = unsafe { LAContext::new() };
        let (tx, rx) = mpsc::channel::<Result<(), (isize, String)>>();
        let block = RcBlock::new(
            move |success: objc2::runtime::Bool, error: *mut objc2_foundation::NSError| {
                let result = if success.as_bool() {
                    Ok(())
                } else if error.is_null() {
                    Err((0, "evaluation failed".to_string()))
                } else {
                    let err = unsafe { &*error };
                    Err((err.code(), err.localizedDescription().to_string()))
                };
                let _ = tx.send(result);
            },
        );
        unsafe {
            ctx.evaluatePolicy_localizedReason_reply(
                LAPolicy::DeviceOwnerAuthenticationWithBiometrics,
                &NSString::from_str(reason),
                &block,
            );
        }
        match rx.recv_timeout(timeout) {
            Ok(Ok(())) => Ok(BiometryOutcome::Approved),
            Ok(Err((code, msg))) => {
                // LAError codes: UserCancel(-2)/AppCancel(-9)/SystemCancel(-4)
                // are explicit non-approvals; everything else (lockout, not
                // enrolled, passcode fallback) is an evaluation failure that
                // the policy core turns into a dialog fallback.
                if code == LAError::UserCancel.0
                    || code == LAError::AppCancel.0
                    || code == LAError::SystemCancel.0
                {
                    Ok(BiometryOutcome::Canceled)
                } else {
                    Err(format!("LAError {code}: {msg}"))
                }
            }
            Err(_) => {
                unsafe { ctx.invalidate() }; // dismiss the sheet
                Ok(BiometryOutcome::Timeout)
            }
        }
    }
}
