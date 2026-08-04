//! Reusable test doubles: a scriptable `Transport` and an in-memory
//! `PortForwarder`. Kept in the library (not #[cfg(test)]) so dependent
//! crates (portal-core's bootstrap/agent-client/engine tests) can drive them.

use std::collections::{BTreeSet, HashSet, VecDeque};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;

use crate::{
    Desc, ExecOutput, ForwardSpec, Health, PortForwarder, StreamSession, Transport, TransportError,
    TransportImpl,
};

/// Scriptable fake transport. `exec` consumes scripted results FIFO
/// (defaulting to empty success) and records every call; `stream` hands out
/// pre-built sessions (see [`duplex_session`]).
pub struct FakeTransport {
    pub host: String,
    exec_calls: Mutex<Vec<(Vec<String>, Vec<u8>)>>,
    exec_script: Mutex<VecDeque<Result<ExecOutput, String>>>,
    sessions: Mutex<VecDeque<StreamSession>>,
    pub health: Mutex<Health>,
}

impl FakeTransport {
    pub fn new(host: impl Into<String>) -> Arc<Self> {
        Arc::new(Self {
            host: host.into(),
            exec_calls: Mutex::new(Vec::new()),
            exec_script: Mutex::new(VecDeque::new()),
            sessions: Mutex::new(VecDeque::new()),
            health: Mutex::new(Health {
                up: true,
                detail: "connected".into(),
            }),
        })
    }

    pub fn push_exec_ok(&self, stdout: &str) {
        self.exec_script.lock().unwrap().push_back(Ok(ExecOutput {
            stdout: stdout.as_bytes().to_vec(),
            stderr: Vec::new(),
        }));
    }

    pub fn push_exec_err(&self, msg: &str) {
        self.exec_script
            .lock()
            .unwrap()
            .push_back(Err(msg.to_string()));
    }

    pub fn push_session(&self, s: StreamSession) {
        self.sessions.lock().unwrap().push_back(s);
    }

    /// Recorded `(argv, stdin)` tuples for every exec call.
    pub fn exec_calls(&self) -> Vec<(Vec<String>, Vec<u8>)> {
        self.exec_calls.lock().unwrap().clone()
    }
}

#[async_trait]
impl Transport for FakeTransport {
    async fn ensure(&self) -> Result<bool, TransportError> {
        Ok(false)
    }
    async fn health(&self) -> Result<Health, TransportError> {
        Ok(self.health.lock().unwrap().clone())
    }
    async fn exec(&self, stdin: &[u8], argv: &[String]) -> Result<ExecOutput, TransportError> {
        self.exec_calls
            .lock()
            .unwrap()
            .push((argv.to_vec(), stdin.to_vec()));
        match self.exec_script.lock().unwrap().pop_front() {
            Some(Ok(out)) => Ok(out),
            Some(Err(msg)) => Err(TransportError::Io(std::io::Error::other(msg))),
            None => Ok(ExecOutput::default()),
        }
    }
    async fn stream(&self, _argv: &[String]) -> Result<StreamSession, TransportError> {
        self.sessions
            .lock()
            .unwrap()
            .pop_front()
            .ok_or(TransportError::Unimplemented("no scripted session left"))
    }
    async fn close(&self) -> Result<bool, TransportError> {
        Ok(false)
    }
    fn describe(&self) -> Desc {
        Desc {
            impl_kind: TransportImpl::LocalExec,
            host: self.host.clone(),
            endpoint: "fake".into(),
        }
    }
}

/// The far (agent-side) ends of a [`duplex_session`].
pub struct AgentSideIo {
    /// Reads what the client wrote to its stdin.
    pub stdin: tokio::io::DuplexStream,
    /// Writes what the client will read from its stdout.
    pub stdout: tokio::io::DuplexStream,
}

/// Build an in-memory StreamSession + the agent-side pipe ends, so a test can
/// play the remote agent over real framed I/O.
pub fn duplex_session(cap: usize) -> (StreamSession, AgentSideIo) {
    let (client_stdin, agent_stdin) = tokio::io::duplex(cap);
    let (agent_stdout, client_stdout) = tokio::io::duplex(cap);
    let (_stderr_w, client_stderr) = tokio::io::duplex(16); // dropped writer = EOF
    let wait = tokio::spawn(async { Ok(()) });
    (
        StreamSession {
            stdin: Box::new(client_stdin),
            stdout: Box::new(client_stdout),
            stderr: Box::new(client_stderr),
            wait,
        },
        AgentSideIo {
            stdin: agent_stdin,
            stdout: agent_stdout,
        },
    )
}

/// In-memory `PortForwarder` with per-local scripted failures.
#[derive(Default)]
pub struct FakeForwarder {
    pub forwards: Mutex<BTreeSet<ForwardSpec>>,
    /// Locals whose `forward` fails with PortInUse (bind conflict).
    pub busy_locals: Mutex<HashSet<u16>>,
    /// Locals whose `forward` fails with a generic Forward error.
    pub fail_locals: Mutex<HashSet<u16>>,
}

#[async_trait]
impl PortForwarder for FakeForwarder {
    async fn forward(&self, spec: ForwardSpec) -> Result<(), TransportError> {
        if self.busy_locals.lock().unwrap().contains(&spec.local) {
            return Err(TransportError::PortInUse { local: spec.local });
        }
        if self.fail_locals.lock().unwrap().contains(&spec.local) {
            return Err(TransportError::Forward {
                local: spec.local,
                remote: spec.remote,
                stderr: "scripted failure".into(),
            });
        }
        self.forwards.lock().unwrap().insert(spec);
        Ok(())
    }
    async fn cancel(&self, spec: ForwardSpec) -> Result<(), TransportError> {
        self.forwards.lock().unwrap().remove(&spec);
        Ok(())
    }
    async fn list_forwards(&self) -> Result<Vec<ForwardSpec>, TransportError> {
        Ok(self.forwards.lock().unwrap().iter().copied().collect())
    }
}
