//! Coordinated lifecycle for portal's two LaunchAgents.
//!
//! The daemon and menu-bar process execute the same on-disk binary. Replacing
//! that file while either job remains registered can violate launchd's cached
//! Lightweight Code Requirement and produces `EX_CONFIG` / `SIGKILL (Code
//! Signature Invalid)`. Deployment therefore has one mandatory order:
//!
//! 1. write manifests;
//! 2. unregister *both* jobs and observe registry removal;
//! 3. atomically swap the executable;
//! 4. freshly bootstrap both jobs;
//! 5. prove the daemon API is healthy before committing the swap.

use std::fs;
use std::io::Write as _;
use std::os::unix::fs::PermissionsExt as _;
use std::path::Path;
use std::time::{Duration, Instant};

use portal_core::paths::Paths;
use portal_transport::runner::Runner;
use tempfile::Builder;
use tokio::io::AsyncReadExt as _;

use crate::launchd::{self, Launchd};

const HEALTH_TIMEOUT: Duration = Duration::from_secs(10);
const HEALTH_PROBE_INTERVAL: Duration = Duration::from_millis(100);

#[derive(Debug, Default)]
pub struct StartReport {
    /// The forwarding daemon is healthy even when an Aqua-only tray cannot be
    /// started (for example, during an SSH-only login session).
    pub tray_warning: Option<String>,
}

pub struct LoginAgents<'a> {
    runner: &'a dyn Runner,
    paths: &'a Paths,
}

impl<'a> LoginAgents<'a> {
    pub fn new(runner: &'a dyn Runner, paths: &'a Paths) -> Self {
        Self { runner, paths }
    }

    fn daemon(&self) -> Launchd<'_> {
        Launchd::new(self.runner, self.paths.uid, self.paths.label.clone())
    }

    fn tray(&self) -> Launchd<'_> {
        Launchd::new(self.runner, self.paths.uid, self.paths.tray_label.clone())
    }

    /// Atomically converge both plists before lifecycle changes begin.
    pub fn write_manifests(&self) -> Result<(), String> {
        let daemon = launchd::render_plist(
            &self.paths.label,
            &self.paths.bin_path,
            &["daemon"],
            &self.paths.home,
            &self.paths.log,
        );
        let tray = launchd::render_tray_plist(
            &self.paths.tray_label,
            &self.paths.bin_path,
            &self.paths.home,
            &self.paths.tray_log,
        );
        write_if_changed(&self.paths.plist, daemon.as_bytes())?;
        write_if_changed(&self.paths.tray_plist, tray.as_bytes())
    }

    /// Stop and fully unregister both jobs. The tray goes first so it never
    /// displays a transient daemon failure while an explicit deployment is in
    /// progress. No binary may be replaced until this succeeds.
    pub async fn quiesce(&self) -> Result<(), String> {
        self.tray()
            .unload()
            .await
            .map_err(|e| format!("stop menu bar agent: {e}"))?;
        self.daemon()
            .unload()
            .await
            .map_err(|e| format!("stop daemon: {e}"))?;
        // A clean daemon removes this itself. Removing a stale pathname after
        // launchd confirms process teardown prevents a false health success.
        match fs::remove_file(&self.paths.api_sock) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(format!("remove stale api socket: {e}")),
        }
        Ok(())
    }

    /// Freshly register the daemon and prove it can answer its status API.
    pub async fn start_daemon_fresh(&self) -> Result<(), String> {
        let daemon = self.daemon();
        daemon
            .load(&self.paths.plist)
            .await
            .map_err(|e| format!("start daemon: {e}"))?;
        daemon
            .wait_until_running()
            .await
            .map_err(|e| format!("start daemon: {e}"))?;
        self.wait_for_daemon_api(&daemon).await
    }

    /// Freshly register both jobs and prove the daemon can answer its status
    /// API. Tray failure is reported separately because forwarding remains
    /// healthy in a non-Aqua session.
    pub async fn start_fresh(&self) -> Result<StartReport, String> {
        self.start_daemon_fresh().await?;

        let tray = self.tray();
        let tray_warning = match tray.load(&self.paths.tray_plist).await {
            Ok(()) => match tray.wait_until_running().await {
                Ok(()) => None,
                Err(e) => {
                    // Do not leave a failed UI job crash-looping. Forwarding
                    // remains healthy and a later install/upgrade retries it.
                    let _ = tray.unload().await;
                    Some(format!("menu bar agent: {e}"))
                }
            },
            Err(e) => Some(format!("menu bar agent: {e}")),
        };

        Ok(StartReport { tray_warning })
    }

    async fn wait_for_daemon_api(&self, daemon: &Launchd<'_>) -> Result<(), String> {
        let deadline = Instant::now() + HEALTH_TIMEOUT;
        loop {
            let last_error = match tokio::net::UnixStream::connect(&self.paths.api_sock).await {
                Ok(mut stream) => {
                    let mut bytes = Vec::new();
                    match tokio::time::timeout(
                        Duration::from_secs(1),
                        stream.read_to_end(&mut bytes),
                    )
                    .await
                    {
                        Ok(Ok(_)) => match serde_json::from_slice::<serde_json::Value>(&bytes) {
                            Ok(value) if value.is_array() => return Ok(()),
                            Ok(_) => "status response was not an array".to_string(),
                            Err(e) => format!("invalid status response: {e}"),
                        },
                        Ok(Err(e)) => format!("read status socket: {e}"),
                        Err(_) => "status socket read timed out".to_string(),
                    }
                }
                Err(e) => e.to_string(),
            };

            if Instant::now() >= deadline {
                return Err(format!(
                    "daemon did not become healthy at {} ({last_error})",
                    self.paths.api_sock.display()
                ));
            }
            // Detect an early process exit, then rate-limit the next probe.
            // Completion remains condition-driven; this is not a fixed
            // post-launch readiness delay.
            if !daemon
                .is_loaded()
                .await
                .map_err(|e| format!("query daemon during startup: {e}"))?
            {
                return Err("daemon became unregistered during startup".to_string());
            }
            tokio::time::sleep(HEALTH_PROBE_INTERVAL).await;
        }
    }
}

fn write_if_changed(path: &Path, contents: &[u8]) -> Result<(), String> {
    if fs::read(path).ok().as_deref() == Some(contents) {
        return Ok(());
    }
    let parent = path
        .parent()
        .ok_or_else(|| format!("{} has no parent", path.display()))?;
    fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
    let mut staged = Builder::new()
        .prefix(".portal.plist.")
        .tempfile_in(parent)
        .map_err(|e| format!("stage {}: {e}", path.display()))?;
    staged
        .write_all(contents)
        .map_err(|e| format!("write {}: {e}", path.display()))?;
    staged
        .as_file()
        .set_permissions(fs::Permissions::from_mode(0o644))
        .map_err(|e| format!("chmod {}: {e}", path.display()))?;
    staged
        .as_file()
        .sync_all()
        .map_err(|e| format!("sync {}: {e}", path.display()))?;
    staged
        .persist(path)
        .map_err(|e| format!("install {}: {}", path.display(), e.error))?;
    FileSync::directory(parent).map_err(|e| format!("sync {}: {e}", parent.display()))
}

struct FileSync;

impl FileSync {
    fn directory(path: &Path) -> std::io::Result<()> {
        fs::File::open(path)?.sync_all()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn manifest_write_is_atomic_and_idempotent() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("agent.plist");
        write_if_changed(&path, b"first").unwrap();
        assert_eq!(fs::read(&path).unwrap(), b"first");
        write_if_changed(&path, b"first").unwrap();
        write_if_changed(&path, b"second").unwrap();
        assert_eq!(fs::read(&path).unwrap(), b"second");
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o644
        );
    }
}
