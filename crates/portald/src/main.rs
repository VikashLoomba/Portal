//! `portald` — the box-side agent binary.
//!
//! ```text
//! portald [--proto-version=N]                agent mode (stdio RPC; run over ssh)
//! portald clip paste|targets|status          local clipboard reads (shims)
//! portald blob put <sha256> <size>           blob push landing point (stdin)
//! portald notify --hook | --title <t> [...]  relay a notification to the Mac
//! portald open <url>                         relay a URL open to the Mac
//! portald --sha                              embedded build SHA
//! ```
//!
//! Agent mode: stdout is EXCLUSIVELY protocol frames; logs go to stderr
//! (the Mac tees them into its daemon log, "agent:"-prefixed).

use std::io::Write as _;

use portald::agent::{Agent, AgentConfig, watcher};
use portald::cli::{self, Tool};
use portald::cmdsock;
use portald::cred::AskpassAction;
use portald::store::{ClipKind, ClipStore};

fn git_sha() -> &'static str {
    option_env!("PORTAL_GIT_SHA").unwrap_or("dev")
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let code = dispatch(&args);
    std::process::exit(code);
}

fn dispatch(args: &[String]) -> i32 {
    // Stdio locks are taken PER ARM, never across `run_agent`: agent mode
    // writes frames via tokio::io::stdout(), whose blocking-pool threads
    // acquire the same global ReentrantMutex — a lock held here by the main
    // thread deadlocks every frame write (the reentrant lock only re-enters
    // on the owning thread).
    match args.first().map(String::as_str) {
        Some("--sha") => {
            println!("{}", git_sha());
            0
        }
        Some("clip") => {
            let mut stdout = std::io::stdout().lock();
            let mut stderr = std::io::stderr().lock();
            let Some(store) = open_store(&mut stderr) else {
                return 1;
            };
            match args.get(1).map(String::as_str) {
                Some("paste") => {
                    let mut want = ClipKind::Text;
                    let mut trim = false;
                    let mut rest = args[2..].iter();
                    while let Some(a) = rest.next() {
                        match a.as_str() {
                            "--trim" => trim = true,
                            "--type" => match rest.next().map(String::as_str) {
                                Some("text") => want = ClipKind::Text,
                                Some("image/png") => want = ClipKind::Image,
                                other => return usage(&mut stderr, &format!("--type {other:?}")),
                            },
                            other => return usage(&mut stderr, other),
                        }
                    }
                    cli::clip_paste(&store, want, trim, &mut stdout, &mut stderr)
                }
                Some("copy") => {
                    let mut want = ClipKind::Text;
                    let mut trim = false;
                    let mut empty_clears = false;
                    let mut rest = args[2..].iter();
                    while let Some(a) = rest.next() {
                        match a.as_str() {
                            "--trim" => trim = true,
                            "--empty-clears" => empty_clears = true,
                            "--type" => match rest.next().map(String::as_str) {
                                Some("text") => want = ClipKind::Text,
                                Some("image/png") => want = ClipKind::Image,
                                other => return usage(&mut stderr, &format!("--type {other:?}")),
                            },
                            other => return usage(&mut stderr, other),
                        }
                    }
                    let mut send = |line: &str| -> Result<(), String> {
                        let Some(dir) = cmdsock::sock_dir() else {
                            return Err("no-client".into());
                        };
                        let rt = tokio::runtime::Builder::new_current_thread()
                            .enable_all()
                            .build()
                            .map_err(|_| "no-client".to_string())?;
                        rt.block_on(async {
                            if cmdsock::send_to_agents(&dir, line, false).await {
                                Ok(())
                            } else {
                                Err("denied or no client".into())
                            }
                        })
                    };
                    cli::clip_copy(
                        &store,
                        want,
                        trim,
                        empty_clears,
                        &mut std::io::stdin().lock(),
                        &mut stderr,
                        &mut send,
                    )
                }
                Some("targets") => {
                    let tool = match args.get(2).map(String::as_str) {
                        Some("wl-paste") => Tool::WlPaste,
                        Some("xclip") | None => Tool::Xclip,
                        Some(other) => return usage(&mut stderr, other),
                    };
                    cli::clip_targets(&store, tool, &mut stdout, &mut stderr)
                }
                Some("status") => {
                    let now = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map(|d| d.as_secs())
                        .unwrap_or(0);
                    cli::clip_status(&store, now, &mut stdout)
                }
                other => usage(&mut stderr, &format!("clip {other:?}")),
            }
        }
        Some("blob") => {
            let mut stderr = std::io::stderr().lock();
            if args.get(1).map(String::as_str) != Some("put") {
                return usage(&mut stderr, "blob");
            }
            let (Some(sha), Some(size)) = (args.get(2), args.get(3)) else {
                return usage(&mut stderr, "blob put");
            };
            let Ok(size) = size.parse::<u64>() else {
                return usage(&mut stderr, "blob put <size>");
            };
            let Some(store) = open_store(&mut stderr) else {
                return 1;
            };
            cli::blob_put(&store, sha, size, &mut std::io::stdin().lock(), &mut stderr)
        }
        Some("notify") => run_notify(&args[1..]),
        Some("open") => run_open(&args[1..]),
        Some("keychain") => run_keychain(&args[1..]),
        Some("help" | "-h" | "--help") => {
            print!("{TOP_LEVEL_HELP}");
            0
        }
        Some(other) if !other.starts_with('-') => usage(&mut std::io::stderr().lock(), other),
        _ => run_agent(args),
    }
}

/// Agent mode: serve the v4 protocol on stdio + the cmd socket for
/// notify/open relays.
fn run_agent(args: &[String]) -> i32 {
    // Logs to stderr ONLY (stdout is the frame pipe).
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    // --proto-version=N (v1 flag shape): honored via handshake validation;
    // a mismatch is answered in-band as a fatal AgentError.
    let _requested: Option<u32> = args
        .iter()
        .find_map(|a| a.strip_prefix("--proto-version="))
        .and_then(|v| v.parse().ok());

    let Some(store_dir) = ClipStore::default_dir() else {
        eprintln!("portald: HOME is not set");
        return 1;
    };
    let Some(sock_dir) = cmdsock::sock_dir() else {
        eprintln!("portald: cannot derive cmd socket dir");
        return 1;
    };

    let runtime = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("portald: runtime: {e}");
            return 1;
        }
    };
    runtime.block_on(async move {
        let cancel = tokio_util::sync::CancellationToken::new();
        let (relay_tx, relay_rx) = tokio::sync::mpsc::channel(16);

        let sock_path = cmdsock::sock_path_for(std::process::id(), &sock_dir);
        let sock_task = tokio::spawn(cmdsock::serve(sock_path.clone(), relay_tx, cancel.clone()));

        let cfg = AgentConfig {
            git_sha: git_sha().to_string(),
            kernel: read_kernel(),
            boot_id: watcher::boot_id(),
            ephem: watcher::ephemeral_range(),
            ..AgentConfig::default()
        };
        let mut agent = Agent::new(
            cfg,
            watcher::ProcNetSource::default(),
            ClipStore::new(store_dir),
            relay_rx,
        );
        let mut stdout = tokio::io::stdout();
        let result = agent.serve(tokio::io::stdin(), &mut stdout).await;

        cancel.cancel();
        let _ = sock_task.await;
        match result {
            Ok(()) => 0,
            Err(e) => {
                eprintln!("portald: session ended: {e}");
                2
            }
        }
    })
}

/// `portald notify --hook` (Claude Code hook JSON on stdin, verified) or
/// `portald notify --title <t> [--body <b>] [--subtitle <s>] [--urgency N]`
/// (generic, unverified). Exit 0 iff a client accepted it.
fn run_notify(args: &[String]) -> i32 {
    let json = if args.first().map(String::as_str) == Some("--hook") {
        let mut input = String::new();
        use std::io::Read;
        if std::io::stdin().read_to_string(&mut input).is_err() {
            eprintln!("portald notify: cannot read hook payload");
            return 1;
        }
        let hook: serde_json::Value = match serde_json::from_str(&input) {
            Ok(v) => v,
            Err(e) => {
                eprintln!("portald notify: bad hook JSON: {e}");
                return 1;
            }
        };
        let event = hook
            .get("hook_event_name")
            .and_then(|v| v.as_str())
            .unwrap_or("unknown");
        let message = hook.get("message").and_then(|v| v.as_str());
        let (title, urgency) = cmdsock::classify_hook(event, message);
        cmdsock::NotifyJson {
            title,
            body: message.map(str::to_string),
            subtitle: None,
            urgency,
            verified: true, // structured hook entrypoint = trusted (v1)
            source: Some("claude_hook".into()),
        }
    } else {
        let mut title = None;
        let mut body = None;
        let mut subtitle = None;
        let mut urgency = 0u8;
        let mut it = args.iter();
        while let Some(a) = it.next() {
            match a.as_str() {
                "--title" => title = it.next().cloned(),
                "--body" => body = it.next().cloned(),
                "--subtitle" => subtitle = it.next().cloned(),
                "--urgency" => urgency = it.next().and_then(|v| v.parse().ok()).unwrap_or(0),
                other => {
                    eprintln!("portald notify: unknown flag {other:?}");
                    return 2;
                }
            }
        }
        let Some(title) = title else {
            eprintln!(
                "usage: portald notify --hook | --title <t> [--body <b>] [--subtitle <s>] [--urgency N]"
            );
            return 2;
        };
        cmdsock::NotifyJson {
            title,
            body,
            subtitle,
            urgency,
            verified: false, // generic entrypoint = [unverified] on the Mac
            source: Some("generic".into()),
        }
    };
    let Ok(payload) = serde_json::to_string(&json) else {
        return 1;
    };
    relay_line(&format!("notify\t{payload}"), true)
}

/// `portald open <url>` — first accepting client wins (v1 semantics).
fn run_open(args: &[String]) -> i32 {
    if args.is_empty() {
        eprintln!("usage: portald open <url>");
        return 2;
    }
    let url = args.join(" ");
    relay_line(&format!("open\t{url}"), false)
}

fn relay_line(line: &str, all: bool) -> i32 {
    let Some(dir) = cmdsock::sock_dir() else {
        return 1;
    };
    let runtime = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(_) => return 1,
    };
    if runtime.block_on(cmdsock::send_to_agents(&dir, line, all)) {
        0
    } else {
        1 // no client: hook callers detect non-delivery; xdg-open falls through
    }
}

const TOP_LEVEL_HELP: &str = r#"portal is the agent-facing command installed on this dev box.

Secure inputs:
  portal keychain run --label <description> --env NAME -- <command> [args...]
  portal keychain run --label <description> --stdin -- <command> [args...]

The approved secret is delivered only to the child command's environment or
stdin; it is never printed for the calling agent. For examples and the quoting
rules, run:
  portal keychain --help

Other box-side commands:
  portal clip <paste|copy|targets|status> ...
  portal notify --hook | --title <title> [options]
  portal open <url>

Do not run `portal keychain askpass` directly. sudo invokes that helper and is
the only process that should read its secret-bearing stdout.
"#;

const KEYCHAIN_HELP: &str = r#"portal keychain requests a credential from the connected Mac without printing it to the agent.

Usage:
  portal keychain run --label <L> --env NAME -- <command> [args...]
  portal keychain run --label <L> --stdin -- <command> [args...]

Examples:
  portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" ...'
  portal keychain run --label "registry token" --stdin -- docker login --password-stdin

The SINGLE quotes in the first example make the child shell expand $PW. The
caller's shell must not expand it. A denied or unavailable request exits 111.

`portal keychain askpass` is an internal sudo/ssh helper. Do not run it to
request a secret; use `portal keychain run` instead.
"#;

const KEYCHAIN_RUN_HELP: &str = r#"Request a credential, then deliver it only to a child process.

Usage:
  portal keychain run --label <L> --env NAME -- <command> [args...]
  portal keychain run --label <L> --stdin -- <command> [args...]

Examples:
  portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" ...'
  portal keychain run --label "database password" --stdin -- psql

The SINGLE quotes in the first example make the child shell expand $PW. The
calling shell and agent never receive the approved secret.
"#;

const ASKPASS_HELP: &str = r#"portal keychain askpass is the SUDO_ASKPASS helper sudo calls for you. It is not meant to be run by hand.

On approval it writes the secret to stdout for sudo to read. To prevent that
secret from entering an agent transcript, portald refuses unless sudo, sudoedit,
sudo-rs, or ssh invoked it.

To exercise the credential path deliberately, use:
  portal keychain run --label "test" --env PW -- sh -c 'echo "len=${#PW}"'

Set PORTAL_ASKPASS_ALLOW_ANY=1 only when wiring this helper into another
askpass-protocol consumer that reads its stdout.
"#;

fn is_help_arg(arg: &str) -> bool {
    matches!(arg, "help" | "-h" | "--help")
}

/// `portal keychain run --label L [--env VAR | --stdin] -- cmd args…` and
/// the internal `portal keychain askpass [prompt]`. Direct run mode keeps the
/// secret inside the child; askpass is separately gated to a known consumer
/// before a request can reach the Mac.
fn run_keychain(args: &[String]) -> i32 {
    match args.first().map(String::as_str) {
        Some(help) if is_help_arg(help) => {
            print!("{KEYCHAIN_HELP}");
            0
        }
        Some("askpass") => {
            let askpass_args = &args[1..];
            let action = portald::cred::current_askpass_action(askpass_args);
            let requester = portald::cred::requester_context();
            let mut request = request_secret;
            run_keychain_askpass(
                askpass_args,
                action,
                requester,
                &mut request,
                &mut std::io::stdout().lock(),
                &mut std::io::stderr().lock(),
            )
        }
        Some("run") => {
            if args.len() == 2 && is_help_arg(&args[1]) {
                print!("{KEYCHAIN_RUN_HELP}");
                return 0;
            }
            let mut label = None;
            let mut env_var: Option<String> = None;
            let mut use_stdin = false;
            let mut cmd: Vec<String> = Vec::new();
            let mut it = args[1..].iter();
            while let Some(a) = it.next() {
                match a.as_str() {
                    "--label" => label = it.next().cloned(),
                    "--env" => env_var = it.next().cloned(),
                    "--stdin" => use_stdin = true,
                    "--" => {
                        cmd = it.cloned().collect();
                        break;
                    }
                    other => {
                        eprintln!("portal keychain run: unknown flag {other:?}");
                        return 2;
                    }
                }
            }
            let Some(label) = label else {
                eprintln!(
                    "usage: portal keychain run --label L [--env VAR | --stdin] -- cmd args…"
                );
                return 2;
            };
            if cmd.is_empty()
                || (env_var.is_none() && !use_stdin)
                || (env_var.is_some() && use_stdin)
            {
                eprintln!(
                    "usage: portal keychain run --label L [--env VAR | --stdin] -- cmd args…"
                );
                return 2;
            }
            let req = portald::cred::CredShimReq {
                label,
                requester: format!("pid {}: {}", std::process::id(), cmd.join(" ")),
                mode: if use_stdin {
                    "stdin".into()
                } else {
                    "env".into()
                },
                target: env_var.clone().unwrap_or_default(),
            };
            let secret = match request_secret(&req) {
                Ok(s) => s,
                Err(reason) => {
                    eprintln!("portal keychain: {}", portald::cred::explain_deny(&reason));
                    return 111;
                }
            };
            let mut child = std::process::Command::new(&cmd[0]);
            child.args(&cmd[1..]);
            if let Some(var) = env_var {
                child.env(var, String::from_utf8_lossy(&secret).into_owned());
            }
            if use_stdin {
                child.stdin(std::process::Stdio::piped());
            }
            match child.spawn() {
                Ok(mut c) => {
                    if use_stdin && let Some(mut stdin) = c.stdin.take() {
                        let _ = stdin.write_all(&secret);
                    } // drop closes: child sees EOF
                    match c.wait() {
                        Ok(status) => status.code().unwrap_or(1),
                        Err(_) => 1,
                    }
                }
                Err(e) => {
                    eprintln!("portal keychain run: spawn {}: {e}", cmd[0]);
                    127
                }
            }
        }
        _ => {
            eprintln!("usage: portal keychain <run|askpass> …");
            eprintln!("run 'portal keychain --help' for secure-input examples");
            2
        }
    }
}

/// Safety-critical askpass path with an injectable request seam. Help and
/// refusal return before `request` is called, which guarantees they cannot
/// open a Mac prompt or put an approved secret in stdout.
fn run_keychain_askpass(
    args: &[String],
    action: AskpassAction,
    requester: String,
    request: &mut dyn FnMut(&portald::cred::CredShimReq) -> Result<Vec<u8>, String>,
    stdout: &mut dyn std::io::Write,
    stderr: &mut dyn std::io::Write,
) -> i32 {
    match action {
        AskpassAction::Help => {
            let _ = write!(stdout, "{ASKPASS_HELP}");
            return 0;
        }
        AskpassAction::Refuse => {
            let _ = writeln!(
                stderr,
                "portal keychain askpass: refusing to request a credential — this helper writes the approved secret to stdout and must be invoked by sudo or ssh"
            );
            let _ = writeln!(
                stderr,
                "portal keychain askpass: use 'portal keychain run --help' for a safe direct request"
            );
            return 2;
        }
        AskpassAction::Request => {}
    }

    let prompt = portald::cred::truncate_utf8(&args.join(" "), portald::cred::CONTEXT_MAX);
    let req = portald::cred::CredShimReq {
        label: "sudo".into(),
        requester,
        mode: "askpass".into(),
        target: prompt,
    };
    match request(&req) {
        Ok(secret) => {
            if stdout.write_all(&secret).is_err() || stdout.write_all(b"\n").is_err() {
                let _ = writeln!(stderr, "portal keychain: could not write to askpass stdout");
                return 111;
            }
            0
        }
        Err(reason) => {
            let _ = writeln!(
                stderr,
                "portal keychain: {}",
                portald::cred::explain_deny(&reason)
            );
            111
        }
    }
}

/// Ask the running agent (cmd socket) for the secret: first LIVE socket wins
/// (stale sockets fail to connect — the v2 shape of v1's single-agent rule).
fn request_secret(req: &portald::cred::CredShimReq) -> Result<Vec<u8>, String> {
    let Some(dir) = portald::cmdsock::sock_dir() else {
        return Err("no-client".into());
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|_| "no-client".to_string())?;
    rt.block_on(async {
        use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
        let entries = std::fs::read_dir(&dir).map_err(|_| "no-client".to_string())?;
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if !name.starts_with("cmd-") || !name.ends_with(".sock") {
                continue;
            }
            let Ok(mut stream) = tokio::net::UnixStream::connect(entry.path()).await else {
                continue; // stale socket
            };
            let line = req.encode_line();
            if stream.write_all(line.as_bytes()).await.is_err()
                || stream.write_all(b"\n").await.is_err()
            {
                continue;
            }
            let mut reply = String::new();
            let mut reader = BufReader::new(stream);
            match tokio::time::timeout(portald::cmdsock::CRED_WAIT, reader.read_line(&mut reply))
                .await
            {
                Ok(Ok(_)) => return portald::cred::parse_reply(&reply),
                _ => return Err("timeout".into()),
            }
        }
        Err("no-client".into())
    })
}

fn read_kernel() -> String {
    std::process::Command::new("uname")
        .arg("-sr")
        .output()
        .ok()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_default()
}

fn open_store(stderr: &mut dyn std::io::Write) -> Option<ClipStore> {
    match ClipStore::default_dir() {
        Some(dir) => Some(ClipStore::new(dir)),
        None => {
            let _ = writeln!(stderr, "portald: HOME is not set");
            None
        }
    }
}

fn usage(stderr: &mut dyn std::io::Write, what: &str) -> i32 {
    let _ = writeln!(
        stderr,
        "portald: unknown/invalid arguments near {what:?}\n\
         usage: portald [--proto-version=N]                     (agent mode)\n\
         \x20      portald clip <paste [--type text|image/png] [--trim] | targets [xclip|wl-paste] | status>\n\
         \x20      portald blob put <sha256> <size>              (bytes on stdin)\n\
         \x20      portald notify --hook | --title <t> [options]\n\
         \x20      portald open <url>\n\
         \x20      portald --sha"
    );
    2
}

#[cfg(test)]
mod tests {
    use super::*;

    fn strings(values: &[&str]) -> Vec<String> {
        values.iter().map(|value| (*value).to_string()).collect()
    }

    /// End-to-end through the safety-critical helper: neither a help probe nor
    /// a refused direct call may invoke the request closure or write a secret.
    #[test]
    fn askpass_help_and_refusal_cannot_request_or_disclose() {
        for (args, action, expected_code) in [
            (strings(&["--help"]), AskpassAction::Help, 0),
            (strings(&["Password:"]), AskpassAction::Refuse, 2),
        ] {
            let mut requests = 0;
            let mut request = |_req: &portald::cred::CredShimReq| {
                requests += 1;
                Ok(b"MUST-NOT-BE-DISCLOSED".to_vec())
            };
            let mut stdout = Vec::new();
            let mut stderr = Vec::new();
            let code = run_keychain_askpass(
                &args,
                action,
                "pid 7: test".into(),
                &mut request,
                &mut stdout,
                &mut stderr,
            );
            assert_eq!(code, expected_code);
            assert_eq!(requests, 0);
            assert!(
                !stdout
                    .windows(b"MUST-NOT-BE-DISCLOSED".len())
                    .any(|w| { w == b"MUST-NOT-BE-DISCLOSED" })
            );
        }
    }

    #[test]
    fn askpass_request_writes_only_for_an_allowed_consumer() {
        let mut seen = None;
        let mut request = |req: &portald::cred::CredShimReq| {
            seen = Some(req.clone());
            Ok(b"approved".to_vec())
        };
        let mut stdout = Vec::new();
        let mut stderr = Vec::new();
        let code = run_keychain_askpass(
            &strings(&["[sudo] password for dev:"]),
            AskpassAction::Request,
            "pid 42: sudo -A whoami".into(),
            &mut request,
            &mut stdout,
            &mut stderr,
        );
        assert_eq!(code, 0);
        assert_eq!(stdout, b"approved\n");
        assert!(stderr.is_empty());
        let req = seen.unwrap();
        assert_eq!(req.mode, "askpass");
        assert_eq!(req.target, "[sudo] password for dev:");
        assert_eq!(req.requester, "pid 42: sudo -A whoami");
    }

    #[test]
    fn agent_facing_help_explains_the_non_disclosing_path() {
        assert!(TOP_LEVEL_HELP.contains("portal keychain run"));
        assert!(TOP_LEVEL_HELP.contains("never printed for the calling agent"));
        assert!(KEYCHAIN_HELP.contains("SINGLE quotes"));
        assert!(ASKPASS_HELP.contains("not meant to be run by hand"));
    }
}
