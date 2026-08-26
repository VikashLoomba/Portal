//! GUI-facing update classification shared with the native SwiftUI host.
//!
//! Network, verification, staging, and installation remain in the existing
//! Rust updater. This module turns its command output into typed presentation
//! state without retaining the removed Rust/AppKit tray implementation.

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum UpdateCheck {
    Current(String),
    Available { tag: String, message: String },
    Migration { tag: String },
}

fn parse_up_to_date_version(message: &str) -> Option<&str> {
    for marker in ["(latest ", "current ("] {
        if let Some((_, rest)) = message.split_once(marker)
            && let Some(tag) = rest.split(')').next().filter(|tag| tag.starts_with('v'))
        {
            return Some(tag);
        }
    }
    None
}

fn classify_update_check(success: bool, stdout: &str, stderr: &str) -> Result<UpdateCheck, String> {
    let message = stdout
        .trim()
        .strip_prefix("portal: ")
        .unwrap_or(stdout.trim());
    if !success {
        let error = stderr.trim();
        return Err(if error.is_empty() {
            message.to_string()
        } else {
            error
                .strip_prefix("portal upgrade: ")
                .unwrap_or(error)
                .to_string()
        });
    }
    if let Some(rest) = message.strip_prefix("new release available: ") {
        let tag = rest
            .split_whitespace()
            .next()
            .filter(|tag| tag.starts_with('v'))
            .ok_or_else(|| format!("unexpected update response: {message}"))?;
        return Ok(UpdateCheck::Available {
            tag: tag.to_string(),
            message: message.to_string(),
        });
    }
    if let Some(rest) = message.strip_prefix("Portal.app migration available") {
        let tag = rest
            .strip_prefix(" for current release")
            .and_then(|tail| tail.split_whitespace().next())
            .filter(|tag| tag.starts_with('v'))
            .map(str::to_string)
            .unwrap_or_else(|| format!("v{}", env!("CARGO_PKG_VERSION")));
        return Ok(UpdateCheck::Migration { tag });
    }
    if message.contains(" is up to date ") {
        let version = parse_up_to_date_version(message)
            .ok_or_else(|| format!("unexpected update response: {message}"))?;
        return Ok(UpdateCheck::Current(version.to_string()));
    }
    Err(format!("unexpected update response: {message}"))
}

pub fn run_update_check(executable: &std::path::Path) -> Result<UpdateCheck, String> {
    let output = std::process::Command::new(executable)
        .args(["upgrade", "--check"])
        .output()
        .map_err(|error| format!("could not run the updater: {error}"))?;
    classify_update_check(
        output.status.success(),
        &String::from_utf8_lossy(&output.stdout),
        &String::from_utf8_lossy(&output.stderr),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classifies_available_current_migration_and_failure() {
        assert_eq!(
            classify_update_check(
                true,
                "portal: new release available: v2.1.0 (current v2.0.27)\n",
                "",
            ),
            Ok(UpdateCheck::Available {
                tag: "v2.1.0".into(),
                message: "new release available: v2.1.0 (current v2.0.27)".into(),
            })
        );
        assert_eq!(
            classify_update_check(
                true,
                "portal: current (v2.0.27) is up to date (latest v2.0.27)\n",
                "",
            ),
            Ok(UpdateCheck::Current("v2.0.27".into()))
        );
        assert_eq!(
            classify_update_check(
                true,
                "portal: Portal.app migration available for current release v2.0.27\n",
                "",
            ),
            Ok(UpdateCheck::Migration {
                tag: "v2.0.27".into(),
            })
        );
        assert_eq!(
            classify_update_check(false, "", "portal upgrade: network unavailable\n"),
            Err("network unavailable".into())
        );
    }
}
