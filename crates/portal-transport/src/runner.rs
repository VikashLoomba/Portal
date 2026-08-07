//! Subprocess runner seam. Every ssh/lsof invocation goes through a `Runner`
//! so transports and port-listers are unit-testable with a scripted fake —
//! that is how the empirically-derived ssh gotchas stay encoded in tests
//! instead of in someone's memory.

use std::collections::VecDeque;
use std::io;
use std::process::Stdio;
use std::sync::Mutex;

use async_trait::async_trait;
use tokio::io::AsyncWriteExt;
use tokio::process::Command;

/// Outcome of one completed subprocess. `code` is the exit code (0 = success);
/// spawn failures surface as `Err(io::Error)` from [`Runner::run`], never as a
/// fake code — callers that treat non-zero exits as data (ssh -O verbs) rely
/// on this split.
#[derive(Debug, Clone, Default)]
pub struct RunOutput {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub code: i32,
}

impl RunOutput {
    pub fn stdout_lossy(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }
    pub fn stderr_lossy(&self) -> String {
        String::from_utf8_lossy(&self.stderr).into_owned()
    }
}

#[async_trait]
pub trait Runner: Send + Sync {
    /// Run `program` with `args`, feeding `stdin`, capturing both streams.
    /// Returns `Ok` for ANY exit code; `Err` only when the process could not
    /// be spawned/driven.
    async fn run(&self, program: &str, args: &[String], stdin: &[u8]) -> io::Result<RunOutput>;
}

/// Production runner (tokio subprocess).
#[derive(Debug, Default, Clone, Copy)]
pub struct OsRunner;

#[async_trait]
impl Runner for OsRunner {
    async fn run(&self, program: &str, args: &[String], stdin: &[u8]) -> io::Result<RunOutput> {
        let mut child = Command::new(program)
            .args(args)
            .stdin(if stdin.is_empty() {
                Stdio::null()
            } else {
                Stdio::piped()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?;
        if !stdin.is_empty() {
            let mut pipe = child.stdin.take().expect("piped stdin");
            pipe.write_all(stdin).await?;
            pipe.shutdown().await?;
        }
        let out = child.wait_with_output().await?;
        Ok(RunOutput {
            stdout: out.stdout,
            stderr: out.stderr,
            code: out.status.code().unwrap_or(-1),
        })
    }
}

/// Scripted test double. Responses are consumed in FIFO order; every call's
/// `(program, args)` is recorded for argv assertions. Exhausting the script
/// returns an empty success (code 0) so incidental trailing calls don't panic.
#[derive(Debug, Default)]
pub struct FakeRunner {
    calls: Mutex<Vec<(String, Vec<String>)>>,
    script: Mutex<VecDeque<Result<RunOutput, String>>>,
}

impl FakeRunner {
    pub fn new() -> Self {
        Self::default()
    }

    /// Queue a scripted response.
    pub fn push(&self, out: RunOutput) {
        self.script.lock().unwrap().push_back(Ok(out));
    }

    /// Convenience: queue `(stdout, stderr, code)`.
    pub fn push_str(&self, stdout: &str, stderr: &str, code: i32) {
        self.push(RunOutput {
            stdout: stdout.as_bytes().to_vec(),
            stderr: stderr.as_bytes().to_vec(),
            code,
        });
    }

    /// Queue a spawn failure.
    pub fn push_err(&self, msg: &str) {
        self.script.lock().unwrap().push_back(Err(msg.to_string()));
    }

    /// All `(program, argv)` tuples observed so far.
    pub fn calls(&self) -> Vec<(String, Vec<String>)> {
        self.calls.lock().unwrap().clone()
    }
}

#[async_trait]
impl Runner for FakeRunner {
    async fn run(&self, program: &str, args: &[String], _stdin: &[u8]) -> io::Result<RunOutput> {
        self.calls
            .lock()
            .unwrap()
            .push((program.to_string(), args.to_vec()));
        match self.script.lock().unwrap().pop_front() {
            Some(Ok(out)) => Ok(out),
            Some(Err(msg)) => Err(io::Error::other(msg)),
            None => Ok(RunOutput::default()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn os_runner_captures_streams_and_code() {
        let out = OsRunner
            .run(
                "sh",
                &["-c".into(), "printf out; printf err >&2; exit 4".into()],
                b"",
            )
            .await
            .unwrap();
        assert_eq!(out.stdout, b"out");
        assert_eq!(out.stderr, b"err");
        assert_eq!(out.code, 4);
    }

    #[tokio::test]
    async fn os_runner_feeds_stdin() {
        let out = OsRunner.run("cat", &[], b"feed me").await.unwrap();
        assert_eq!(out.stdout, b"feed me");
        assert_eq!(out.code, 0);
    }

    #[tokio::test]
    async fn os_runner_spawn_failure_is_err() {
        assert!(
            OsRunner
                .run("/nonexistent-portal-test-binary", &[], b"")
                .await
                .is_err()
        );
    }
}
