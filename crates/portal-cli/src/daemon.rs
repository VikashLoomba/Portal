//! The daemon: what launchd runs (`portal daemon`). Loads config (with v1
//! auto-migration), acquires the single-instance lock, composes the
//! Supervisor with production deps (native transport, NSPasteboard watcher,
//! feature-gate files, osascript notifications, `open`), serves the minimal
//! read-only status socket, and exits cleanly on SIGTERM.

use std::path::PathBuf;
use std::sync::Arc;

use tokio::io::AsyncWriteExt;
use tokio::net::{UnixListener, UnixStream};
use tokio_util::sync::CancellationToken;

use portal_core::bootstrap::EmbeddedAgent;
use portal_core::config::Config;
use portal_core::paths::Paths;
use portal_core::supervisor::{BoxStatus, Deps, NotifyEvent, Supervisor, native_transport};

/// Embedded portald binaries: baked at BUILD time when the release pipeline
/// sets PORTAL_AGENT_*_FILE (build.rs); dev builds fall back to runtime env
/// paths (PORTAL_AGENT_AMD64/ARM64).
pub fn embedded_agent() -> EmbeddedAgent {
    EmbeddedAgent {
        git_sha: crate::BUILD_SHA.to_string(),
        linux_amd64: agent_bytes(
            include_bytes!(concat!(env!("OUT_DIR"), "/agent-amd64.bin")),
            "PORTAL_AGENT_AMD64",
        ),
        linux_arm64: agent_bytes(
            include_bytes!(concat!(env!("OUT_DIR"), "/agent-arm64.bin")),
            "PORTAL_AGENT_ARM64",
        ),
    }
}

/// Build-time bytes (release pipeline, via build.rs) win; else the RUNTIME
/// env path (dev/staging); else None — bootstrap fails LOUDLY per box.
fn agent_bytes(embedded: &'static [u8], runtime_var: &str) -> Option<Arc<[u8]>> {
    if !embedded.is_empty() {
        return Some(Arc::from(embedded));
    }
    if let Ok(path) = std::env::var(runtime_var)
        && let Ok(bytes) = std::fs::read(&path)
    {
        return Some(Arc::from(bytes.into_boxed_slice()));
    }
    None
}

/// Load config.toml; if absent but v1 files exist, auto-migrate (writing the
/// new file). v1's same-port forwarding carries over unchanged: the migrated
/// box keeps the identity mapping whenever the local port is free.
pub fn load_or_migrate_config(paths: &Paths) -> Result<Config, String> {
    if paths.config_file.exists() {
        let raw = std::fs::read_to_string(&paths.config_file)
            .map_err(|e| format!("read {}: {e}", paths.config_file.display()))?;
        return Config::parse(&raw).map_err(|e| e.to_string());
    }
    if paths.v1_host_file.exists() {
        let host = std::fs::read_to_string(&paths.v1_host_file)
            .map_err(|e| e.to_string())?
            .split_whitespace()
            .collect::<String>();
        // v1 allow files may carry `#` comments ("8000 # api"): strip per
        // line before tokenizing so a port glued to a comment isn't dropped.
        let allow: Vec<u16> = std::fs::read_to_string(&paths.v1_allow_file)
            .unwrap_or_default()
            .lines()
            .map(|l| l.split('#').next().unwrap_or(""))
            .flat_map(str::split_whitespace)
            .filter_map(|t| t.parse().ok())
            .collect();
        let cfg = Config::migrate_from_v1(&host, &allow);
        let doc = toml::to_string_pretty(&cfg).map_err(|e| e.to_string())?;
        std::fs::create_dir_all(&paths.config_dir).map_err(|e| e.to_string())?;
        std::fs::write(&paths.config_file, &doc).map_err(|e| e.to_string())?;
        tracing::info!(
            "migrated v1 config: box {:?} has index 1 — forwarded ports keep their \
             remote numbers (localhost:8000 → 8000), so Host/Origin-checking \
             services keep working",
            cfg.boxes[0].name
        );
        return Ok(cfg);
    }
    Err(format!(
        "no configuration: create {} or run `portal install <host>`",
        paths.config_file.display()
    ))
}

#[cfg(target_os = "macos")]
fn clipboard_writer() -> Option<Arc<dyn portal_clip::ClipboardWriter>> {
    Some(Arc::new(portal_clip::macos::NativePasteboard::new()))
}

#[cfg(not(target_os = "macos"))]
fn clipboard_writer() -> Option<Arc<dyn portal_clip::ClipboardWriter>> {
    None
}

/// Live feature gates: v1 file-per-toggle contract (missing = ON; contents
/// off/false/0/no/disabled = OFF; re-read every call).
pub fn feature_gates(config_dir: PathBuf) -> Arc<dyn Fn(&str) -> bool + Send + Sync> {
    Arc::new(move |feature: &str| {
        match std::fs::read_to_string(config_dir.join(format!("feature.{feature}"))) {
            Err(_) => true,
            Ok(s) => !matches!(
                s.split_whitespace()
                    .collect::<String>()
                    .to_lowercase()
                    .as_str(),
                "off" | "false" | "0" | "no" | "disabled"
            ),
        }
    })
}

/// Native macOS notification via osascript (v1's cgo-free path). Box name in
/// the subtitle; unverified events carry the v1 "[unverified] " title prefix.
pub fn osascript_notify(ev: NotifyEvent) {
    let title = if ev.verified {
        ev.title
    } else {
        format!("[unverified] {}", ev.title)
    };
    let script = format!(
        "display notification {} with title {} subtitle {}",
        applescript_str(ev.body.as_deref().unwrap_or("")),
        applescript_str(&title),
        applescript_str(&format!("portal · {}", ev.box_name)),
    );
    std::thread::spawn(move || {
        let _ = std::process::Command::new("osascript")
            .arg("-e")
            .arg(&script)
            .output();
    });
}

fn applescript_str(s: &str) -> String {
    format!("\"{}\"", s.replace('\\', "\\\\").replace('"', "\\\""))
}

pub fn open_url(url: String) {
    // The cmd-socket layer already enforced http(s)-only; defense in depth.
    if !(url.starts_with("https://") || url.starts_with("http://")) {
        return;
    }
    std::thread::spawn(move || {
        let _ = std::process::Command::new("open").arg(&url).output();
    });
}

/// Single-instance lock + status endpoint in one socket (v1 D7): probe an
/// existing socket first — if something answers, another daemon owns it.
pub async fn bind_status_socket(path: &PathBuf) -> Result<UnixListener, String> {
    if UnixStream::connect(path).await.is_ok() {
        return Err(format!(
            "another portal daemon already owns {}",
            path.display()
        ));
    }
    let _ = std::fs::remove_file(path); // stale socket from a crash
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let listener = UnixListener::bind(path).map_err(|e| format!("bind {}: {e}", path.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600));
    }
    Ok(listener)
}

pub fn status_json(statuses: &[BoxStatus]) -> String {
    // Hand-rolled (stable, tiny) rather than pulling serde_json into the CLI
    // for one shape... except serde_json is already a workspace dep — use it.
    #[derive(serde::Serialize)]
    struct S<'a> {
        name: &'a str,
        host: &'a str,
        index: u8,
        connected: bool,
        agent_sha: Option<&'a str>,
        forwards: &'a [(u16, u16)],
        clipsync_synced: bool,
        clipsync_change_id: u64,
    }
    let view: Vec<S> = statuses
        .iter()
        .map(|b| S {
            name: &b.name,
            host: &b.host,
            index: b.index,
            connected: b.connected,
            agent_sha: b.agent_sha.as_deref(),
            forwards: &b.forwards,
            clipsync_synced: b.clipsync_synced,
            clipsync_change_id: b.clipsync_change_id,
        })
        .collect();
    serde_json::to_string_pretty(&view).unwrap_or_else(|_| "[]".into())
}

/// Production cred deps: helper-process dialog (this binary's `_prompt`),
/// in-process LAContext biometry, macOS Keychain (macOS only; None elsewhere).
fn cred_deps(bin_path: PathBuf) -> Option<Arc<portal_core::cred::CredDeps>> {
    #[cfg(target_os = "macos")]
    {
        Some(Arc::new(portal_core::cred::CredDeps {
            prompter: Box::new(portal_cred::helper::HelperPrompter {
                helper_path: bin_path,
            }),
            biometry: Some(Box::new(portal_cred::macos::MacBiometry)),
            keychain: Box::new(portal_cred::macos::MacKeychain),
            cooldown: portal_cred::cooldown::Cooldown::default(),
        }))
    }
    #[cfg(not(target_os = "macos"))]
    {
        let _ = bin_path;
        None
    }
}

/// The daemon entry point.
pub async fn run(paths: Paths) -> Result<(), String> {
    let config = load_or_migrate_config(&paths)?;
    let listener = bind_status_socket(&paths.api_sock).await?;

    let cancel = CancellationToken::new();
    let deps = Deps {
        agent: embedded_agent(),
        gates: feature_gates(paths.config_dir.clone()),
        notify: Arc::new(osascript_notify),
        open_url: Arc::new(open_url),
        transport: Arc::new(native_transport),
        cred: cred_deps(std::env::current_exe().unwrap_or_else(|_| paths.bin_path.clone())),
        clipboard_writer: clipboard_writer(),
    };

    #[cfg(target_os = "macos")]
    let watcher = Some((
        portal_clip::macos::NativePasteboard::new(),
        FileGates {
            gates: feature_gates(paths.config_dir.clone()),
        },
    ));
    #[cfg(not(target_os = "macos"))]
    let watcher: Option<(NoSource, FileGates)> = None;

    let supervisor = Arc::new(tokio::sync::Mutex::new(Supervisor::start(
        &config,
        &deps,
        watcher,
        cancel.clone(),
    )));
    tracing::info!(boxes = config.enabled_boxes().count(), "portal daemon up");

    // Status socket reads through the lock.
    let status_sup = supervisor.clone();
    let status_task = tokio::spawn({
        let cancel = cancel.clone();
        async move {
            serve_status_locked(listener, status_sup, cancel).await;
        }
    });

    // Config hot-reload: poll mtime (2s), reconcile on change. allow/unallow
    // + box add/remove apply WITHOUT a daemon restart (v1's live-file-read
    // semantics, generalized to the TOML config).
    let config_task = tokio::spawn({
        let supervisor = supervisor.clone();
        let cancel = cancel.clone();
        let path = paths.config_file.clone();
        async move {
            let mut last_mtime = std::fs::metadata(&path)
                .ok()
                .and_then(|m| m.modified().ok());
            let mut tick = tokio::time::interval(std::time::Duration::from_secs(2));
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    _ = tick.tick() => {
                        let mtime = std::fs::metadata(&path).ok().and_then(|m| m.modified().ok());
                        if mtime == last_mtime {
                            continue;
                        }
                        last_mtime = mtime;
                        match std::fs::read_to_string(&path)
                            .map_err(|e| e.to_string())
                            .and_then(|raw| Config::parse(&raw).map_err(|e| e.to_string()))
                        {
                            Ok(cfg) => {
                                tracing::info!(boxes = cfg.enabled_boxes().count(),
                                    "config changed; hot-reloading");
                                supervisor.lock().await.reconcile(&cfg).await;
                            }
                            Err(e) => {
                                tracing::warn!("config reload skipped (invalid): {e}");
                            }
                        }
                    }
                }
            }
        }
    });

    // SIGTERM (launchd bootout) and ctrl-c both drain cleanly.
    let mut sigterm = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .map_err(|e| e.to_string())?;
    tokio::select! {
        _ = sigterm.recv() => tracing::info!("SIGTERM: shutting down"),
        _ = tokio::signal::ctrl_c() => tracing::info!("interrupt: shutting down"),
    }
    cancel.cancel();
    let _ = status_task.await;
    let _ = config_task.await;
    let _ = std::fs::remove_file(&paths.api_sock);
    Ok(())
}

/// Status serving against the Mutex-wrapped supervisor (hot-reload needs
/// &mut access, so the Arc holds the lock).
async fn serve_status_locked(
    listener: UnixListener,
    supervisor: Arc<tokio::sync::Mutex<Supervisor>>,
    cancel: CancellationToken,
) {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return,
            conn = listener.accept() => {
                let Ok((mut stream, _)) = conn else { continue };
                let snapshot = status_json(&supervisor.lock().await.status());
                tokio::spawn(async move {
                    let _ = stream.write_all(snapshot.as_bytes()).await;
                    let _ = stream.shutdown().await;
                });
            }
        }
    }
}

/// File-backed gates adapter for the pasteboard watcher.
pub struct FileGates {
    gates: Arc<dyn Fn(&str) -> bool + Send + Sync>,
}

impl portal_clip::watcher::Gates for FileGates {
    fn text_enabled(&self) -> bool {
        (self.gates)("clip-text")
    }
    fn image_enabled(&self) -> bool {
        (self.gates)("clip-image")
    }
}

#[cfg(not(target_os = "macos"))]
pub struct NoSource;
#[cfg(not(target_os = "macos"))]
impl portal_clip::watcher::SnapshotSource for NoSource {
    fn change_count(&self) -> i64 {
        0
    }
    fn observe(&self) -> Result<portal_clip::watcher::Observation, portal_clip::ClipError> {
        Ok(portal_clip::watcher::Observation::Empty)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn paths_in(dir: &std::path::Path) -> Paths {
        Paths::derive_with(dir, 501, |k| match k {
            "PORTAL_CONFIG_DIR" => Some(dir.join("cfg").display().to_string()),
            _ => None,
        })
    }

    #[test]
    fn v1_files_migrate_with_loud_port_shift() {
        let dir = tempfile::tempdir().unwrap();
        let paths = paths_in(dir.path());
        std::fs::create_dir_all(&paths.config_dir).unwrap();
        std::fs::write(&paths.v1_host_file, "vikash@devbox\n").unwrap();
        std::fs::write(&paths.v1_allow_file, "9000\n8080\n").unwrap();

        let cfg = load_or_migrate_config(&paths).unwrap();
        assert_eq!(cfg.boxes.len(), 1);
        assert_eq!(cfg.boxes[0].host, "vikash@devbox");
        assert_eq!(cfg.boxes[0].index, 1);
        assert_eq!(cfg.boxes[0].allow, vec![9000, 8080]);
        assert!(paths.config_file.exists(), "migration persists");
        // Second load reads the migrated file.
        let again = load_or_migrate_config(&paths).unwrap();
        assert_eq!(again, cfg);
    }

    #[test]
    fn no_config_is_a_clear_error() {
        let dir = tempfile::tempdir().unwrap();
        let err = load_or_migrate_config(&paths_in(dir.path())).unwrap_err();
        assert!(err.contains("portal install"), "{err}");
    }

    #[test]
    fn gates_read_live() {
        let dir = tempfile::tempdir().unwrap();
        let gates = feature_gates(dir.path().to_path_buf());
        assert!(gates("clip-text"), "missing file = ON");
        std::fs::write(dir.path().join("feature.clip-text"), "off\n").unwrap();
        assert!(!gates("clip-text"));
        std::fs::write(dir.path().join("feature.clip-text"), "on\n").unwrap();
        assert!(gates("clip-text"));
    }

    #[test]
    fn status_json_shape() {
        let s = status_json(&[BoxStatus {
            name: "devbox1".into(),
            host: "h".into(),
            index: 1,
            connected: true,
            agent_sha: Some("cafe".into()),
            forwards: vec![(18000, 8000)],
            clipsync_synced: true,
            clipsync_change_id: 7,
        }]);
        assert!(s.contains("\"devbox1\""));
        assert!(s.contains("18000"));
        let parsed: serde_json::Value = serde_json::from_str(&s).unwrap();
        assert_eq!(parsed[0]["clipsync_change_id"], 7);
    }
}
