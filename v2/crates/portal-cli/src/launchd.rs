//! launchd login-agent management (port of internal/service): plist render +
//! bootstrap/bootout/kickstart/print through the Runner seam so every
//! launchctl interaction is unit-testable.

use std::path::Path;

use portal_transport::runner::Runner;

/// Render the LaunchAgent plist (v1 template, verbatim semantics):
/// RunAtLoad + KeepAlive (any exit relaunches), ThrottleInterval 30 to damp
/// crash loops, Background process type, PATH pinned (launchd PATH search is
/// unreliable), stdout+stderr into the portal log.
pub fn render_plist(
    label: &str,
    bin_path: &Path,
    args: &[&str],
    home: &Path,
    log: &Path,
) -> String {
    let mut program_args = format!("        <string>{}</string>", bin_path.display());
    for a in args {
        program_args.push_str(&format!("\n        <string>{a}</string>"));
    }
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{label}</string>

    <key>ProgramArguments</key>
    <array>
{program_args}
    </array>

    <key>RunAtLoad</key>
    <true/>

    <!-- Daemon loops forever; ANY exit means relaunch. -->
    <key>KeepAlive</key>
    <true/>

    <!-- Floor on relaunch frequency to dampen crash-loops. -->
    <key>ThrottleInterval</key>
    <integer>30</integer>

    <key>ProcessType</key>
    <string>Background</string>

    <key>LowPriorityIO</key>
    <true/>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>HOME</key>
        <string>{home}</string>
    </dict>

    <key>StandardOutPath</key>
    <string>{log}</string>
    <key>StandardErrorPath</key>
    <string>{log}</string>
</dict>
</plist>
"#,
        home = home.display(),
        log = log.display(),
    )
}

pub struct Launchd<'a> {
    pub runner: &'a dyn Runner,
    /// gui/<uid>
    pub domain: String,
    pub label: String,
}

impl<'a> Launchd<'a> {
    pub fn new(runner: &'a dyn Runner, uid: u32, label: impl Into<String>) -> Self {
        Self {
            runner,
            domain: format!("gui/{uid}"),
            label: label.into(),
        }
    }

    fn domain_label(&self) -> String {
        format!("{}/{}", self.domain, self.label)
    }

    async fn launchctl(&self, args: &[String]) -> std::io::Result<i32> {
        Ok(self.runner.run("launchctl", args, b"").await?.code)
    }

    /// Is the agent loaded? (`launchctl print` exit 0)
    pub async fn is_loaded(&self) -> std::io::Result<bool> {
        Ok(self
            .launchctl(&["print".into(), self.domain_label()])
            .await?
            == 0)
    }

    /// Load the plist (idempotent: an already-loaded agent is bootout'd
    /// first so plist changes apply — v1 `reload` semantics).
    pub async fn load(&self, plist: &Path) -> std::io::Result<()> {
        if self.is_loaded().await? {
            let _ = self
                .launchctl(&["bootout".into(), self.domain_label()])
                .await;
        }
        let code = self
            .launchctl(&[
                "bootstrap".into(),
                self.domain.clone(),
                plist.display().to_string(),
            ])
            .await?;
        if code != 0 {
            return Err(std::io::Error::other(format!(
                "launchctl bootstrap failed (exit {code})"
            )));
        }
        Ok(())
    }

    pub async fn unload(&self) -> std::io::Result<bool> {
        if !self.is_loaded().await? {
            return Ok(false);
        }
        let code = self
            .launchctl(&["bootout".into(), self.domain_label()])
            .await?;
        Ok(code == 0)
    }

    /// Restart the (loaded) agent now.
    pub async fn kickstart(&self) -> std::io::Result<()> {
        let code = self
            .launchctl(&["kickstart".into(), "-k".into(), self.domain_label()])
            .await?;
        if code != 0 {
            return Err(std::io::Error::other(format!(
                "launchctl kickstart failed (exit {code})"
            )));
        }
        Ok(())
    }

    /// The state/pid/last-exit lines from `launchctl print` (status display).
    pub async fn status_lines(&self) -> std::io::Result<Vec<String>> {
        let out = self
            .runner
            .run("launchctl", &["print".into(), self.domain_label()], b"")
            .await?;
        if out.code != 0 {
            return Ok(Vec::new());
        }
        Ok(out
            .stdout_lossy()
            .lines()
            .map(|l| l.trim())
            .filter(|l| {
                l.starts_with("state =")
                    || l.starts_with("pid =")
                    || l.starts_with("runs =")
                    || l.starts_with("last exit code =")
            })
            .map(|l| format!("  {l}"))
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_transport::runner::FakeRunner;
    use std::path::PathBuf;

    #[test]
    fn plist_renders_the_v1_shape() {
        let p = render_plist(
            "local.portal.autoforward",
            &PathBuf::from("/Users/u/.local/bin/portal"),
            &["daemon"],
            &PathBuf::from("/Users/u"),
            &PathBuf::from("/Users/u/Library/Logs/portal.log"),
        );
        for needle in [
            "<string>local.portal.autoforward</string>",
            "<string>/Users/u/.local/bin/portal</string>",
            "<string>daemon</string>",
            "<key>KeepAlive</key>",
            "<integer>30</integer>",
            "<string>Background</string>",
            "/Users/u/Library/Logs/portal.log",
        ] {
            assert!(p.contains(needle), "missing {needle}");
        }
    }

    #[tokio::test]
    async fn load_boots_out_first_when_loaded() {
        let fake = FakeRunner::new();
        fake.push_str("state = running", "", 0); // print → loaded
        fake.push_str("", "", 0); // bootout
        fake.push_str("", "", 0); // bootstrap
        let l = Launchd::new(&fake, 501, "local.portal.autoforward");
        l.load(&PathBuf::from("/tmp/x.plist")).await.unwrap();
        let calls = fake.calls();
        assert_eq!(calls[0].1[0], "print");
        assert_eq!(calls[1].1[0], "bootout");
        assert_eq!(calls[2].1, vec!["bootstrap", "gui/501", "/tmp/x.plist"]);
    }

    #[tokio::test]
    async fn load_skips_bootout_when_not_loaded() {
        let fake = FakeRunner::new();
        fake.push_str("", "not found", 113); // print → not loaded
        fake.push_str("", "", 0); // bootstrap
        let l = Launchd::new(&fake, 501, "local.portal.autoforward");
        l.load(&PathBuf::from("/tmp/x.plist")).await.unwrap();
        assert_eq!(fake.calls()[1].1[0], "bootstrap");
    }

    #[tokio::test]
    async fn status_lines_filters_the_four_fields() {
        let fake = FakeRunner::new();
        fake.push_str(
            "system stuff\n\tstate = running\n\tpid = 99\n\truns = 3\n\tlast exit code = 0\n\tother = x\n",
            "",
            0,
        );
        let l = Launchd::new(&fake, 501, "local.portal.autoforward");
        let lines = l.status_lines().await.unwrap();
        assert_eq!(lines.len(), 4);
        assert_eq!(lines[0], "  state = running");
    }
}
