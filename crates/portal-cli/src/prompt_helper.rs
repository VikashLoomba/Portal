//! `portal _prompt` — the consent-dialog helper process (hidden subcommand).
//! Reads one PromptRequest (JSON, stdin→EOF), shows a native NSAlert with an
//! NSSecureTextField accessory, writes one PromptDecision (JSON, stdout).
//!
//! Why a helper process: AppKit modal UI needs a main-thread NSApplication,
//! which the headless daemon is not. Why NSAlert and not AppleScript:
//! attacker-influenced strings become widget PROPERTIES (no script-source
//! injection), and NSAlert has no 3-button cap — remembered labels get all
//! four outcomes as real buttons (TASKS.md design decision).

use portal_cred::helper::{PromptDecision, PromptRequest};

pub fn run() -> i32 {
    use std::io::Read;
    let mut input = String::new();
    if std::io::stdin().read_to_string(&mut input).is_err() {
        return emit(PromptDecision {
            outcome: "unavailable".into(),
            secret: None,
        });
    }
    let req: PromptRequest = match serde_json::from_str(&input) {
        Ok(r) => r,
        Err(_) => {
            return emit(PromptDecision {
                outcome: "unavailable".into(),
                secret: None,
            });
        }
    };
    let decision = show_alert(&req);
    emit(decision)
}

fn emit(d: PromptDecision) -> i32 {
    match serde_json::to_string(&d) {
        Ok(s) => {
            println!("{s}");
            0
        }
        Err(_) => 1,
    }
}

#[cfg(target_os = "macos")]
fn show_alert(req: &PromptRequest) -> PromptDecision {
    use objc2::rc::Retained;
    use objc2::{MainThreadMarker, MainThreadOnly};
    use objc2_app_kit::{
        NSAlert, NSAlertFirstButtonReturn, NSApplication, NSApplicationActivationPolicy, NSImage,
        NSSecureTextField,
    };
    use objc2_foundation::{NSPoint, NSRect, NSSize, NSString};

    // This subcommand IS the main thread (fresh process).
    let Some(mtm) = MainThreadMarker::new() else {
        return PromptDecision {
            outcome: "unavailable".into(),
            secret: None,
        };
    };
    // Activate as an accessory app so the alert can take key focus without a
    // Dock icon.
    let app = NSApplication::sharedApplication(mtm);
    app.setActivationPolicy(NSApplicationActivationPolicy::Accessory);
    // `runModal` can otherwise finish application launch lazily while the
    // alert is already animating in. That made the accessory field appear a
    // moment after the buttons and left users looking at an incomplete prompt.
    app.finishLaunching();

    let alert = NSAlert::new(mtm);
    if let Some(icon) = NSImage::imageWithSystemSymbolName_accessibilityDescription(
        &NSString::from_str("lock.shield.fill"),
        Some(&NSString::from_str("Secure credential approval")),
    ) {
        // SAFETY: the retained NSImage remains alive for the alert's lifetime.
        unsafe { alert.setIcon(Some(&icon)) };
    }
    let delivery = match req.mode.as_str() {
        "askpass" => "a sudo password prompt".to_string(),
        "env" => format!("environment variable {:?}", req.target),
        _ => "the command's standard input".to_string(),
    };
    alert.setMessageText(&NSString::from_str(&format!(
        "portal: credential \"{}\"",
        req.label
    )));
    alert.setInformativeText(&NSString::from_str(&format!(
        "{} on {} requests this credential.\nDelivery: {}.{}",
        req.requester,
        req.host,
        delivery,
        if req.remembered {
            "\nA remembered secret exists in your Keychain."
        } else {
            ""
        }
    )));

    // Button order defines return codes: First, First+1, ...
    // Fresh labels: [Allow Once] [Allow & Remember] [Deny]
    //   (askpass+biometrics flips remember first — v1's default.)
    // Remembered:   [Allow & Remember] [Allow Once→n/a] [Forget] [Deny]
    let buttons: Vec<&str> = if req.remembered {
        vec!["Approve", "Forget", "Deny"]
    } else if req.touch_id_enroll {
        vec!["Allow & Remember", "Allow Once", "Deny"]
    } else {
        vec!["Allow Once", "Allow & Remember", "Deny"]
    };
    for b in &buttons {
        alert.addButtonWithTitle(&NSString::from_str(b));
    }

    // Secret entry only for FRESH prompts (remembered secrets come from the
    // Keychain after approval — typing is not needed).
    let field: Option<Retained<NSSecureTextField>> = if req.remembered {
        None
    } else {
        let frame = NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(280.0, 28.0));
        let field = NSSecureTextField::initWithFrame(NSSecureTextField::alloc(mtm), frame);
        field.setPlaceholderString(Some(&NSString::from_str("Password")));
        alert.setAccessoryView(Some(&field));
        Some(field)
    };

    // Force the complete alert—including its accessory—to lay out before it
    // becomes visible, and put keyboard focus in the password field.
    alert.layout();
    if let Some(field) = field.as_ref() {
        alert.window().setInitialFirstResponder(Some(field));
    }
    app.activate();
    let response = alert.runModal();
    let idx = (response - NSAlertFirstButtonReturn) as usize;
    let chosen = buttons.get(idx).copied().unwrap_or("Deny");

    let secret = field.map(|f| f.stringValue().to_string());
    let outcome = match chosen {
        "Approve" | "Allow & Remember" => "allow-remember",
        "Allow Once" => "allow-once",
        "Forget" => "forget",
        _ => "deny",
    };
    // An empty typed secret on an allow is a mis-click, not consent.
    if matches!(outcome, "allow-once" | "allow-remember")
        && !req.remembered
        && secret.as_deref().unwrap_or("").is_empty()
    {
        return PromptDecision {
            outcome: "deny".into(),
            secret: None,
        };
    }
    PromptDecision {
        outcome: outcome.into(),
        secret: if req.remembered { None } else { secret },
    }
}

#[cfg(not(target_os = "macos"))]
fn show_alert(_req: &PromptRequest) -> PromptDecision {
    PromptDecision {
        outcome: "unavailable".into(),
        secret: None,
    }
}
