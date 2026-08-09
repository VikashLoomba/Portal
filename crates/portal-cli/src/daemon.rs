//! The daemon: what launchd runs (`portal daemon`). Loads config (with v1
//! auto-migration), acquires the single-instance lock, composes the
//! Supervisor with production deps (native transport, NSPasteboard watcher,
//! feature-gate files, osascript notifications, `open`), serves the versioned
//! local control API plus legacy status snapshots, and exits cleanly on SIGTERM.

use std::collections::BTreeMap;
use std::io::{Read as _, Seek as _, SeekFrom};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt};
use tokio::net::{UnixListener, UnixStream};
use tokio_util::sync::CancellationToken;

use portal_core::bootstrap::EmbeddedAgent;
use portal_core::config::{BoxConfig, Config, sanitize_name};
use portal_core::localapi::{
    API_VERSION, KNOWN_FEATURES, Request, RequestEnvelope, Response, ResponseEnvelope, State,
};
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
    // A packaged Portal.app must be able to start before first-run onboarding
    // has added a box. Existing or migratable configuration still fails loud
    // when malformed; only a genuinely fresh installation starts empty.
    let config = if paths.config_file.exists() || paths.v1_host_file.exists() {
        load_or_migrate_config(&paths)?
    } else {
        let config = Config::default();
        crate::save_config(&paths, &config)?;
        config
    };
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
    let live_config = Arc::new(tokio::sync::Mutex::new(config.clone()));
    tracing::info!(boxes = config.enabled_boxes().count(), "portal daemon up");

    // The local API is the single integration surface for Portal.app. Legacy
    // clients that write nothing still receive the old bare status array.
    let status_sup = supervisor.clone();
    let status_config = live_config.clone();
    let status_paths = paths.clone();
    let status_task = tokio::spawn({
        let cancel = cancel.clone();
        async move {
            serve_local_api(listener, status_sup, status_config, status_paths, cancel).await;
        }
    });

    // Only release compatibility binaries carry the build flag that enables
    // this one-time migration in a separate one-shot launchd job. Source
    // builds never auto-download anything.
    crate::spawn_app_migration_if_needed(&paths);

    // Config hot-reload: poll mtime (2s), reconcile on change. allow/unallow
    // + box add/remove apply WITHOUT a daemon restart (v1's live-file-read
    // semantics, generalized to the TOML config).
    let config_task = tokio::spawn({
        let supervisor = supervisor.clone();
        let live_config = live_config.clone();
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
                                *live_config.lock().await = cfg.clone();
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

/// Serve the versioned control API while preserving the v2.0 bare-snapshot
/// response for clients that connect and write nothing.
async fn serve_local_api(
    listener: UnixListener,
    supervisor: Arc<tokio::sync::Mutex<Supervisor>>,
    config: Arc<tokio::sync::Mutex<Config>>,
    paths: Paths,
    cancel: CancellationToken,
) {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return,
            conn = listener.accept() => {
                let Ok((stream, _)) = conn else { continue };
                match stream.peer_cred() {
                    Ok(credentials) if credentials.uid() == paths.uid => {}
                    Ok(credentials) => {
                        tracing::warn!(peer_uid = credentials.uid(), "rejected local API client with a different uid");
                        continue;
                    }
                    Err(error) => {
                        tracing::warn!(%error, "could not authenticate local API peer");
                        continue;
                    }
                }
                let supervisor = supervisor.clone();
                let config = config.clone();
                let paths = paths.clone();
                let cancel = cancel.clone();
                tokio::spawn(async move {
                    handle_local_api_connection(stream, supervisor, config, paths, cancel).await;
                });
            }
        }
    }
}

async fn handle_local_api_connection(
    stream: UnixStream,
    supervisor: Arc<tokio::sync::Mutex<Supervisor>>,
    config: Arc<tokio::sync::Mutex<Config>>,
    paths: Paths,
    cancel: CancellationToken,
) {
    let mut reader = tokio::io::BufReader::new(stream);
    let mut line = String::new();
    let read = tokio::time::timeout(Duration::from_millis(25), reader.read_line(&mut line)).await;
    let mut stream = reader.into_inner();

    // v2.0 clients wait for the server to speak first. A short bounded grace
    // period distinguishes them from API clients, which write immediately.
    match read {
        Err(_) | Ok(Ok(0)) if line.is_empty() => {
            let snapshot = status_json(&supervisor.lock().await.status());
            let _ = stream.write_all(snapshot.as_bytes()).await;
            let _ = stream.shutdown().await;
            return;
        }
        Err(_) => {
            let _ = write_api_response(
                &mut stream,
                &ResponseEnvelope::error(0, "request_timeout", "request line was not completed"),
            )
            .await;
            return;
        }
        Ok(Err(error)) => {
            tracing::debug!(%error, "failed reading local API request");
            return;
        }
        Ok(Ok(_)) => {}
    }

    if line.len() > 64 * 1024 {
        let _ = write_api_response(
            &mut stream,
            &ResponseEnvelope::error(0, "request_too_large", "local API request exceeds 64 KiB"),
        )
        .await;
        return;
    }
    let request: RequestEnvelope = match serde_json::from_str(&line) {
        Ok(request) => request,
        Err(error) => {
            let _ = write_api_response(
                &mut stream,
                &ResponseEnvelope::error(0, "invalid_json", error.to_string()),
            )
            .await;
            return;
        }
    };
    if request.api_version != API_VERSION {
        let _ = write_api_response(
            &mut stream,
            &ResponseEnvelope::error(
                request.id,
                "unsupported_api_version",
                format!(
                    "client requested API {}; daemon supports API {}",
                    request.api_version, API_VERSION
                ),
            ),
        )
        .await;
        return;
    }

    if request.request == Request::SubscribeState {
        let mut changes = supervisor.lock().await.subscribe_state_changes();
        let initial = local_api_state(&supervisor, &config, &paths).await;
        if write_api_response(
            &mut stream,
            &ResponseEnvelope::new(request.id, Response::State { state: initial }),
        )
        .await
        .is_err()
        {
            return;
        }
        loop {
            tokio::select! {
                _ = cancel.cancelled() => return,
                changed = changes.recv() => match changed {
                    Ok(()) | Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => {
                        let state = local_api_state(&supervisor, &config, &paths).await;
                        let response = ResponseEnvelope::new(request.id, Response::State { state });
                        if write_api_response(&mut stream, &response).await.is_err() {
                            return;
                        }
                    }
                    Err(tokio::sync::broadcast::error::RecvError::Closed) => return,
                }
            }
        }
    }

    let response = handle_local_api_request(request, &supervisor, &config, &paths).await;
    let _ = write_api_response(&mut stream, &response).await;
    let _ = stream.shutdown().await;
}

async fn write_api_response(
    stream: &mut UnixStream,
    response: &ResponseEnvelope,
) -> std::io::Result<()> {
    let mut bytes = serde_json::to_vec(response).map_err(std::io::Error::other)?;
    bytes.push(b'\n');
    stream.write_all(&bytes).await
}

async fn local_api_state(
    supervisor: &Arc<tokio::sync::Mutex<Supervisor>>,
    config: &Arc<tokio::sync::Mutex<Config>>,
    paths: &Paths,
) -> State {
    let boxes = config.lock().await.boxes.clone();
    let statuses = supervisor.lock().await.status();
    let gates = feature_gates(paths.config_dir.clone());
    let features = KNOWN_FEATURES
        .into_iter()
        .map(|name| (name.to_string(), gates(name)))
        .collect::<BTreeMap<_, _>>();
    State {
        version: env!("CARGO_PKG_VERSION").to_string(),
        build_sha: crate::BUILD_SHA.to_string(),
        boxes,
        statuses,
        features,
    }
}

async fn handle_local_api_request(
    request: RequestEnvelope,
    supervisor: &Arc<tokio::sync::Mutex<Supervisor>>,
    config: &Arc<tokio::sync::Mutex<Config>>,
    paths: &Paths,
) -> ResponseEnvelope {
    let id = request.id;
    let mutates_config = matches!(
        &request.request,
        Request::AddBox { .. }
            | Request::RemoveBox { .. }
            | Request::SetBoxEnabled { .. }
            | Request::SetAllow { .. }
    );
    let mutates_feature = matches!(&request.request, Request::SetFeature { .. });
    let result: Result<Response, String> = match request.request {
        Request::GetState => Ok(Response::State {
            state: local_api_state(supervisor, config, paths).await,
        }),
        Request::GetLogs { lines } => {
            read_log_tail(&paths.log, lines).map(|lines| Response::Logs { lines })
        }
        Request::SetFeature { name, enabled } => {
            if !KNOWN_FEATURES.contains(&name.as_str()) {
                Err(format!("unknown feature {name:?}"))
            } else {
                std::fs::create_dir_all(&paths.config_dir)
                    .and_then(|()| {
                        std::fs::write(
                            paths.feature_file(&name),
                            if enabled { "on\n" } else { "off\n" },
                        )
                    })
                    .map_err(|e| e.to_string())
                    .map(|()| Response::Ok {
                        message: format!("{name} is now {}", if enabled { "on" } else { "off" }),
                    })
            }
        }
        Request::AddBox { host, name, index } => {
            mutate_api_config(config, paths, move |next| {
                let name = name.unwrap_or_else(|| sanitize_name(&host));
                if next.boxes.iter().any(|b| b.name == name) {
                    return Err(format!("box {name:?} already exists"));
                }
                let index = index.unwrap_or_else(|| {
                    (1..=u8::MAX)
                        .find(|candidate| !next.boxes.iter().any(|b| b.index == *candidate))
                        .unwrap_or(0)
                });
                if index == 0 {
                    return Err("no free box index".into());
                }
                next.boxes.push(BoxConfig {
                    name: name.clone(),
                    host,
                    index,
                    allow: Vec::new(),
                    deny: Vec::new(),
                    enabled: true,
                });
                Ok(format!("added box {name:?}"))
            })
            .await
        }
        Request::RemoveBox { name } => {
            mutate_api_config(config, paths, move |next| {
                let before = next.boxes.len();
                next.boxes.retain(|b| b.name != name);
                if before == next.boxes.len() {
                    return Err(format!("no box named {name:?}"));
                }
                Ok(format!("removed box {name:?}"))
            })
            .await
        }
        Request::SetBoxEnabled { name, enabled } => {
            mutate_api_config(config, paths, move |next| {
                let Some(box_config) = next.boxes.iter_mut().find(|b| b.name == name) else {
                    return Err(format!("no box named {name:?}"));
                };
                box_config.enabled = enabled;
                Ok(format!(
                    "box {name:?} {}",
                    if enabled { "enabled" } else { "disabled" }
                ))
            })
            .await
        }
        Request::SetAllow {
            name,
            ports,
            allowed,
        } => {
            mutate_api_config(config, paths, move |next| {
                let Some(box_config) = next.boxes.iter_mut().find(|b| b.name == name) else {
                    return Err(format!("no box named {name:?}"));
                };
                if allowed {
                    for port in ports {
                        if !box_config.allow.contains(&port) {
                            box_config.allow.push(port);
                        }
                    }
                    box_config.allow.sort_unstable();
                } else {
                    box_config.allow.retain(|port| !ports.contains(port));
                }
                Ok(format!("updated allowlist for {name:?}"))
            })
            .await
        }
        Request::SubscribeState => unreachable!("handled above"),
    };

    match result {
        Ok(response) => {
            if mutates_config {
                let next = config.lock().await.clone();
                supervisor.lock().await.reconcile(&next).await;
            } else if mutates_feature {
                supervisor.lock().await.notify_state_changed();
            }
            ResponseEnvelope::new(id, response)
        }
        Err(message) => ResponseEnvelope::error(id, "operation_failed", message),
    }
}

async fn mutate_api_config(
    config: &Arc<tokio::sync::Mutex<Config>>,
    paths: &Paths,
    mutate: impl FnOnce(&mut Config) -> Result<String, String>,
) -> Result<Response, String> {
    let mut current = config.lock().await;
    let mut next = current.clone();
    let message = mutate(&mut next)?;
    crate::save_config(paths, &next)?;
    *current = next;
    Ok(Response::Ok { message })
}

/// Read a bounded tail without loading an arbitrarily large daemon log into
/// the UI process. The first partial line in the one-MiB window is discarded.
fn read_log_tail(path: &std::path::Path, lines: usize) -> Result<Vec<String>, String> {
    const MAX_BYTES: u64 = 1024 * 1024;
    const MAX_LINES: usize = 2000;
    let mut file =
        std::fs::File::open(path).map_err(|e| format!("read {}: {e}", path.display()))?;
    let len = file.metadata().map_err(|e| e.to_string())?.len();
    let start = len.saturating_sub(MAX_BYTES);
    file.seek(SeekFrom::Start(start))
        .map_err(|e| e.to_string())?;
    let mut text = String::new();
    file.read_to_string(&mut text).map_err(|e| e.to_string())?;
    if start > 0
        && let Some(newline) = text.find('\n')
    {
        text.drain(..=newline);
    }
    let all = text.lines().collect::<Vec<_>>();
    let count = lines.clamp(1, MAX_LINES);
    Ok(all[all.len().saturating_sub(count)..]
        .iter()
        .map(|line| strip_ansi(line))
        .collect())
}

/// Remove terminal color/control sequences before logs cross the local API.
/// This handles both real ESC bytes and `\\x1b[` sequences quoted inside a
/// nested agent log message.
fn strip_ansi(line: &str) -> String {
    let bytes = line.as_bytes();
    let mut out = String::with_capacity(line.len());
    let mut index = 0;
    while index < bytes.len() {
        let sequence_start = if bytes[index] == 0x1b && bytes.get(index + 1) == Some(&b'[') {
            Some(index + 2)
        } else if bytes.get(index..index + 5) == Some(b"\\x1b[") {
            Some(index + 5)
        } else {
            None
        };
        if let Some(mut end) = sequence_start {
            while end < bytes.len() && !(0x40..=0x7e).contains(&bytes[end]) {
                end += 1;
            }
            index = (end + 1).min(bytes.len());
            continue;
        }
        let Some(ch) = line[index..].chars().next() else {
            break;
        };
        out.push(ch);
        index += ch.len_utf8();
    }
    out
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
    fn log_api_removes_terminal_sequences() {
        assert_eq!(
            strip_ansi("\u{1b}[2m2026 INFO\u{1b}[0m nested \\x1b[33mWARN\\x1b[0m"),
            "2026 INFO nested WARN"
        );
    }

    #[test]
    fn bounded_log_tail_returns_requested_suffix() {
        let dir = tempfile::tempdir().unwrap();
        let log = dir.path().join("portal.log");
        std::fs::write(&log, "one\ntwo\nthree\nfour\n").unwrap();
        assert_eq!(read_log_tail(&log, 2).unwrap(), ["three", "four"]);
        assert_eq!(read_log_tail(&log, 0).unwrap(), ["four"]);
    }

    #[tokio::test]
    async fn api_config_mutation_is_validated_and_persisted() {
        let dir = tempfile::tempdir().unwrap();
        let paths = paths_in(dir.path());
        let config = Arc::new(tokio::sync::Mutex::new(Config::default()));
        let response = mutate_api_config(&config, &paths, |next| {
            next.boxes.push(BoxConfig {
                name: "dev".into(),
                host: "dev.example".into(),
                index: 1,
                allow: vec![],
                deny: vec![],
                enabled: true,
            });
            Ok("added".into())
        })
        .await
        .unwrap();
        assert!(matches!(response, Response::Ok { .. }));
        assert_eq!(
            Config::parse(&std::fs::read_to_string(paths.config_file).unwrap())
                .unwrap()
                .boxes
                .len(),
            1
        );
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
