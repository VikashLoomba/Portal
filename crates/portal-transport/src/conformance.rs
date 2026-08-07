//! Shared conformance suite. Every
//! `Transport` implementation must pass `exercise`; run it from the impl's
//! own test module. Panics on contract violations (test helper).

use crate::{Transport, TransportError, shell_quote};

fn argv(parts: &[&str]) -> Vec<String> {
    parts.iter().map(|s| s.to_string()).collect()
}

/// Exercise the core exec contract:
/// 1. ensure + health reports up;
/// 2. exec captures stdout;
/// 3. stdin bytes reach the command;
/// 4. non-zero exit surfaces as `Exit` with the code and captured stderr;
/// 5. the shell-join argv contract holds for pre-quoted metacharacters.
pub async fn exercise<T: Transport>(t: &T) {
    t.ensure().await.expect("ensure");
    let h = t.health().await.expect("health");
    assert!(h.up, "health must report up after ensure");

    let out = t
        .exec(b"", &argv(&["echo", "hello"]))
        .await
        .expect("echo exec");
    assert_eq!(out.stdout_lossy(), "hello\n");

    let out = t
        .exec(b"stdin bytes", &argv(&["cat"]))
        .await
        .expect("cat exec");
    assert_eq!(out.stdout, b"stdin bytes");

    // Shell metacharacters must be pre-quoted into a single argv element.
    let quoted = shell_quote("echo err >&2; exit 3");
    match t.exec(b"", &argv(&["sh", "-c", &quoted])).await {
        Err(TransportError::Exit { code, output }) => {
            assert_eq!(code, 3, "exit code must be faithful");
            assert_eq!(output.stderr_lossy().trim(), "err");
        }
        other => panic!("expected Exit error, got {other:?}"),
    }

    let d = t.describe();
    assert!(!d.host.is_empty(), "describe().host must be non-empty");
}
