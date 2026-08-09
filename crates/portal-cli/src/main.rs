//! `portal` — CLI, local daemon, and native Portal.app entry point. The GUI
//! uses the versioned owner-only local API; existing CLI verbs retain their
//! command shapes and lifecycle recovery paths.

mod app_install;
mod daemon;
mod deployment;
mod launchd;
mod local_client;
mod prompt_helper;
mod services;
mod tray;
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
    command: Option<Command>,
}

#[derive(Subcommand)]
enum Command {
    /// v1 compat — DO NOT REMOVE: v1's LaunchAgent plist runs `portal run`,
    /// and a v1→v2 `portal upgrade` keeps that plist. Without this alias the
    /// upgraded daemon crash-loops on clap's unknown-subcommand error
    /// (launchd: "spawn scheduled", ThrottleInterval pacing).
    #[command(hide = true)]
    Run,
    /// Open the native Portal desktop application
    App,
    /// Menu bar status item (normally started by launchd; `portal install`
    /// and `portal upgrade` manage its LaunchAgent)
    Tray,
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
    /// Stable-path second phase of self-upgrade. The public `upgrade` command
    /// execs a hard-linked copy before replacing ~/.local/bin/portal.
    #[command(name = "_apply-upgrade", hide = true)]
    ApplyUpgrade {
        #[arg(long)]
        app: bool,
        candidate: PathBuf,
        tag: String,
        staging: PathBuf,
    },
    /// Finish the one-time standalone-to-Portal.app migration after an older
    /// upgrader has installed this compatibility binary.
    #[command(name = "_complete-app-migration", hide = true)]
    CompleteAppMigration,
    #[command(name = "_app-migration-mode", hide = true)]
    AppMigrationMode,
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

/// The single wording for "which build is this", shared by `--version`, the
/// `version` subcommand, and the menu bar footer. One function so those
/// surfaces cannot drift, and so no test needs to name a version literal.
pub fn version_string() -> String {
    format!("portal v{} (sha {})", env!("CARGO_PKG_VERSION"), BUILD_SHA)
}

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
        println!("{}", version_string());
        std::process::exit(0);
    }
    let cli = Cli::parse();
    let home = PathBuf::from(std::env::var_os("HOME").expect("HOME not set"));
    let uid = unsafe { libc_getuid() };
    let paths = Paths::derive(&home, uid);
    let code = run(cli.command.unwrap_or(Command::App), paths);
    std::process::exit(code);
}

// getuid without a libc dependency line for one call.
pub(crate) unsafe fn libc_getuid() -> u32 {
    unsafe extern "C" {
        fn getuid() -> u32;
    }
    unsafe { getuid() }
}

fn run(cmd: Command, paths: Paths) -> i32 {
    match cmd {
        Command::Daemon | Command::Run => block_on_daemon(paths),
        Command::App => tray::run_app(),
        Command::Tray => tray::run(),
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
        Command::ApplyUpgrade {
            app,
            candidate,
            tag,
            staging,
        } => apply_prepared_upgrade(&paths, &candidate, &tag, &staging, app),
        Command::CompleteAppMigration => complete_app_migration(&paths),
        Command::AppMigrationMode => {
            println!(
                "{}",
                if auto_app_migration_enabled() {
                    "enabled"
                } else {
                    "disabled"
                }
            );
            0
        }
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

/// First-launch bridge for a signed Portal.app. The app bundle remains the
/// source of truth; the familiar CLI path is an owner-writable symlink to its
/// bundled executable. The daemon is then registered and started independently
/// so closing or quitting the UI never tears down SSH connections.
fn prepare_desktop_app(paths: &Paths) -> Result<(), String> {
    // Manual UI smoke tests use an isolated daemon and must not register the
    // real user's launchd labels.
    if std::env::var_os("PORTAL_SKIP_APP_BOOTSTRAP").is_some() {
        return Ok(());
    }
    let current = std::env::current_exe().map_err(|e| e.to_string())?;
    let bundled = current
        .ancestors()
        .any(|path| path.extension().is_some_and(|extension| extension == "app"));
    if bundled
        && (current.starts_with("/Volumes")
            || current.to_string_lossy().contains("/AppTranslocation/"))
    {
        return Err("move Portal.app to Applications before opening it".into());
    }
    let mut runtime_is_bundled = false;
    let mut migrate_standalone = false;

    if bundled {
        std::fs::create_dir_all(&paths.bin_dir).map_err(|e| e.to_string())?;
        match std::fs::symlink_metadata(&paths.bin_path) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                let target = std::fs::read_link(&paths.bin_path).map_err(|e| e.to_string())?;
                if target != current {
                    let staged = paths.bin_dir.join(".portal.app-link");
                    let _ = std::fs::remove_file(&staged);
                    std::os::unix::fs::symlink(&current, &staged).map_err(|e| e.to_string())?;
                    std::fs::rename(&staged, &paths.bin_path).map_err(|e| e.to_string())?;
                }
                runtime_is_bundled = true;
            }
            Ok(_) => {
                // Do not let an older app bundle undo a newer `portal upgrade`.
                // Otherwise first app launch migrates the standalone install
                // to the bundle-owned executable below.
                let standalone = std::process::Command::new(&paths.bin_path)
                    .arg("--version")
                    .output()
                    .ok()
                    .filter(|output| output.status.success())
                    .and_then(|output| String::from_utf8(output.stdout).ok())
                    .and_then(|output| {
                        output
                            .split_whitespace()
                            .find_map(|word| word.strip_prefix('v').map(str::to_string))
                    });
                migrate_standalone = !standalone.is_some_and(|version| {
                    upgrade::is_newer(
                        &format!("v{version}"),
                        &format!("v{}", env!("CARGO_PKG_VERSION")),
                    )
                });
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                std::os::unix::fs::symlink(&current, &paths.bin_path)
                    .map_err(|e| format!("install portal CLI link: {e}"))?;
                runtime_is_bundled = true;
            }
            Err(error) => return Err(error.to_string()),
        }
    }

    let runner = OsRunner;
    let agents = services::LoginAgents::new(&runner, paths);
    agents.write_manifests()?;

    if migrate_standalone {
        let _deployment = deployment::Deployment::acquire(&paths.bin_path)?;
        let backup = paths.bin_dir.join(".portal.pre-app");
        if backup.exists() {
            return Err(format!(
                "stale app-migration backup at {}",
                backup.display()
            ));
        }
        let rt = tokio::runtime::Runtime::new().map_err(|e| e.to_string())?;
        rt.block_on(agents.quiesce())?;
        if let Err(error) = std::fs::rename(&paths.bin_path, &backup) {
            let recovery = rt.block_on(agents.start_daemon_fresh());
            return Err(match recovery {
                Ok(()) => format!("stage existing portal CLI: {error}; daemon restarted"),
                Err(recovery) => format!(
                    "stage existing portal CLI: {error}; restarting daemon failed: {recovery}"
                ),
            });
        }
        let install_result = (|| {
            let staged = paths.bin_dir.join(".portal.app-link");
            let _ = std::fs::remove_file(&staged);
            std::os::unix::fs::symlink(&current, &staged)
                .map_err(|e| format!("stage portal app CLI link: {e}"))?;
            std::fs::rename(&staged, &paths.bin_path)
                .map_err(|e| format!("install portal app CLI link: {e}"))?;
            agents.write_manifests()?;
            rt.block_on(agents.start_daemon_fresh())
        })();
        if let Err(error) = install_result {
            let _ = rt.block_on(agents.quiesce());
            let _ = std::fs::remove_file(&paths.bin_path);
            std::fs::rename(&backup, &paths.bin_path)
                .map_err(|rollback| format!("{error}; restore standalone CLI: {rollback}"))?;
            agents.write_manifests()?;
            let recovery = rt.block_on(agents.start_daemon_fresh());
            return Err(match recovery {
                Ok(()) => format!("{error}; restored the standalone daemon"),
                Err(recovery) => format!("{error}; restoring daemon failed: {recovery}"),
            });
        }
        std::fs::remove_file(&backup).map_err(|e| format!("remove app-migration backup: {e}"))?;
        runtime_is_bundled = true;
    }

    let daemon_matches = matches!(
        local_client::request(&paths.api_sock, portal_core::localapi::Request::GetState),
        Ok(portal_core::localapi::Response::State { state })
            if !runtime_is_bundled || state.build_sha == BUILD_SHA
    );
    if daemon_matches {
        return Ok(());
    }
    let rt = tokio::runtime::Runtime::new().map_err(|e| e.to_string())?;
    rt.block_on(agents.restart_daemon_fresh())
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
    // Serialize explicit lifecycle commands against install/upgrade. Restart
    // performs a fresh registration (not kickstart), so it also repairs a
    // stale launchd Lightweight Code Requirement after an external swap.
    let _deployment = match deployment::Deployment::acquire(&paths.bin_path) {
        Ok(lock) => lock,
        Err(e) => {
            eprintln!("portal: {e}");
            return 1;
        }
    };
    let runner = OsRunner;
    let agents = services::LoginAgents::new(&runner, paths);
    if !matches!(verb, Verb::Stop)
        && let Err(e) = agents.write_manifests()
    {
        eprintln!("portal: {e}");
        return 1;
    }
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    let result: Result<&str, String> = rt.block_on(async {
        match verb {
            Verb::Start => agents.start_daemon_fresh().await.map(|()| "started"),
            Verb::Restart => agents.start_daemon_fresh().await.map(|()| "restarted"),
            Verb::Stop => {
                let daemon = launchd::Launchd::new(&runner, paths.uid, paths.label.clone());
                match daemon.unload().await {
                    Ok(true) => Ok("stopped"),
                    Ok(false) => Ok("was not running"),
                    Err(e) => Err(e.to_string()),
                }
            }
        }
    });
    match result {
        Ok(message) => {
            println!("portal: {message}");
            0
        }
        Err(e) => {
            eprintln!("portal: {e}");
            1
        }
    }
}

enum DeployCandidate<'a> {
    /// Source/dev install: preserve the source and copy into a staged inode.
    Copy(&'a std::path::Path),
    /// Self-upgrade: move the exact inode that already passed verification.
    Staged(&'a std::path::Path),
}

/// Replace the shared executable only while BOTH LaunchAgents are fully
/// unregistered, then fresh-bootstrap and health-check before committing.
/// Any failed health gate restores the previous binary and starts it again.
fn deploy_binary(
    paths: &Paths,
    candidate: Option<DeployCandidate<'_>>,
) -> Result<services::StartReport, String> {
    let deployment = deployment::Deployment::acquire(&paths.bin_path)?;
    let runner = OsRunner;
    let agents = services::LoginAgents::new(&runner, paths);
    agents.write_manifests()?;
    let rt = tokio::runtime::Runtime::new().map_err(|e| e.to_string())?;
    rt.block_on(async {
        if let Err(quiesce_error) = agents.quiesce().await {
            let recovery = agents.start_fresh().await;
            return Err(match recovery {
                Ok(_) => format!(
                    "could not quiesce login agents for deployment ({quiesce_error}); current binary restarted"
                ),
                Err(recovery_error) => format!(
                    "could not quiesce login agents for deployment ({quiesce_error}); restoring their prior state failed: {recovery_error}"
                ),
            });
        }

        let swap_result = match candidate {
            Some(DeployCandidate::Copy(candidate)) => deployment.swap_copy(candidate).map(Some),
            Some(DeployCandidate::Staged(candidate)) => {
                deployment.swap_staged(candidate).map(Some)
            }
            None => Ok(None),
        };
        let swap = match swap_result {
            Ok(swap) => swap,
            Err(swap_error) => {
                let recovery = agents.start_fresh().await;
                return Err(match recovery {
                    Ok(_) => format!(
                        "binary replacement failed ({swap_error}); previous daemon restored"
                    ),
                    Err(recovery_error) => format!(
                        "binary replacement failed ({swap_error}); restoring daemon also failed: {recovery_error}"
                    ),
                });
            }
        };

        match agents.start_fresh().await {
            Ok(report) => {
                if let Some(swap) = swap {
                    swap.commit().map_err(|e| {
                        format!("new binary is healthy, but finalizing its install failed: {e}")
                    })?;
                }
                Ok(report)
            }
            Err(start_error) => {
                // A rollback is safe only after launchd confirms both jobs are
                // unregistered. Never mutate the executable under a live job.
                if let Err(stop_error) = agents.quiesce().await {
                    return Err(format!(
                        "new binary failed its daemon health gate ({start_error}); could not quiesce it for rollback ({stop_error})"
                    ));
                }
                if let Some(swap) = swap
                    && let Err(rollback_error) = swap.rollback()
                {
                    return Err(format!(
                        "new binary failed its daemon health gate ({start_error}); binary rollback failed: {rollback_error}"
                    ));
                }
                let recovery = agents.start_fresh().await;
                Err(match recovery {
                    Ok(_) => format!(
                        "new binary failed its daemon health gate ({start_error}); previous binary restored and restarted"
                    ),
                    Err(recovery_error) => format!(
                        "new binary failed its daemon health gate ({start_error}); previous binary restored but did not restart: {recovery_error}"
                    ),
                })
            }
        }
    })
}

fn install(paths: &Paths, host: &str, name: Option<String>, index: Option<u8>) -> i32 {
    // Refuse binaries that cannot provision boxes. `make build` and every
    // release path embed both Linux portald architectures.
    let agent = daemon::embedded_agent();
    if agent.linux_amd64.is_none() || agent.linux_arm64.is_none() {
        eprintln!(
            "portal install: this binary has no embedded box agent (portald).\n\
             Build with `make build` or release.sh — or, for a dev daemon,\n\
             set PORTAL_AGENT_AMD64/PORTAL_AGENT_ARM64 to local portald builds."
        );
        return 1;
    }
    if let Err(e) = add_box_to_config(paths, host, name, index) {
        eprintln!("portal install: {e}");
        return 1;
    }
    let self_path = match std::env::current_exe() {
        Ok(path) => path,
        Err(e) => {
            eprintln!("portal install: cannot locate own binary: {e}");
            return 1;
        }
    };
    let candidate =
        (self_path != paths.bin_path).then_some(DeployCandidate::Copy(self_path.as_path()));
    let report = match deploy_binary(paths, candidate) {
        Ok(report) => report,
        Err(e) => {
            eprintln!("portal install: {e}");
            return 1;
        }
    };
    if let Some(warning) = report.tray_warning {
        eprintln!("portal install: {warning} (forwarding is healthy)");
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

fn uninstall(paths: &Paths) -> i32 {
    let _deployment = match deployment::Deployment::acquire(&paths.bin_path) {
        Ok(lock) => lock,
        Err(e) => {
            eprintln!("portal uninstall: {e}");
            return 1;
        }
    };
    let runner = OsRunner;
    let agents = services::LoginAgents::new(&runner, paths);
    let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
    if let Err(e) = rt.block_on(agents.quiesce()) {
        let recovery = rt.block_on(agents.start_fresh());
        eprintln!("portal uninstall: {e}");
        if let Err(recovery_error) = recovery {
            eprintln!("portal uninstall: restoring login agents failed: {recovery_error}");
        }
        return 1;
    }
    let _ = std::fs::remove_file(&paths.tray_plist);
    let _ = std::fs::remove_file(&paths.plist);
    println!(
        "portal: login agents removed (config kept at {})",
        paths.config_dir.display()
    );
    0
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
    println!(
        "portal: adding box {name:?} (remote port p → localhost:p; \
         falls back to {index}0000+p if p is already in use locally)"
    );
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
    if !check {
        let _ = std::fs::remove_file(paths.config_dir.join("app-migration.failed"));
    }
    // Network and cryptographic verification happen while the current daemon
    // remains untouched. Only a fully executable candidate enters deployment.
    let install_dir = paths
        .bin_path
        .parent()
        .expect("installed binary has a parent directory");
    let plan = {
        let rt = tokio::runtime::Runtime::new().expect("tokio runtime");
        let runner = OsRunner;
        let current = format!("v{}", env!("CARGO_PKG_VERSION"));
        rt.block_on(upgrade::prepare(
            &runner,
            install_dir,
            &current,
            check,
            force,
            app_install::app_is_needed(paths),
        ))
    };
    match plan {
        Ok(upgrade::UpgradePlan::NoChange(message)) => {
            println!("portal: {message}");
            0
        }
        Ok(upgrade::UpgradePlan::Candidate(prepared)) => exec_upgrade_helper(prepared),
        Err(e) => {
            eprintln!("portal upgrade: {e}");
            1
        }
    }
}

/// Move execution to a stable hard link BEFORE replacing the installed path.
/// macOS denies child processes from a running executable whose own path was
/// replaced; launchctl must therefore run from this unchanged helper inode.
fn exec_upgrade_helper(prepared: upgrade::PreparedUpgrade) -> i32 {
    use std::os::unix::process::CommandExt as _;

    let current = match std::env::current_exe() {
        Ok(path) => path,
        Err(e) => {
            eprintln!("portal upgrade: cannot locate updater executable: {e}");
            return 1;
        }
    };
    let helper = prepared.staging_dir().join("portal-upgrade-helper");
    if let Err(link_error) = std::fs::hard_link(&current, &helper) {
        // Same-filesystem staging makes hard-link the normal path. Copy is a
        // safe fallback for filesystems that disable links; it still gives the
        // helper a stable pathname distinct from the installation target.
        if let Err(copy_error) = std::fs::copy(&current, &helper) {
            eprintln!(
                "portal upgrade: stage updater helper: hard link failed ({link_error}); copy failed ({copy_error})"
            );
            return 1;
        }
    }

    let mut command = std::process::Command::new(&helper);
    command.arg("_apply-upgrade");
    if prepared.kind == upgrade::ArtifactKind::AppBundle {
        command.arg("--app");
    }
    let error = command
        .arg(prepared.candidate())
        .arg(&prepared.tag)
        .arg(prepared.staging_dir())
        .exec();
    eprintln!("portal upgrade: exec stable updater helper: {error}");
    1
}

fn apply_prepared_upgrade(
    paths: &Paths,
    candidate: &std::path::Path,
    tag: &str,
    staging: &std::path::Path,
    app: bool,
) -> i32 {
    let valid_staging = staging
        .file_name()
        .and_then(|name| name.to_str())
        .is_some_and(|name| name.starts_with("portal-upgrade-"))
        && candidate.starts_with(staging)
        && if app {
            candidate.file_name().and_then(|name| name.to_str()) == Some("Portal.app")
        } else {
            candidate.file_name().and_then(|name| name.to_str()) == Some("portal.new")
        };
    if !valid_staging {
        eprintln!("portal upgrade: invalid staged candidate path");
        return 1;
    }

    let result = if app {
        app_install::install_verified_app(candidate, paths, tag)
    } else {
        deploy_binary(paths, Some(DeployCandidate::Staged(candidate)))
    };
    // `exec` bypassed TempDir::drop. The helper's mapped inode remains valid
    // while its staging pathname is removed.
    let _ = std::fs::remove_dir_all(staging);
    let code = match result {
        Ok(report) => {
            println!("portal: upgraded to {tag}");
            println!(
                "portal: {} installed and daemon health gate passed",
                if app { "Portal.app" } else { "new binary" }
            );
            if let Some(warning) = report.tray_warning {
                eprintln!("portal: {warning} (forwarding is healthy)");
            }
            0
        }
        Err(error) => {
            eprintln!("portal upgrade: {error}");
            1
        }
    };
    cleanup_migration_job();
    code
}

/// Release compatibility binaries use this once: an older `portal upgrade`
/// can only install the standalone asset it knows about, so the newly started
/// daemon launches this stable child to finish the signed app-bundle migration.
fn auto_app_migration_enabled() -> bool {
    option_env!("PORTAL_AUTO_APP_MIGRATION") == Some("1")
}

const MIGRATION_JOB_PREFIX: &str = "local.portal.app-migration.";
const MIGRATION_JOB_ENV: &str = "PORTAL_MIGRATION_JOB_LABEL";

fn migration_submit_command(
    label: &str,
    current: &std::path::Path,
    paths: &Paths,
) -> std::process::Command {
    let mut command = std::process::Command::new("launchctl");
    command
        .args(["submit", "-l"])
        .arg(label)
        .arg("-o")
        .arg(&paths.log)
        .arg("-e")
        .arg(&paths.log)
        .arg("--")
        .arg("/usr/bin/env")
        .arg(format!("HOME={}", paths.home.display()))
        .arg(format!("{MIGRATION_JOB_ENV}={label}"))
        .arg(current)
        .arg("_complete-app-migration");
    command
}

pub(crate) fn spawn_app_migration_if_needed(paths: &Paths) {
    if !auto_app_migration_enabled()
        || std::env::var_os("PORTAL_DISABLE_APP_MIGRATION").is_some()
        || paths.config_dir.join("app-migration.failed").exists()
        || !app_install::app_is_needed(paths)
    {
        return;
    }
    let Ok(current) = std::env::current_exe() else {
        return;
    };
    if app_install::app_from_executable(&current).is_some() {
        return;
    }
    let Ok(installed) = std::fs::canonicalize(&paths.bin_path) else {
        return;
    };
    let Ok(running) = std::fs::canonicalize(&current) else {
        return;
    };
    if installed != running {
        return;
    }

    // A plain child remains part of this launchd service. Booting out the old
    // daemon during installation therefore SIGTERMs that child halfway
    // through migration. Submit a distinct one-shot launchd job instead; it
    // survives daemon/tray replacement and removes itself when finished.
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |duration| duration.as_nanos());
    let label = format!(
        "{MIGRATION_JOB_PREFIX}{}.{}.{nonce}",
        unsafe { libc_getuid() },
        std::process::id()
    );
    let _ = std::fs::create_dir_all(paths.log.parent().unwrap_or(&paths.home));
    let output = migration_submit_command(&label, &current, paths).output();
    match output {
        Ok(output) if output.status.success() => {
            tracing::info!(%label, "scheduled standalone-to-Portal.app migration")
        }
        Ok(output) => tracing::warn!(
            %label,
            error = %String::from_utf8_lossy(&output.stderr).trim(),
            "could not schedule Portal.app migration"
        ),
        Err(error) => tracing::warn!(%label, %error, "could not schedule Portal.app migration"),
    }
}

struct MigrationJobCleanup;

impl Drop for MigrationJobCleanup {
    fn drop(&mut self) {
        cleanup_migration_job();
    }
}

fn cleanup_migration_job() {
    use std::io::Write as _;

    let Some(label) = std::env::var_os(MIGRATION_JOB_ENV) else {
        return;
    };
    let label = label.to_string_lossy();
    if !label.starts_with(MIGRATION_JOB_PREFIX)
        || !label
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-'))
    {
        tracing::warn!(%label, "refusing to remove malformed migration job label");
        return;
    }
    // Flush diagnostics before `launchctl remove` tears down this one-shot
    // service and every process in its coalition.
    let _ = std::io::stdout().flush();
    let _ = std::io::stderr().flush();
    match std::process::Command::new("launchctl")
        .arg("remove")
        .arg(label.as_ref())
        .status()
    {
        // A successful self-removal normally terminates us before `status`
        // returns. These branches cover launchctl failures that leave us live.
        Ok(status) if status.success() => {}
        Ok(status) => tracing::warn!(%status, "could not remove completed migration job"),
        Err(error) => tracing::warn!(%error, "could not remove completed migration job"),
    }
}

fn complete_app_migration(paths: &Paths) -> i32 {
    use fs2::FileExt as _;

    // All ordinary returns clean up the submitted launchd job. A successful
    // updater `exec` bypasses Drop, so `_apply-upgrade` performs the same
    // cleanup after installation and health gating.
    let _migration_job_cleanup = MigrationJobCleanup;
    if !app_install::app_is_needed(paths) {
        return 0;
    }
    if let Err(error) = std::fs::create_dir_all(&paths.config_dir) {
        eprintln!("portal app migration: {error}");
        return 1;
    }
    let lock_path = paths.config_dir.join("app-migration.lock");
    let lock = match std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(&lock_path)
    {
        Ok(lock) => lock,
        Err(error) => {
            eprintln!(
                "portal app migration: open {}: {error}",
                lock_path.display()
            );
            return 1;
        }
    };
    if lock.try_lock_exclusive().is_err() {
        return 0;
    }

    // The older upgrader still owns the deployment lock while it health-checks
    // this daemon. Wait on that condition rather than racing its transaction.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(120);
    loop {
        match deployment::Deployment::acquire(&paths.bin_path) {
            Ok(deployment) => {
                drop(deployment);
                break;
            }
            Err(error) if std::time::Instant::now() < deadline => {
                tracing::debug!(%error, "waiting for compatibility upgrade to finish");
                std::thread::sleep(std::time::Duration::from_millis(250));
            }
            Err(error) => {
                eprintln!("portal app migration: {error}");
                return 1;
            }
        }
    }
    let code = upgrade(paths, false, true);
    if code != 0 && app_install::app_is_needed(paths) {
        let _ = std::fs::write(
            paths.config_dir.join("app-migration.failed"),
            "automatic Portal.app migration failed; run `portal upgrade` to retry\n",
        );
    }
    code
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
            let enabled = state == "on";
            match local_client::request(
                &paths.api_sock,
                portal_core::localapi::Request::SetFeature {
                    name: f.clone(),
                    enabled,
                },
            ) {
                Ok(portal_core::localapi::Response::Ok { .. }) => {
                    println!("{f}: {state} (applied live)");
                    0
                }
                Ok(_) => {
                    eprintln!("portal features: daemon returned an unexpected response");
                    1
                }
                Err(api_error) => {
                    // Preserve the CLI's offline configuration behavior. A
                    // future daemon reads this file on startup; a live daemon
                    // always goes through the API so subscribed UI updates are
                    // event-driven.
                    let _ = std::fs::create_dir_all(&paths.config_dir);
                    match std::fs::write(paths.feature_file(&f), format!("{state}\n")) {
                        Ok(()) => {
                            println!("{f}: {state} (saved; daemon unavailable: {api_error})");
                            0
                        }
                        Err(error) => {
                            eprintln!("portal features: {error}");
                            1
                        }
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod migration_bridge_tests {
    use super::*;

    #[test]
    fn migration_runs_as_a_distinct_launchd_job() {
        let home = tempfile::tempdir().unwrap();
        let paths = Paths::derive(home.path(), 501);
        let command = migration_submit_command(
            "local.portal.app-migration.501.42.7",
            std::path::Path::new("/tmp/portal"),
            &paths,
        );
        assert_eq!(command.get_program(), "launchctl");
        let args: Vec<_> = command
            .get_args()
            .map(|arg| arg.to_string_lossy().into_owned())
            .collect();
        assert_eq!(
            &args[..3],
            ["submit", "-l", "local.portal.app-migration.501.42.7"]
        );
        assert!(args.iter().any(|arg| arg == "/usr/bin/env"));
        assert!(
            args.iter()
                .any(|arg| arg == "PORTAL_MIGRATION_JOB_LABEL=local.portal.app-migration.501.42.7")
        );
        assert_eq!(
            args.last().map(String::as_str),
            Some("_complete-app-migration")
        );
    }
}
