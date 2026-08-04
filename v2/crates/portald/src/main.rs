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

use portald::agent::{Agent, AgentConfig, watcher};
use portald::cli::{self, Tool};
use portald::cmdsock;
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
            watcher::ProcNetSource,
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

/// `portald keychain run --label L [--env VAR | --stdin] -- cmd args…` and
/// `portald keychain askpass [prompt]`. The secret reaches ONLY the child's
/// env or stdin (askpass: our stdout, which sudo reads) — never argv, disk,
/// or logs. Exit 111 = denied/unavailable (v1 contract).
fn run_keychain(args: &[String]) -> i32 {
    match args.first().map(String::as_str) {
        Some("askpass") => {
            let prompt = args.get(1).cloned().unwrap_or_default();
            let req = portald::cred::CredShimReq {
                label: "sudo".into(),
                requester: format!("pid {}: sudo askpass", std::process::id()),
                mode: "askpass".into(),
                target: prompt,
            };
            match request_secret(&req) {
                Ok(secret) => {
                    use std::io::Write;
                    let mut out = std::io::stdout().lock();
                    let _ = out.write_all(&secret);
                    let _ = out.write_all(b"\n"); // sudo strips the newline
                    0
                }
                Err(reason) => {
                    eprintln!("portal keychain: {}", portald::cred::explain_deny(&reason));
                    111
                }
            }
        }
        Some("run") => {
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
                        eprintln!("portald keychain run: unknown flag {other:?}");
                        return 2;
                    }
                }
            }
            let Some(label) = label else {
                eprintln!(
                    "usage: portald keychain run --label L [--env VAR | --stdin] -- cmd args…"
                );
                return 2;
            };
            if cmd.is_empty()
                || (env_var.is_none() && !use_stdin)
                || (env_var.is_some() && use_stdin)
            {
                eprintln!(
                    "usage: portald keychain run --label L [--env VAR | --stdin] -- cmd args…"
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
                    if use_stdin {
                        use std::io::Write;
                        if let Some(mut stdin) = c.stdin.take() {
                            let _ = stdin.write_all(&secret);
                        } // drop closes: child sees EOF
                    }
                    match c.wait() {
                        Ok(status) => status.code().unwrap_or(1),
                        Err(_) => 1,
                    }
                }
                Err(e) => {
                    eprintln!("portald keychain run: spawn {}: {e}", cmd[0]);
                    127
                }
            }
        }
        _ => {
            eprintln!("usage: portald keychain <run|askpass> …");
            2
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
