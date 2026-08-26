//! launchd login-agent management: plist rendering plus synchronized
//! bootstrap/bootout/print through the Runner seam, so every launchctl
//! interaction is unit-testable.

use std::path::Path;
use std::time::{Duration, Instant};

use portal_transport::runner::{RunOutput, Runner};

/// launchd transitions are asynchronous. Correctness is driven by querying
/// the registry state, never by assuming an arbitrary sleep was long enough.
/// The deadline only bounds a broken launchd interaction; each condition
/// probe is a launchctl subprocess, which naturally yields to launchd.
const TRANSITION_TIMEOUT: Duration = Duration::from_secs(8);
/// Rate-limit launchctl condition probes. This is not a readiness delay: every
/// transition still completes on observed state, while a stuck transition no
/// longer forks thousands of launchctl processes per second.
const PROBE_INTERVAL: Duration = Duration::from_millis(50);

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

/// Render the tray (menu bar status item) LaunchAgent plist. Differs from
/// the daemon's on exactly the axes a UI agent needs:
/// - `LimitLoadToSessionType=Aqua`: a status item only exists in a GUI login
///   session — launchd must not spawn it for ssh-only sessions;
/// - `KeepAlive={SuccessfulExit:false}`: a crash relaunches it, but the
///   user's deliberate Quit (exit 0) STAYS quit until next login/upgrade —
///   KeepAlive=true would make Quit a lie;
/// - `ProcessType=Interactive`: it services a click, not a batch queue.
pub fn render_tray_plist(
    label: &str,
    bin_path: &Path,
    mode_argument: &str,
    home: &Path,
    log: &Path,
) -> String {
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{label}</string>

    <key>ProgramArguments</key>
    <array>
        <string>{bin}</string>
        <string>{mode_argument}</string>
    </array>

    <key>RunAtLoad</key>
    <true/>

    <!-- Crash ⇒ relaunch; deliberate Quit (exit 0) stays quit. -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <key>ThrottleInterval</key>
    <integer>10</integer>

    <!-- Status items exist only in GUI login sessions. -->
    <key>LimitLoadToSessionType</key>
    <string>Aqua</string>

    <key>ProcessType</key>
    <string>Interactive</string>

    <key>EnvironmentVariables</key>
    <dict>
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
        bin = bin_path.display(),
        mode_argument = mode_argument,
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

    async fn launchctl_output(&self, args: &[String]) -> std::io::Result<RunOutput> {
        self.runner.run("launchctl", args, b"").await
    }

    fn command_error(verb: &str, out: &RunOutput) -> std::io::Error {
        let detail = out.stderr_lossy().trim().to_string();
        let detail = if detail.is_empty() {
            format!("exit {}", out.code)
        } else {
            format!("exit {}: {detail}", out.code)
        };
        std::io::Error::other(format!("launchctl {verb} failed ({detail})"))
    }

    async fn print(&self) -> std::io::Result<RunOutput> {
        self.launchctl_output(&["print".into(), self.domain_label()])
            .await
    }

    /// Is the agent registered? (`launchctl print` exit 0)
    pub async fn is_loaded(&self) -> std::io::Result<bool> {
        Ok(self.print().await?.code == 0)
    }

    /// Wait for launchd's registry—not elapsed wall-clock guesswork—to say
    /// bootout is complete. This closes the documented bootout/bootstrap race
    /// that otherwise surfaces as bootstrap exit 5 (EIO).
    async fn wait_until_unloaded(&self) -> std::io::Result<()> {
        let deadline = Instant::now() + TRANSITION_TIMEOUT;
        loop {
            if !self.is_loaded().await? {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    format!("launchd did not unregister {}", self.domain_label()),
                ));
            }
            tokio::time::sleep(PROBE_INTERVAL).await;
        }
    }

    /// Register an unloaded job from its plist.
    pub async fn bootstrap(&self, plist: &Path) -> std::io::Result<()> {
        let out = self
            .launchctl_output(&[
                "bootstrap".into(),
                self.domain.clone(),
                plist.display().to_string(),
            ])
            .await?;
        if out.code != 0 {
            return Err(Self::command_error("bootstrap", &out));
        }
        Ok(())
    }

    /// Freshly register the plist. Unlike kickstart, this refreshes launchd's
    /// Lightweight Code Requirement after an executable replacement.
    pub async fn load(&self, plist: &Path) -> std::io::Result<()> {
        self.unload().await?;
        self.bootstrap(plist).await
    }

    /// Unregister the job and do not return until launchd confirms the label
    /// is gone. `Ok(false)` means it was already absent.
    pub async fn unload(&self) -> std::io::Result<bool> {
        if !self.is_loaded().await? {
            return Ok(false);
        }
        let out = self
            .launchctl_output(&["bootout".into(), self.domain_label()])
            .await?;
        if out.code != 0 {
            return Err(Self::command_error("bootout", &out));
        }
        self.wait_until_unloaded().await?;
        Ok(true)
    }

    /// Wait until the job's top-level launchd state reaches `running`.
    pub async fn wait_until_running(&self) -> std::io::Result<()> {
        let deadline = Instant::now() + TRANSITION_TIMEOUT;
        loop {
            let out = self.print().await?;
            let state = if out.code == 0 {
                out.stdout_lossy()
                    .lines()
                    .map(str::trim)
                    .find(|line| line.starts_with("state ="))
                    .unwrap_or("state unavailable")
                    .to_string()
            } else {
                "not registered".to_string()
            };
            if state == "state = running" {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    format!(
                        "{} did not reach running state ({state})",
                        self.domain_label()
                    ),
                ));
            }
            tokio::time::sleep(PROBE_INTERVAL).await;
        }
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
            // Nested resource/jetsam coalitions have their own `state` lines;
            // only the four top-level job fields belong in user-facing status.
            .take(4)
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

    /// The tray plist's load-bearing differences from the daemon's: Aqua-only
    /// (status items need a GUI session), quit-stays-quit KeepAlive, and the
    /// `tray` verb.
    #[test]
    fn tray_plist_is_aqua_only_with_quit_stays_quit() {
        let p = render_tray_plist(
            "local.portal.tray",
            &PathBuf::from("/Users/u/.local/bin/portal"),
            "tray",
            &PathBuf::from("/Users/u"),
            &PathBuf::from("/Users/u/Library/Logs/portal-tray.log"),
        );
        for needle in [
            "<string>local.portal.tray</string>",
            "<string>tray</string>",
            "<key>LimitLoadToSessionType</key>",
            "<string>Aqua</string>",
            "<key>SuccessfulExit</key>",
            "<false/>",
            "<string>Interactive</string>",
            "/Users/u/Library/Logs/portal-tray.log",
        ] {
            assert!(p.contains(needle), "missing {needle}");
        }
        assert!(
            !p.contains("<key>KeepAlive</key>\n    <true/>"),
            "KeepAlive=true would relaunch a deliberately quit tray"
        );
    }

    #[tokio::test]
    async fn load_waits_for_registry_removal_before_bootstrap() {
        let fake = FakeRunner::new();
        fake.push_str("state = running", "", 0); // initial print → loaded
        fake.push_str("", "", 0); // bootout accepted
        fake.push_str("state = exited", "", 0); // teardown still registered
        fake.push_str("", "not found", 113); // registry removal complete
        fake.push_str("", "", 0); // bootstrap
        let l = Launchd::new(&fake, 501, "local.portal.autoforward");
        l.load(&PathBuf::from("/tmp/x.plist")).await.unwrap();
        let calls = fake.calls();
        assert_eq!(calls[0].1[0], "print");
        assert_eq!(calls[1].1[0], "bootout");
        assert_eq!(calls[2].1[0], "print");
        assert_eq!(calls[3].1[0], "print");
        assert_eq!(calls[4].1, vec!["bootstrap", "gui/501", "/tmp/x.plist"]);
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
    async fn unload_propagates_bootout_failure() {
        let fake = FakeRunner::new();
        fake.push_str("state = running", "", 0);
        fake.push_str("", "permission denied", 1);
        let l = Launchd::new(&fake, 501, "local.portal.autoforward");
        let err = l.unload().await.unwrap_err().to_string();
        assert!(err.contains("permission denied"), "{err}");
    }

    #[tokio::test]
    async fn wait_until_running_observes_launchd_state() {
        let fake = FakeRunner::new();
        fake.push_str("state = xpcproxy", "", 0);
        fake.push_str("state = running", "", 0);
        let l = Launchd::new(&fake, 501, "local.portal.autoforward");
        l.wait_until_running().await.unwrap();
        assert_eq!(fake.calls().len(), 2);
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
