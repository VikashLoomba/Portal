//! The prompt-helper wire format: the daemon spawns `portal _prompt`,
//! writes one PromptRequest as JSON on stdin, reads one PromptDecision as
//! JSON on stdout. Keeping the (de)serialization + outcome mapping here (not
//! in the CLI) makes both ends share one definition and keeps the policy
//! core's Prompter seam trivial to implement over any helper binary.
//!
//! Design (TASKS.md, "NO osascript in security-critical paths"): the helper
//! hosts a native NSAlert + NSSecureTextField. Attacker-influenced strings
//! (label/requester) are rendered into WIDGET PROPERTIES, never interpolated
//! into script source — the AppleScript injection surface is gone by
//! construction. NSAlert has no 3-button cap, so all four remembered-label
//! outcomes are first-class buttons.

use std::io::{Read, Write};
use std::process::Stdio;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use zeroize::Zeroize;

use crate::prompt::{Decision, Outcome, Prompter, Request};

#[derive(Debug, Serialize, Deserialize)]
pub struct PromptRequest {
    pub label: String,
    pub requester: String,
    pub host: String,
    /// "env" | "stdin" | "askpass"
    pub mode: String,
    pub target: String,
    pub remembered: bool,
    pub touch_id_enroll: bool,
    pub timeout_secs: u64,
}

impl PromptRequest {
    pub fn from_request(req: &Request) -> Self {
        Self {
            label: req.label.clone(),
            requester: req.requester.clone(),
            host: req.host.clone(),
            mode: req.mode.as_str().to_string(),
            target: req.target.clone(),
            remembered: req.remembered,
            touch_id_enroll: req.touch_id_enroll,
            timeout_secs: req.timeout.as_secs(),
        }
    }
}

/// One decision on stdout. `secret` accompanies allow-once/allow-remember
/// for fresh prompts. The helper's stdout is a pipe to the daemon — the
/// secret never touches argv, files, or logs.
#[derive(Debug, Serialize, Deserialize)]
pub struct PromptDecision {
    /// "allow-once" | "allow-remember" | "forget" | "deny" | "timeout" | "unavailable"
    pub outcome: String,
    #[serde(default)]
    pub secret: Option<String>,
}

impl Drop for PromptDecision {
    fn drop(&mut self) {
        if let Some(s) = &mut self.secret {
            s.zeroize();
        }
    }
}

impl PromptDecision {
    pub fn into_decision(mut self) -> Decision {
        let outcome = match self.outcome.as_str() {
            "allow-once" => Outcome::AllowOnce,
            "allow-remember" => Outcome::AllowRemember,
            "forget" => Outcome::Forget,
            "deny" => Outcome::Deny,
            "timeout" => Outcome::Timeout,
            _ => Outcome::Unavailable,
        };
        let secret = self
            .secret
            .take() // taken out before Drop zeroizes the (now empty) field
            .map(String::into_bytes)
            .unwrap_or_default();
        Decision { outcome, secret }
    }
}

/// Prompter over a spawned helper: `<helper_path> _prompt`. The child gets
/// timeout_secs+5 of wall clock before we kill it (the helper enforces its
/// own timeout; the kill is the backstop).
pub struct HelperPrompter {
    pub helper_path: std::path::PathBuf,
}

impl Prompter for HelperPrompter {
    fn prompt(&self, req: &Request) -> Decision {
        match self.run(req) {
            Ok(d) => d,
            Err(err) => {
                tracing::warn!(target: "portal::cred", %err, "prompt helper failed");
                Decision::of(Outcome::Unavailable)
            }
        }
    }
}

impl HelperPrompter {
    fn run(&self, req: &Request) -> Result<Decision, String> {
        let mut child = std::process::Command::new(&self.helper_path)
            .arg("_prompt")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| format!("spawn: {e}"))?;
        let payload =
            serde_json::to_vec(&PromptRequest::from_request(req)).map_err(|e| e.to_string())?;
        {
            let mut stdin = child.stdin.take().ok_or("no stdin")?;
            stdin.write_all(&payload).map_err(|e| e.to_string())?;
            // drop closes stdin — the helper reads to EOF.
        }

        // Backstop kill: helper timeout + 5s grace.
        let deadline = std::time::Instant::now() + req.timeout + Duration::from_secs(5);
        loop {
            match child.try_wait().map_err(|e| e.to_string())? {
                Some(_status) => break,
                None if std::time::Instant::now() >= deadline => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Ok(Decision::of(Outcome::Timeout));
                }
                None => std::thread::sleep(Duration::from_millis(50)),
            }
        }
        let mut out = String::new();
        child
            .stdout
            .take()
            .ok_or("no stdout")?
            .read_to_string(&mut out)
            .map_err(|e| e.to_string())?;
        let decision: PromptDecision =
            serde_json::from_str(out.trim()).map_err(|e| format!("bad decision: {e}"))?;
        Ok(decision.into_decision())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Mode;

    fn req(timeout_secs: u64) -> Request {
        Request {
            label: "staging admin".into(),
            requester: "pid 42: sudo".into(),
            host: "devbox1".into(),
            mode: Mode::Askpass,
            target: "prompt".into(),
            remembered: false,
            touch_id_enroll: true,
            timeout: Duration::from_secs(timeout_secs),
        }
    }

    fn helper_script(dir: &std::path::Path, body: &str) -> std::path::PathBuf {
        let path = dir.join("helper.sh");
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        path
    }

    #[test]
    fn roundtrip_through_a_fake_helper() {
        let dir = tempfile::tempdir().unwrap();
        // Echo the request to /dev/null; answer allow-remember with a secret.
        let path = helper_script(
            dir.path(),
            r#"cat > /dev/null; printf '{"outcome":"allow-remember","secret":"s3kr3t"}'"#,
        );
        let p = HelperPrompter { helper_path: path };
        let d = p.prompt(&req(30));
        assert_eq!(d.outcome, Outcome::AllowRemember);
        assert_eq!(d.secret, b"s3kr3t");
    }

    #[test]
    fn helper_receives_the_request_json() {
        let dir = tempfile::tempdir().unwrap();
        let capture = dir.path().join("seen.json");
        let path = helper_script(
            dir.path(),
            &format!(
                r#"cat > {}; printf '{{"outcome":"deny"}}'"#,
                capture.display()
            ),
        );
        let p = HelperPrompter { helper_path: path };
        let d = p.prompt(&req(30));
        assert_eq!(d.outcome, Outcome::Deny);
        let seen: PromptRequest =
            serde_json::from_str(&std::fs::read_to_string(&capture).unwrap()).unwrap();
        assert_eq!(seen.label, "staging admin");
        assert_eq!(seen.mode, "askpass");
        assert!(seen.touch_id_enroll);
    }

    #[test]
    fn garbage_output_maps_to_unavailable_and_hang_to_timeout() {
        let dir = tempfile::tempdir().unwrap();
        let bad = helper_script(dir.path(), r#"cat > /dev/null; echo not-json"#);
        let p = HelperPrompter { helper_path: bad };
        assert_eq!(p.prompt(&req(30)).outcome, Outcome::Unavailable);

        // A hanging helper is killed at timeout+grace → Timeout. Use a 0s
        // request timeout so the backstop (5s grace) is the bound.
        let hang = helper_script(dir.path(), r#"cat > /dev/null; sleep 60"#);
        let p = HelperPrompter { helper_path: hang };
        assert_eq!(p.prompt(&req(0)).outcome, Outcome::Timeout);
    }
}
