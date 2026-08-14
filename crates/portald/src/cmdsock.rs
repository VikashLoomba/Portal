//! The box-local cmd socket: `portald notify`/`portald open` (one-shot CLI
//! invocations, e.g. from a Claude Code hook or the xdg-open wrapper) relay
//! events to the RUNNING agent process, which forwards them up the pipe.
//!
//! v1 contract kept: pid-keyed socket path (`~/.cache/portal/cmd-<pid>.sock`,
//! one per live Mac session), 0600 via the 0700 parent dir, tab-framed verb
//! lines, "ok\n"/"rejected\n" answers, default-deny for unknown verbs.

use std::path::{Path, PathBuf};

use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::mpsc;

use crate::agent::Relay;
use portal_proto::messages::Notify;

/// Max accepted line (v1 read whole lines into a 4096 buffer).
const MAX_LINE: usize = 8192;

pub fn sock_dir() -> Option<PathBuf> {
    // Beside the running binary when possible (matches v1: permissions and
    // lifetime follow the cache dir), else $HOME/.cache/portal.
    if let Ok(exe) = std::env::current_exe()
        && let Some(dir) = exe.parent()
    {
        return Some(dir.to_path_buf());
    }
    std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".cache").join("portal"))
}

pub fn sock_path_for(pid: u32, dir: &Path) -> PathBuf {
    dir.join(format!("cmd-{pid}.sock"))
}

/// Bind the agent's cmd socket and serve verb lines, forwarding accepted
/// events into `relay`. Runs until cancelled; removes the socket on exit.
pub async fn serve(
    path: PathBuf,
    relay: mpsc::Sender<Relay>,
    cancel: tokio_util::sync::CancellationToken,
) -> std::io::Result<()> {
    let _ = std::fs::remove_file(&path); // stale from a crashed predecessor
    let listener = UnixListener::bind(&path)?;
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            conn = listener.accept() => {
                let Ok((stream, _)) = conn else { continue };
                let relay = relay.clone();
                tokio::spawn(async move {
                    let _ = handle_conn(stream, relay).await;
                });
            }
        }
    }
    let _ = std::fs::remove_file(&path);
    Ok(())
}

/// Parse one tab-framed verb line. Recognized (default-deny otherwise):
///   `open\t<url>`
///   `notify\t<json>`   — the Notify payload as JSON (portald notify built it)
///   `cred\t<b64 json>` — handled by handle_conn directly (needs the reply)
pub fn parse_verb_line(line: &str) -> Option<Relay> {
    let (verb, rest) = line.split_once('\t')?;
    match verb {
        "open" => {
            let url = rest.trim();
            (is_safe_url(url)).then(|| Relay::OpenUrl(url.to_string()))
        }
        "notify" => {
            let n: NotifyJson = serde_json::from_str(rest).ok()?;
            Some(Relay::Notify(n.into()))
        }
        _ => None,
    }
}

async fn handle_conn(stream: UnixStream, relay: mpsc::Sender<Relay>) -> std::io::Result<()> {
    let (r, mut w) = stream.into_split();
    let mut line = String::new();
    let mut reader = BufReader::new(r).take(MAX_LINE as u64);
    reader.read_line(&mut line).await?;
    let line = line.trim_end_matches(['\r', '\n']);

    // cred is request/response: relay with a oneshot, wait for the Mac's
    // decision (bounded by the Mac-side dialog budget + agent margin).
    if let Some(rest) = line.strip_prefix("cred\t") {
        let answer = match crate::cred::CredShimReq::decode(rest) {
            None => crate::cred::encode_reply_deny("label-invalid"),
            Some(req) => {
                let (tx, rx) = tokio::sync::oneshot::channel();
                if relay.send(Relay::Cred { req, reply: tx }).await.is_err() {
                    crate::cred::encode_reply_deny("no-client")
                } else {
                    match tokio::time::timeout(CRED_WAIT, rx).await {
                        Ok(Ok(Ok(secret))) => crate::cred::encode_reply_ok(&secret),
                        Ok(Ok(Err(reason))) => crate::cred::encode_reply_deny(&reason),
                        _ => crate::cred::encode_reply_deny("timeout"),
                    }
                }
            }
        };
        w.write_all(answer.as_bytes()).await?;
        return Ok(());
    }

    // clipwrite is request/response too: `clipwrite\t<json ClipWriteRequest
    // subset>` (kind/format/sha/size; the blob is already in the store).
    if let Some(rest) = line.strip_prefix("clipwrite\t") {
        let answer = match serde_json::from_str::<ClipWriteJson>(rest) {
            Err(_) => "deny\trejected\n".to_string(),
            Ok(j) => {
                let (tx, rx) = tokio::sync::oneshot::channel();
                let req = portal_proto::messages::ClipWriteRequest {
                    nonce: 0, // minted by the serve loop
                    epoch: 0,
                    kind: j.kind,
                    format: j.format,
                    sha: j.sha,
                    size: j.size,
                };
                if relay
                    .send(Relay::ClipWrite { req, reply: tx })
                    .await
                    .is_err()
                {
                    "deny\tno-client\n".to_string()
                } else {
                    match tokio::time::timeout(CLIPWRITE_WAIT, rx).await {
                        Ok(Ok(Ok(()))) => "ok\n".to_string(),
                        Ok(Ok(Err(reason))) => format!("deny\t{reason}\n"),
                        _ => "deny\ttimeout\n".to_string(),
                    }
                }
            }
        };
        w.write_all(answer.as_bytes()).await?;
        return Ok(());
    }

    let answer = match parse_verb_line(line) {
        Some(relay_ev) => {
            if relay.send(relay_ev).await.is_ok() {
                "ok\n"
            } else {
                "rejected\n"
            }
        }
        None => "rejected\n",
    };
    w.write_all(answer.as_bytes()).await?;
    Ok(())
}

/// Outer bound for an accepted FIFO entry. One Mac decision has a 115-second
/// budget; allow that budget plus margin for every position in the bounded
/// box-side queue so later requests do not expire before reaching the user.
pub const CRED_WAIT: std::time::Duration =
    std::time::Duration::from_secs(130 * crate::agent::MAX_PENDING_CRED_REQUESTS as u64);

/// Outer bound on a clipboard write (Mac pull + pasteboard set is ~instant;
/// generous for big blobs on slow links).
pub const CLIPWRITE_WAIT: std::time::Duration = std::time::Duration::from_secs(30);

/// Only http(s) escapes the box (v1's `open` relay posture: the Mac runs
/// `open <url>`, so file:///.app vectors must never cross).
fn is_safe_url(url: &str) -> bool {
    (url.starts_with("https://") || url.starts_with("http://"))
        && !url.chars().any(|c| c.is_control())
}

/// JSON mirror of the wire Notify (the cmd socket is line-oriented; CBOR
/// doesn't fit; portald builds this from its own CLI flags/hook input).
#[derive(Debug, serde::Serialize, serde::Deserialize)]
pub struct NotifyJson {
    pub title: String,
    #[serde(default)]
    pub body: Option<String>,
    #[serde(default)]
    pub subtitle: Option<String>,
    #[serde(default)]
    pub urgency: u8,
    #[serde(default)]
    pub verified: bool,
    #[serde(default)]
    pub source: Option<String>,
}

/// JSON shape for the clipwrite verb line (`portald clip copy` builds it).
#[derive(Debug, serde::Serialize, serde::Deserialize)]
pub struct ClipWriteJson {
    /// "text" | "image" | "clear"
    pub kind: String,
    #[serde(default)]
    pub format: Option<String>,
    #[serde(default)]
    pub sha: Option<String>,
    #[serde(default)]
    pub size: Option<i64>,
}

impl From<NotifyJson> for Notify {
    fn from(n: NotifyJson) -> Self {
        Notify {
            title: n.title,
            body: n.body,
            subtitle: n.subtitle,
            urgency: Some(n.urgency),
            verified: Some(n.verified),
            source: n.source,
            sound: None,
        }
    }
}

/// Classify a Claude Code hook payload into (title, body, urgency) — the port
/// of v1's ClassifyHookPayload. 0 = completion, 1 = attention, 2 = critical.
pub fn classify_hook(event_name: &str, message: Option<&str>) -> (String, u8) {
    match event_name {
        "Stop" | "SubagentStop" => ("Claude finished".to_string(), 0),
        "Notification" => {
            let msg = message.unwrap_or("");
            if msg.to_lowercase().contains("permission") || msg.to_lowercase().contains("approval")
            {
                ("Claude needs approval".to_string(), 2)
            } else {
                ("Claude needs attention".to_string(), 1)
            }
        }
        other => (format!("Claude: {other}"), 1),
    }
}

/// One-shot client side: send `line` to every cmd-*.sock in `dir`; true if
/// at least one live agent answered ok (multi-Mac: first accept wins for
/// open; notify goes to ALL, matching v1 runOpen/notify fan-out).
pub async fn send_to_agents(dir: &Path, line: &str, all: bool) -> bool {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return false;
    };
    let mut accepted = false;
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if !name.starts_with("cmd-") || !name.ends_with(".sock") {
            continue;
        }
        let Ok(mut stream) = UnixStream::connect(entry.path()).await else {
            continue; // stale socket, agent gone
        };
        let ok = async {
            stream.write_all(line.as_bytes()).await.ok()?;
            stream.write_all(b"\n").await.ok()?;
            stream.shutdown().await.ok()?;
            let mut buf = String::new();
            let mut reader = BufReader::new(stream).take(64);
            reader.read_line(&mut buf).await.ok()?;
            Some(buf.trim() == "ok")
        }
        .await
        .unwrap_or(false);
        if ok {
            accepted = true;
            if !all {
                break;
            }
        }
    }
    accepted
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verb_parsing_is_default_deny() {
        assert_eq!(
            parse_verb_line("open\thttps://example.com/x?y=1"),
            Some(Relay::OpenUrl("https://example.com/x?y=1".into()))
        );
        assert_eq!(parse_verb_line("open\tfile:///etc/passwd"), None);
        assert_eq!(parse_verb_line("open\thttps://e.com/\x1b]evil"), None);
        assert_eq!(parse_verb_line("https://no-verb.example"), None);
        assert_eq!(parse_verb_line("exec\trm -rf /"), None);
        assert_eq!(parse_verb_line(""), None);
    }

    #[test]
    fn notify_json_roundtrip() {
        let line = r#"notify	{"title":"build done","body":"42 tests","urgency":1,"verified":true,"source":"claude_hook"}"#;
        match parse_verb_line(line) {
            Some(Relay::Notify(n)) => {
                assert_eq!(n.title, "build done");
                assert_eq!(n.verified, Some(true));
                assert_eq!(n.urgency, Some(1));
            }
            other => panic!("{other:?}"),
        }
        // Malformed JSON is rejected, not half-parsed.
        assert_eq!(parse_verb_line("notify\t{not json"), None);
    }

    #[test]
    fn hook_classification() {
        assert_eq!(classify_hook("Stop", None), ("Claude finished".into(), 0));
        assert_eq!(
            classify_hook(
                "Notification",
                Some("Claude needs your permission to run Bash")
            ),
            ("Claude needs approval".into(), 2)
        );
        assert_eq!(
            classify_hook("Notification", Some("Claude is waiting for your input")),
            ("Claude needs attention".into(), 1)
        );
        assert_eq!(classify_hook("PreToolUse", None).1, 1);
    }

    #[tokio::test]
    async fn socket_roundtrip_and_fanout() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, mut rx) = mpsc::channel(4);
        let cancel = tokio_util::sync::CancellationToken::new();
        let path = sock_path_for(4242, dir.path());
        let server = tokio::spawn(serve(path.clone(), tx, cancel.clone()));
        // Wait for bind.
        for _ in 0..50 {
            if path.exists() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }

        assert!(send_to_agents(dir.path(), "open\thttps://example.com", false).await);
        assert_eq!(
            rx.recv().await,
            Some(Relay::OpenUrl("https://example.com".into()))
        );
        // Rejected verb answers but does not relay.
        assert!(!send_to_agents(dir.path(), "exec\tls", false).await);
        cancel.cancel();
        let _ = server.await;
    }
}
