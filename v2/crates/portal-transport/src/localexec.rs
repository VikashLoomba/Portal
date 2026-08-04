//! Local-subprocess transport: runs the shell-joined argv under `sh -c` on
//! THIS machine. Used by the conformance suite and same-machine development
//! (port of `pkg/transport/localexec`). Does NOT implement `PortForwarder`.

use std::process::Stdio;

use async_trait::async_trait;
use tokio::io::AsyncWriteExt;
use tokio::process::Command;

use crate::{
    Desc, ExecOutput, Health, StreamSession, Transport, TransportError, TransportImpl, shell_join,
};

#[derive(Debug, Default, Clone, Copy)]
pub struct LocalExec;

#[async_trait]
impl Transport for LocalExec {
    async fn ensure(&self) -> Result<bool, TransportError> {
        Ok(false) // nothing to build; always "up"
    }

    async fn health(&self) -> Result<Health, TransportError> {
        Ok(Health {
            up: true,
            detail: "localexec".into(),
        })
    }

    async fn exec(&self, stdin: &[u8], argv: &[String]) -> Result<ExecOutput, TransportError> {
        if argv.is_empty() {
            return Err(TransportError::EmptyArgv);
        }
        let mut child = Command::new("sh")
            .arg("-c")
            .arg(shell_join(argv))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?;

        if let Some(mut pipe) = child.stdin.take() {
            pipe.write_all(stdin).await?;
            pipe.shutdown().await?;
            drop(pipe);
        }
        let out = child.wait_with_output().await?;
        let output = ExecOutput {
            stdout: out.stdout,
            stderr: out.stderr,
        };
        if !out.status.success() {
            return Err(TransportError::Exit {
                code: out.status.code().unwrap_or(-1),
                output,
            });
        }
        Ok(output)
    }

    async fn stream(&self, argv: &[String]) -> Result<StreamSession, TransportError> {
        if argv.is_empty() {
            return Err(TransportError::EmptyArgv);
        }
        let mut child = Command::new("sh")
            .arg("-c")
            .arg(shell_join(argv))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?;

        let stdin = child.stdin.take().expect("piped stdin");
        let stdout = child.stdout.take().expect("piped stdout");
        let stderr = child.stderr.take().expect("piped stderr");
        let wait = tokio::spawn(async move {
            let status = child.wait().await?;
            if !status.success() {
                return Err(TransportError::Exit {
                    code: status.code().unwrap_or(-1),
                    output: ExecOutput::default(),
                });
            }
            Ok(())
        });

        Ok(StreamSession {
            stdin: Box::new(stdin),
            stdout: Box::new(stdout),
            stderr: Box::new(stderr),
            wait,
        })
    }

    async fn close(&self) -> Result<bool, TransportError> {
        Ok(false)
    }

    fn describe(&self) -> Desc {
        Desc {
            impl_kind: TransportImpl::LocalExec,
            host: "localhost".into(),
            endpoint: "-".into(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::conformance;
    use tokio::io::AsyncReadExt;

    #[tokio::test]
    async fn passes_conformance() {
        conformance::exercise(&LocalExec).await;
    }

    #[tokio::test]
    async fn stream_pipes_and_waits() {
        let t = LocalExec;
        let mut s = t.stream(&["cat".to_string()]).await.expect("stream starts");
        s.stdin.write_all(b"over the pipe").await.unwrap();
        s.stdin.shutdown().await.unwrap();
        drop(s.stdin);
        let mut out = Vec::new();
        s.stdout.read_to_end(&mut out).await.unwrap();
        assert_eq!(out, b"over the pipe");
        s.wait.await.unwrap().unwrap();
    }
}
