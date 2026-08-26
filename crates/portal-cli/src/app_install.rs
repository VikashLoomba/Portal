//! Transactional installation of a verified Portal.app bundle.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

use portal_core::paths::Paths;
use portal_transport::runner::OsRunner;

use crate::deployment::Deployment;
use crate::services::{LoginAgents, StartReport};

const BUNDLE_ID: &str = "com.vikashloomba.portal";
const APP_EXECUTABLE: &str = "Contents/MacOS/Portal";
const APP_CLI_LAUNCHER: &str = "Contents/Resources/bin/portal";

pub(crate) fn app_executable(app: &Path) -> PathBuf {
    app.join(APP_EXECUTABLE)
}

pub(crate) fn app_cli_launcher(app: &Path) -> PathBuf {
    app.join(APP_CLI_LAUNCHER)
}

pub fn app_from_executable(executable: &Path) -> Option<PathBuf> {
    executable
        .ancestors()
        .find(|path| path.extension().is_some_and(|extension| extension == "app"))
        .map(Path::to_path_buf)
}

pub fn installed_app(paths: &Paths) -> Option<PathBuf> {
    let has_runtime = |app: &Path| app_executable(app).is_file() && app_cli_launcher(app).is_file();
    if let Ok(current) = std::env::current_exe()
        && let Some(app) = app_from_executable(&current)
        && has_runtime(&app)
    {
        return Some(app);
    }
    if let Ok(target) = std::fs::read_link(&paths.bin_path)
        && let Some(app) = app_from_executable(&target)
        && has_runtime(&app)
    {
        return Some(app);
    }
    let system = PathBuf::from("/Applications/Portal.app");
    if has_runtime(&system) {
        return Some(system);
    }
    let user = paths.home.join("Applications/Portal.app");
    has_runtime(&user).then_some(user)
}

pub fn app_is_needed(paths: &Paths) -> bool {
    installed_app(paths).is_none()
}

pub fn install_verified_app(
    candidate: &Path,
    paths: &Paths,
    expected_tag: &str,
) -> Result<StartReport, String> {
    let _deployment = Deployment::acquire(&paths.bin_path)?;
    let destination = destination(paths)?;
    let parent = destination
        .parent()
        .ok_or_else(|| format!("{} has no parent", destination.display()))?;
    std::fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
    cleanup_abandoned_app_staging(parent);

    // Copy into the destination filesystem and re-run platform verification
    // before stopping a single live process.
    let staged_dir = tempfile::Builder::new()
        .prefix(".portal-app-stage.")
        .tempdir_in(parent)
        .map_err(|e| format!("stage Portal.app in {}: {e}", parent.display()))?;
    let staged_app = staged_dir.path().join("Portal.app");
    run(
        "ditto",
        &[candidate.as_os_str(), staged_app.as_os_str()],
        "copy verified Portal.app",
    )?;
    verify_app(&staged_app, expected_tag)?;

    let pid = process_id();
    std::fs::create_dir_all(&paths.bin_dir).map_err(|e| e.to_string())?;
    cleanup_abandoned_cli_links(&paths.bin_dir);
    let staged_cli = paths.bin_dir.join(format!(".portal.app-link.{pid}"));
    if std::fs::symlink_metadata(&staged_cli).is_ok() {
        return Err(format!(
            "stale CLI staging path at {}",
            staged_cli.display()
        ));
    }
    std::os::unix::fs::symlink(app_cli_launcher(&destination), &staged_cli)
        .map_err(|e| format!("stage CLI link: {e}"))?;

    let runner = OsRunner;
    let agents = LoginAgents::new(&runner, paths);
    let runtime = tokio::runtime::Runtime::new().map_err(|e| e.to_string())?;
    if let Err(error) = runtime.block_on(agents.quiesce()) {
        let _ = std::fs::remove_file(&staged_cli);
        return Err(error);
    }
    terminate_running_app_instances();

    let had_app = destination.exists();
    let had_cli = std::fs::symlink_metadata(&paths.bin_path).is_ok();
    let mut state = RollbackState {
        app_backup: staged_app.clone(),
        cli_backup: staged_cli.clone(),
        app_backed_up: false,
        cli_backed_up: false,
        app_installed: false,
        cli_installed: false,
    };
    let install_result = (|| {
        if had_app {
            atomic_swap(&staged_app, &destination)
                .map_err(|e| format!("atomically replace {}: {e}", destination.display()))?;
            state.app_backed_up = true;
            state.app_installed = true;
        } else {
            std::fs::rename(&staged_app, &destination)
                .map_err(|e| format!("install {}: {e}", destination.display()))?;
            state.app_installed = true;
        }
        if had_cli {
            atomic_swap(&staged_cli, &paths.bin_path)
                .map_err(|e| format!("atomically replace {}: {e}", paths.bin_path.display()))?;
            state.cli_backed_up = true;
            state.cli_installed = true;
        } else {
            std::fs::rename(&staged_cli, &paths.bin_path)
                .map_err(|e| format!("install CLI link: {e}"))?;
            state.cli_installed = true;
        }
        if std::env::var_os("PORTAL_INSTALL_FAULT").as_deref()
            == Some(std::ffi::OsStr::new("after-cli-swap"))
        {
            return Err("injected failure after app and CLI swap".into());
        }
        agents.write_manifests()?;
        runtime.block_on(agents.start_fresh())
    })();

    let report = match install_result {
        Ok(report) => report,
        Err(error) => {
            // Set this before restarting the previous daemon so its release
            // bridge cannot enter an automatic retry loop on a persistent
            // filesystem/Gatekeeper failure. A manual `portal upgrade` clears
            // the marker and retries visibly.
            let _ = std::fs::create_dir_all(&paths.config_dir);
            let _ = std::fs::write(
                paths.config_dir.join("app-migration.failed"),
                format!("{error}\n"),
            );
            let _ = runtime.block_on(agents.quiesce());
            let rollback = rollback(paths, &destination, &state);
            let recovery = rollback
                .and_then(|()| agents.write_manifests())
                .and_then(|()| runtime.block_on(agents.start_fresh()).map(|_| ()));
            return Err(match recovery {
                Ok(()) => format!("{error}; previous installation restored"),
                Err(recovery) => {
                    format!("{error}; restoring previous installation failed: {recovery}")
                }
            });
        }
    };

    if state.app_backed_up
        && let Err(error) = std::fs::remove_dir_all(&state.app_backup)
    {
        tracing::warn!(%error, path = %state.app_backup.display(), "could not remove app rollback backup");
    }
    if state.cli_backed_up
        && let Err(error) = std::fs::remove_file(&state.cli_backup)
    {
        tracing::warn!(%error, path = %state.cli_backup.display(), "could not remove CLI rollback backup");
    }
    let _ = std::fs::remove_file(paths.config_dir.join("app-migration.failed"));
    if let Err(error) = sync_directory(parent) {
        tracing::warn!(%error, "could not sync app installation directory");
    }
    if let Err(error) = sync_directory(&paths.bin_dir) {
        tracing::warn!(%error, "could not sync CLI installation directory");
    }
    Ok(report)
}

fn cleanup_abandoned_app_staging(parent: &Path) {
    let Ok(entries) = std::fs::read_dir(parent) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        if name.to_string_lossy().starts_with(".portal-app-stage.")
            && entry.file_type().is_ok_and(|kind| kind.is_dir())
            && let Err(error) = std::fs::remove_dir_all(entry.path())
        {
            tracing::warn!(%error, path = %entry.path().display(), "could not remove abandoned app staging directory");
        }
    }
}

fn cleanup_abandoned_cli_links(bin_dir: &Path) {
    let Ok(entries) = std::fs::read_dir(bin_dir) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        if name.to_string_lossy().starts_with(".portal.app-link.")
            && std::fs::symlink_metadata(entry.path())
                .is_ok_and(|meta| meta.file_type().is_symlink())
            && let Err(error) = std::fs::remove_file(entry.path())
        {
            tracing::warn!(%error, path = %entry.path().display(), "could not remove abandoned CLI staging link");
        }
    }
}

fn destination(paths: &Paths) -> Result<PathBuf, String> {
    if let Some(path) = std::env::var_os("PORTAL_APP_PATH") {
        return Ok(PathBuf::from(path));
    }
    if let Some(existing) = installed_app(paths) {
        return Ok(existing);
    }
    let system = Path::new("/Applications");
    if tempfile::Builder::new()
        .prefix(".portal-write-test.")
        .tempfile_in(system)
        .is_ok()
    {
        return Ok(system.join("Portal.app"));
    }
    let user = paths.home.join("Applications");
    std::fs::create_dir_all(&user).map_err(|e| format!("create {}: {e}", user.display()))?;
    Ok(user.join("Portal.app"))
}

struct RollbackState {
    app_backup: PathBuf,
    cli_backup: PathBuf,
    app_backed_up: bool,
    cli_backed_up: bool,
    app_installed: bool,
    cli_installed: bool,
}

fn rollback(paths: &Paths, destination: &Path, state: &RollbackState) -> Result<(), String> {
    if state.cli_backed_up {
        atomic_swap(&state.cli_backup, &paths.bin_path).map_err(|e| e.to_string())?;
        let _ = std::fs::remove_file(&state.cli_backup);
    } else if state.cli_installed && std::fs::symlink_metadata(&paths.bin_path).is_ok() {
        std::fs::remove_file(&paths.bin_path).map_err(|e| e.to_string())?;
    }
    if std::fs::symlink_metadata(&state.cli_backup).is_ok() {
        let _ = std::fs::remove_file(&state.cli_backup);
    }
    if state.app_backed_up {
        atomic_swap(&state.app_backup, destination).map_err(|e| e.to_string())?;
        let _ = std::fs::remove_dir_all(&state.app_backup);
    } else if state.app_installed && destination.exists() {
        std::fs::remove_dir_all(destination).map_err(|e| e.to_string())?;
    }
    Ok(())
}

fn verify_app(app: &Path, expected_tag: &str) -> Result<(), String> {
    run(
        "codesign",
        &[
            std::ffi::OsStr::new("--verify"),
            std::ffi::OsStr::new("--deep"),
            std::ffi::OsStr::new("--strict"),
            app.as_os_str(),
        ],
        "verify installed app signature",
    )?;
    run(
        "spctl",
        &[
            std::ffi::OsStr::new("--assess"),
            std::ffi::OsStr::new("--type"),
            std::ffi::OsStr::new("execute"),
            app.as_os_str(),
        ],
        "assess installed app with Gatekeeper",
    )?;
    let binary = app_executable(app);
    let output = Command::new(&binary)
        .args(["--cli", "--version"])
        .output()
        .map_err(|e| format!("execute {}: {e}", binary.display()))?;
    let reported = String::from_utf8_lossy(&output.stdout);
    if output.status.success() && reported.contains(expected_tag.trim_start_matches('v')) {
        Ok(())
    } else {
        Err(format!(
            "installed app reports {:?}, expected {expected_tag}",
            reported.trim()
        ))
    }
}

fn run(program: &str, args: &[&std::ffi::OsStr], label: &str) -> Result<(), String> {
    let output = Command::new(program)
        .args(args)
        .output()
        .map_err(|e| format!("{label}: {e}"))?;
    if output.status.success() {
        Ok(())
    } else {
        Err(format!(
            "{label}: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        ))
    }
}

#[cfg(target_os = "macos")]
fn terminate_running_app_instances() {
    use objc2_app_kit::NSRunningApplication;
    use objc2_foundation::NSString;

    let current = process_id();
    let applications = NSRunningApplication::runningApplicationsWithBundleIdentifier(
        &NSString::from_str(BUNDLE_ID),
    );
    let targets = applications
        .iter()
        .filter(|application| application.processIdentifier() != current)
        .collect::<Vec<_>>();
    for application in &targets {
        application.terminate();
    }
    let deadline = Instant::now() + Duration::from_secs(3);
    while Instant::now() < deadline
        && targets
            .iter()
            .any(|application| !application.isTerminated())
    {
        std::thread::sleep(Duration::from_millis(50));
    }
    for application in targets
        .iter()
        .filter(|application| !application.isTerminated())
    {
        application.forceTerminate();
    }
}

#[cfg(not(target_os = "macos"))]
fn terminate_running_app_instances() {}

#[cfg(target_os = "macos")]
fn atomic_swap(first: &Path, second: &Path) -> std::io::Result<()> {
    use std::os::unix::ffi::OsStrExt as _;

    const RENAME_SWAP: u32 = 0x0000_0002;
    unsafe extern "C" {
        fn renameatx_np(
            from_fd: i32,
            from: *const std::ffi::c_char,
            to_fd: i32,
            to: *const std::ffi::c_char,
            flags: u32,
        ) -> i32;
    }
    let first = std::ffi::CString::new(first.as_os_str().as_bytes())
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "NUL in path"))?;
    let second = std::ffi::CString::new(second.as_os_str().as_bytes())
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "NUL in path"))?;
    // AT_FDCWD on Darwin.
    let result = unsafe { renameatx_np(-2, first.as_ptr(), -2, second.as_ptr(), RENAME_SWAP) };
    if result == 0 {
        Ok(())
    } else {
        Err(std::io::Error::last_os_error())
    }
}

#[cfg(not(target_os = "macos"))]
fn atomic_swap(_first: &Path, _second: &Path) -> std::io::Result<()> {
    Err(std::io::Error::new(
        std::io::ErrorKind::Unsupported,
        "Portal.app installation requires macOS",
    ))
}

fn process_id() -> i32 {
    unsafe extern "C" {
        fn getpid() -> i32;
    }
    unsafe { getpid() }
}

fn sync_directory(path: &Path) -> Result<(), String> {
    std::fs::File::open(path)
        .and_then(|directory| directory.sync_all())
        .map_err(|e| format!("sync {}: {e}", path.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rollback_restores_app_and_cli() {
        let dir = tempfile::tempdir().unwrap();
        let paths = Paths::derive(dir.path(), 501);
        std::fs::create_dir_all(&paths.bin_dir).unwrap();
        let destination = dir.path().join("Applications/Portal.app");
        std::fs::create_dir_all(destination.parent().unwrap()).unwrap();
        std::fs::create_dir_all(&destination).unwrap();
        std::fs::write(destination.join("new"), b"new").unwrap();
        std::fs::write(&paths.bin_path, b"new-cli").unwrap();
        let app_backup = dir.path().join("Applications/.Portal.rollback.app");
        std::fs::create_dir_all(&app_backup).unwrap();
        std::fs::write(app_backup.join("old"), b"old").unwrap();
        let cli_backup = paths.bin_dir.join(".portal.rollback");
        std::fs::write(&cli_backup, b"old-cli").unwrap();
        let state = RollbackState {
            app_backup,
            cli_backup,
            app_backed_up: true,
            cli_backed_up: true,
            app_installed: true,
            cli_installed: true,
        };
        rollback(&paths, &destination, &state).unwrap();
        assert_eq!(std::fs::read(destination.join("old")).unwrap(), b"old");
        assert_eq!(std::fs::read(paths.bin_path).unwrap(), b"old-cli");
    }

    #[test]
    fn abandoned_v2_0_16_staging_is_cleaned_safely() {
        let dir = tempfile::tempdir().unwrap();
        let app_stage = dir.path().join(".portal-app-stage.abandoned");
        let unrelated_dir = dir.path().join("keep");
        std::fs::create_dir(&app_stage).unwrap();
        std::fs::create_dir(&unrelated_dir).unwrap();
        cleanup_abandoned_app_staging(dir.path());
        assert!(!app_stage.exists());
        assert!(unrelated_dir.exists());

        let stale_link = dir.path().join(".portal.app-link.10837");
        let lookalike_file = dir.path().join(".portal.app-link.not-a-link");
        std::os::unix::fs::symlink(
            "/Applications/Portal.app/Contents/Resources/bin/portal",
            &stale_link,
        )
        .unwrap();
        std::fs::write(&lookalike_file, b"keep").unwrap();
        cleanup_abandoned_cli_links(dir.path());
        assert!(std::fs::symlink_metadata(&stale_link).is_err());
        assert!(lookalike_file.exists());
    }

    #[test]
    fn finds_bundle_ancestor() {
        assert_eq!(
            app_from_executable(Path::new("/Applications/Portal.app/Contents/MacOS/Portal")),
            Some(PathBuf::from("/Applications/Portal.app"))
        );
        assert_eq!(app_from_executable(Path::new("/tmp/portal")), None);
    }
}
