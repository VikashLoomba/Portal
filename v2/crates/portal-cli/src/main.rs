//! `portal` — v2 CLI + daemon entry point. Most verbs are file/launchctl
//! operations (no daemon API needed); `status` reads the daemon's read-only
//! status socket; `daemon` is what launchd runs.

mod daemon;
mod launchd;
mod prompt_helper;
mod upgrade;

use std::io::Write as _;
use std::path::PathBuf;

use clap::{Parser, Subcommand};
use portal_core::config::{BoxConfig, Config, sanitize_name};
use portal_core::paths::Paths;
use portal_transport::runner::OsRunner;

#[derive(Parser)]
#[command(
    name = "portal",
    version,
    about = "Per-box SSH port forwarding, clipboard sync, and notification relay for remote dev boxes"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Configure a dev box and install the login agent (auto-start + self-heal)
    Install {
        /// ssh alias or user@host
        host: String,
        /// Box name (default: derived from the host)
        #[arg(long)]
        name: Option<String>,
        /// Port-mapping index (default: next free)
        #[arg(long)]
        index: Option<u8>,
    },
    /// Stop and remove the login agent (config is kept)
    Uninstall,
    /// Manage remote boxes
    #[command(subcommand)]
    r#Box(BoxCmd),
    /// Show per-box daemon state and the port mapping table
    Status,
    /// Load the login agent (start the daemon)
    Start,
    /// Unload the login agent (stop the daemon; forwards drop)
    Stop,
    /// Restart the daemon
    Restart,
    /// Force-forward ports for a box
    Allow { r#box: String, ports: Vec<u16> },
    /// Remove force-forwarded ports for a box
    Unallow { r#box: String, ports: Vec<u16> },
    /// Show recent daemon log lines
    Logs {
        /// Follow the log
        #[arg(short = 'f', long)]
        follow: bool,
        /// Last N lines (default 50)
        #[arg(default_value_t = 50)]
        lines: usize,
    },
    /// Show or toggle capability gates (clip-text / clip-image / notify / …)
    Features {
        name: Option<String>,
        #[arg(value_parser = ["on", "off"])]
        state: Option<String>,
    },
    /// Manage remembered credentials (askpass/sudo) — lands with the
    /// credentials phase (TASKS.md)
    #[command(subcommand)]
    Keychain(KeychainCmd),
    /// Install the newest release and reload the agent
    Upgrade {
        /// Only report whether a newer release exists
        #[arg(long)]
        check: bool,
        /// Reinstall the published release even if current
        #[arg(long)]
        force: bool,
    },
    /// Self-test each box: connection, shims, clipsync, forwards
    Doctor,
    /// Run the daemon in the foreground (launchd entry point)
    #[command(hide = true)]
    Daemon,
}

#[derive(Subcommand)]
enum KeychainCmd {
    /// List remembered labels (and Touch ID availability)
    List,
    /// Forget one remembered credential
    Forget { label: String },
}

#[derive(Subcommand)]
enum BoxCmd {
    /// List configured boxes
    List,
    /// Add a box (allocates the next free index unless --index is given)
    Add {
        host: String,
        #[arg(long)]
        name: Option<String>,
        #[arg(long)]
        index: Option<u8>,
    },
    /// Remove a box from the config
    Remove { name: String },
}

/// The build SHA both the agent upload and `--version` stamp (must match
/// the portald the release pipeline embeds — the SHA match is what heals
/// stale agents on the box).
pub const BUILD_SHA: &str = match option_env!("PORTAL_GIT_SHA") {
    Some(sha) => sha,
    None => "dev",
};

fn main() {
    // Hidden helper subcommand, dispatched before clap (it must own the
    // process: AppKit wants the main thread, and the arg shape is internal).
    let raw: Vec<String> = std::env::args().collect();
    if raw.get(1).map(String::as_str) == Some("_prompt") {
        std::process::exit(prompt_helper::run());
    }
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();
    // `--version` needs the build SHA (the box-side agent-heal compares it),
    // so intercept before clap's plain version printer. The bare `version`
    // subcommand is v1 parity — v1's `portal upgrade` self-tests a downloaded
    // binary by running `<binary> version` and checking the release tag
    // appears in the output; without it a v1→v2 upgrade refuses the swap.
    if matches!(
        std::env::args().nth(1).as_deref(),
        Some("--version") | Some("version")
    ) {
        println!("portal v{} (sha {})", env!("CARGO_PKG_VERSION"), BUILD_SHA);
        std::process::exit(0);
    }
    let cli = Cli::parse();
    let home = PathBuf::from(std::env::var_os("HOME").expect("HOME not set"));
    let uid = unsafe { libc_getuid() };
    let paths = Paths::derive(&home, uid);
    let code = run(cli.command, paths);
    std::process::exit(code);
}

// getuid without a libc dependency line for one call.
unsafe fn libc_getuid() -> u32 {
    unsafe extern "C" {
        fn getuid() -> u32;
    }
    unsafe { getuid() }
}

fn run(cmd: Command, paths: Paths) -> i32 {
    match cmd {
        Command::Daemon => block_on_daemon(paths),
        Command::Install { host, name, index } => install(&paths, &host, name, index),
        Command::Uninstall => uninstall(&paths),
        Command::Start => launchctl_verb(&paths, Verb::Start),
        Command::Stop => launchctl_verb(&paths, Verb::Stop),
        Command::Restart => launchctl_verb(&paths, Verb::Restart),
        Command::Status => status(&paths),
        Command::r#Box(cmd) => box_cmd(&paths, cmd),
        Command::Allow { r#box, ports } => mutate_allow(&paths, &r#box, &ports, true),
        Command::Unallow { r#box, ports } => mutate_allow(&paths, &r#box, &ports, false),
        Command::Logs { follow, lines } => logs(&paths, follow, lines),
        Command::Doctor => doctor(&paths),
        Command::Upgrade { check, force } => upgrade(&paths, check, force),
        Command::Features { name, state } => features(&paths, name, state),
        Command::Keychain(cmd) => keychain(cmd),
    }
}

#[cfg(target_os = "macos")]
fn keychain(cmd: KeychainCmd) -> i32 {
    use portal_cred::keychain::Keychain as _;
    use portal_cred::prompt::Biometry as _;
    let kc = portal_cred::macos::MacKeychain;
    match cmd {
        KeychainCmd::List => {
            let bio = portal_cred::macos::MacBiometry;
            println!(
                "touch id: {}",
                if bio.available() {
                    "available"
                } else {
                    "unavailable"
                }
            );
            match kc.list() {
                Ok(labels) if labels.is_empty() => println!("no remembered credentials"),
                Ok(labels) => {
                    for l in labels {
                        println!("{l}");
                    }
                }
                Err(e) => {
                    eprintln!("portal keychain: {e}");
                    return 1;
                }
            }
            0
        }
        KeychainCmd::Forget { label } => match kc.delete(&label) {
            Ok(()) => {
                println!("forgot {label:?}");
                0
            }
            Err(e) => {
                eprintln!("portal keychain: {e}");
                1
            }
        },
    }
}

#[cfg(not(target_os = "macos"))]
fn keychain(_cmd: KeychainCmd) -> i32 {
    eprintln!("portal keychain: macOS only");
    2
}

fn block_on_daemon(paths: Paths) -> i32 {
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    match rt.block_on(daemon::run(paths)) {
        Ok(()) => 0,
        Err(e) => {
            eprintln!("portal daemon: {e}");
            1
        }
    }
}

enum Verb {
    Start,
    Stop,
    Restart,
}

fn launchctl_verb(paths: &Paths, verb: Verb) -> i32 {
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    rt.block_on(async {
        let runner = OsRunner;
        let l = launchd::Launchd::new(&runner, paths.uid, paths.label.clone());
        let result = match verb {
            Verb::Start => l.load(&paths.plist).await.map(|_| "started"),
            Verb::Stop => match l.unload().await {
                Ok(true) => Ok("stopped"),
                Ok(false) => Ok("was not running"),
                Err(e) => Err(e),
            },
            Verb::Restart => {
                if l.is_loaded().await.unwrap_or(false) {
                    l.kickstart().await.map(|_| "restarted")
                } else {
                    l.load(&paths.plist).await.map(|_| "started")
                }
            }
        };
        match result {
            Ok(msg) => {
                println!("portal: {msg}");
                0
            }
            Err(e) => {
                eprintln!("portal: {e}");
                1
            }
        }
    })
}

fn install(paths: &Paths, host: &str, name: Option<String>, index: Option<u8>) -> i32 {
    // 1. Config: add the box (or create the config).
    if let Err(e) = add_box_to_config(paths, host, name, index) {
        eprintln!("portal install: {e}");
        return 1;
    }
    // 2. Copy this binary to ~/.local/bin/portal (atomic).
    let self_path = match std::env::current_exe() {
        Ok(p) => p,
        Err(e) => {
            eprintln!("portal install: cannot locate own binary: {e}");
            return 1;
        }
    };
    if self_path != paths.bin_path
        && let Err(e) = install_binary(&self_path, &paths.bin_path)
    {
        eprintln!("portal install: {e}");
        return 1;
    }
    // 3. Render + load the launch agent.
    let plist = launchd::render_plist(
        &paths.label,
        &paths.bin_path,
        &["daemon"],
        &paths.home,
        &paths.log,
    );
    if let Some(dir) = paths.plist.parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    if let Err(e) = std::fs::write(&paths.plist, plist) {
        eprintln!("portal install: write plist: {e}");
        return 1;
    }
    let code = launchctl_verb(paths, Verb::Restart);
    if code != 0 {
        return code;
    }
    println!(
        "portal: installed. `portal status` shows per-box state; logs: {}",
        paths.log.display()
    );
    if !std::env::var("PATH")
        .unwrap_or_default()
        .contains(&paths.bin_dir.display().to_string())
    {
        println!(
            "note: add to PATH: export PATH=\"{}:$PATH\"",
            paths.bin_dir.display()
        );
    }
    0
}

fn install_binary(src: &PathBuf, dst: &PathBuf) -> Result<(), String> {
    if let Some(dir) = dst.parent() {
        std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
    }
    let tmp = dst.with_extension("tmp");
    std::fs::copy(src, &tmp).map_err(|e| format!("copy: {e}"))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o755))
            .map_err(|e| e.to_string())?;
    }
    std::fs::rename(&tmp, dst).map_err(|e| format!("rename: {e}"))
}

fn uninstall(paths: &Paths) -> i32 {
    let code = launchctl_verb(paths, Verb::Stop);
    let _ = std::fs::remove_file(&paths.plist);
    let _ = std::fs::remove_file(&paths.api_sock);
    println!(
        "portal: login agent removed (config kept at {})",
        paths.config_dir.display()
    );
    code
}

fn load_config(paths: &Paths) -> Result<Config, String> {
    daemon::load_or_migrate_config(paths)
}

fn save_config(paths: &Paths, cfg: &Config) -> Result<(), String> {
    cfg.validate().map_err(|e| e.to_string())?;
    let doc = toml::to_string_pretty(cfg).map_err(|e| e.to_string())?;
    std::fs::create_dir_all(&paths.config_dir).map_err(|e| e.to_string())?;
    let tmp = paths.config_file.with_extension("tmp");
    std::fs::write(&tmp, doc).map_err(|e| e.to_string())?;
    std::fs::rename(&tmp, &paths.config_file).map_err(|e| e.to_string())
}

fn add_box_to_config(
    paths: &Paths,
    host: &str,
    name: Option<String>,
    index: Option<u8>,
) -> Result<(), String> {
    let mut cfg = load_config(paths).unwrap_or_default();
    let name = name.unwrap_or_else(|| sanitize_name(host));
    if let Some(existing) = cfg.boxes.iter().find(|b| b.name == name) {
        if existing.host == host {
            // Idempotent reinstall/upgrade: same box, same host — the config
            // is already right; install proceeds to binary + plist + restart.
            println!("portal: box {name:?} already configured; keeping config");
            return Ok(());
        }
        return Err(format!(
            "box {name:?} already exists with host {:?} (portal box list)",
            existing.host
        ));
    }
    let index = match index {
        Some(i) => i,
        None => (1..=u8::MAX)
            .find(|i| !cfg.boxes.iter().any(|b| b.index == *i))
            .ok_or("no free index")?,
    };
    println!("portal: adding box {name:?} (index {index}: remote port p → local {index}0000+p)");
    cfg.boxes.push(BoxConfig {
        name,
        host: host.to_string(),
        index,
        allow: Vec::new(),
        deny: Vec::new(),
        enabled: true,
    });
    save_config(paths, &cfg)
}

fn box_cmd(paths: &Paths, cmd: BoxCmd) -> i32 {
    match cmd {
        BoxCmd::List => match load_config(paths) {
            Ok(cfg) => {
                for b in &cfg.boxes {
                    println!(
                        "{:<16} {:<24} index={} {}",
                        b.name,
                        b.host,
                        b.index,
                        if b.enabled { "" } else { "(disabled)" }
                    );
                }
                0
            }
            Err(e) => {
                eprintln!("portal: {e}");
                1
            }
        },
        BoxCmd::Add { host, name, index } => match add_box_to_config(paths, &host, name, index) {
            Ok(()) => {
                println!("portal: daemon applies it live (config hot-reload, ~2s)");
                0
            }
            Err(e) => {
                eprintln!("portal: {e}");
                1
            }
        },
        BoxCmd::Remove { name } => {
            let mut cfg = match load_config(paths) {
                Ok(c) => c,
                Err(e) => {
                    eprintln!("portal: {e}");
                    return 1;
                }
            };
            let before = cfg.boxes.len();
            cfg.boxes.retain(|b| b.name != name);
            if cfg.boxes.len() == before {
                eprintln!("portal: no box named {name:?}");
                return 1;
            }
            match save_config(paths, &cfg) {
                Ok(()) => {
                    println!("portal: removed {name:?}; daemon applies it live (~2s)");
                    0
                }
                Err(e) => {
                    eprintln!("portal: {e}");
                    1
                }
            }
        }
    }
}

fn mutate_allow(paths: &Paths, box_name: &str, ports: &[u16], add: bool) -> i32 {
    let mut cfg = match load_config(paths) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("portal: {e}");
            return 1;
        }
    };
    let Some(b) = cfg.boxes.iter_mut().find(|b| b.name == box_name) else {
        eprintln!("portal: no box named {box_name:?} (portal box list)");
        return 1;
    };
    if add {
        for &p in ports {
            if !b.allow.contains(&p) {
                b.allow.push(p);
                println!("allowed: {p}");
            }
        }
    } else {
        b.allow.retain(|p| {
            let drop = ports.contains(p);
            if drop {
                println!("unallowed: {p}");
            }
            !drop
        });
    }
    match save_config(paths, &cfg) {
        Ok(()) => {
            println!("portal: daemon applies it live (config hot-reload, ~2s)");
            0
        }
        Err(e) => {
            eprintln!("portal: {e}");
            1
        }
    }
}

fn status(paths: &Paths) -> i32 {
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    rt.block_on(async {
        // launchd view.
        let runner = OsRunner;
        let l = launchd::Launchd::new(&runner, paths.uid, paths.label.clone());
        match l.status_lines().await {
            Ok(lines) if !lines.is_empty() => {
                println!("login agent ({}):", paths.label);
                for line in lines {
                    println!("{line}");
                }
            }
            _ => println!("login agent: not loaded (portal start)"),
        }
        // Daemon view (status socket).
        use tokio::io::AsyncReadExt;
        match tokio::net::UnixStream::connect(&paths.api_sock).await {
            Ok(mut s) => {
                let mut buf = String::new();
                if s.read_to_string(&mut buf).await.is_ok() {
                    println!("boxes:");
                    println!("{buf}");
                }
                0
            }
            Err(_) => {
                println!("daemon: not reachable at {}", paths.api_sock.display());
                1
            }
        }
    })
}

fn logs(paths: &Paths, follow: bool, lines: usize) -> i32 {
    if follow {
        // tail -f is exactly right; don't reimplement it.
        let err = std::process::Command::new("tail")
            .arg("-f")
            .arg(&paths.log)
            .status();
        return match err {
            Ok(st) => st.code().unwrap_or(1),
            Err(e) => {
                eprintln!("portal logs: {e}");
                1
            }
        };
    }
    match std::fs::read_to_string(&paths.log) {
        Ok(content) => {
            let all: Vec<&str> = content.lines().collect();
            let start = all.len().saturating_sub(lines);
            let mut out = std::io::stdout().lock();
            for line in &all[start..] {
                let _ = writeln!(out, "{line}");
            }
            0
        }
        Err(e) => {
            eprintln!("portal logs: {} ({e})", paths.log.display());
            1
        }
    }
}

fn upgrade(paths: &Paths, check: bool, force: bool) -> i32 {
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    rt.block_on(async {
        let runner = OsRunner;
        let current = format!("v{}", env!("CARGO_PKG_VERSION"));
        match upgrade::upgrade(&runner, &paths.bin_path, &current, check, force).await {
            Ok(msg) => {
                println!("portal: {msg}");
                if !check && !msg.contains("up to date") {
                    println!("portal: reload the agent to run the new binary (portal restart)");
                }
                0
            }
            Err(e) => {
                eprintln!("portal upgrade: {e}");
                1
            }
        }
    })
}

fn doctor(paths: &Paths) -> i32 {
    let cfg = match load_config(paths) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("portal doctor: {e}");
            return 1;
        }
    };
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    rt.block_on(async {
        // Daemon-view checks come from the status socket when the daemon is
        // up; the box-side checks run over a FRESH transport (works even
        // with the daemon stopped — doctor must diagnose that case too).
        use tokio::io::AsyncReadExt;
        let statuses: Vec<serde_json::Value> =
            match tokio::net::UnixStream::connect(&paths.api_sock).await {
                Ok(mut s) => {
                    let mut buf = String::new();
                    let _ = s.read_to_string(&mut buf).await;
                    serde_json::from_str(&buf).unwrap_or_default()
                }
                Err(_) => {
                    println!("daemon: not running (box checks still run)");
                    Vec::new()
                }
            };
        let mut failed = false;
        for b in cfg.enabled_boxes() {
            println!("{} ({}):", b.name, b.host);
            if let Some(st) = statuses.iter().find(|s| s["name"] == b.name.as_str()) {
                let status = portal_core::supervisor::BoxStatus {
                    name: b.name.clone(),
                    host: b.host.clone(),
                    index: b.index,
                    connected: st["connected"].as_bool().unwrap_or(false),
                    agent_sha: st["agent_sha"].as_str().map(str::to_string),
                    forwards: st["forwards"]
                        .as_array()
                        .map(|a| {
                            a.iter()
                                .filter_map(|p| {
                                    Some((p[0].as_u64()? as u16, p[1].as_u64()? as u16))
                                })
                                .collect()
                        })
                        .unwrap_or_default(),
                    clipsync_synced: st["clipsync_synced"].as_bool().unwrap_or(false),
                    clipsync_change_id: st["clipsync_change_id"].as_u64().unwrap_or(0),
                };
                for v in portal_core::doctor::check_status(&status) {
                    failed |= v.is_fail();
                    println!("{}", v.line());
                }
            }
            let (transport, _fwd) = portal_core::supervisor::native_transport(b);
            for v in portal_core::doctor::check_box(&*transport).await {
                failed |= v.is_fail();
                println!("{}", v.line());
            }
            let _ = transport.close().await;
        }
        if failed { 1 } else { 0 }
    })
}

fn features(paths: &Paths, name: Option<String>, state: Option<String>) -> i32 {
    const KNOWN: [&str; 4] = ["clip-text", "clip-image", "clip-write", "notify"];
    let gates = daemon::feature_gates(paths.config_dir.clone());
    match (name, state) {
        (None, _) => {
            for f in KNOWN {
                println!("{f}: {}", if gates(f) { "on" } else { "off" });
            }
            0
        }
        (Some(f), None) => {
            println!("{f}: {}", if gates(&f) { "on" } else { "off" });
            0
        }
        (Some(f), Some(state)) => {
            let _ = std::fs::create_dir_all(&paths.config_dir);
            match std::fs::write(paths.feature_file(&f), format!("{state}\n")) {
                Ok(()) => {
                    println!("{f}: {state} (picked up live by the daemon)");
                    0
                }
                Err(e) => {
                    eprintln!("portal features: {e}");
                    1
                }
            }
        }
    }
}
