//! Local port-conflict DIAGNOSTICS via `lsof`/`ps`.
//!
//! v2 scope note: v1 also used lsof as the forwarding ground truth (the
//! ControlMaster owned the sockets, so "what is forwarded" had to be
//! reconstructed from its LISTEN set). v2's [`crate::forwarder::
//! ListenerForwarder`] owns the listeners in-process, so lsof is now ONLY
//! consulted after a bind conflict to tell the user WHO holds the port.

use std::sync::Arc;

use crate::runner::Runner;

/// Default absolute path — launchd PATH search is unreliable across macOS
/// variants (same rationale as v1's `app.LsofPath`).
pub const DEFAULT_LSOF_PATH: &str = "/usr/sbin/lsof";

#[derive(Clone)]
pub struct LsofPorts {
    pub path: String,
    pub runner: Arc<dyn Runner>,
}

impl LsofPorts {
    pub fn new(path: impl Into<String>, runner: Arc<dyn Runner>) -> Self {
        Self {
            path: path.into(),
            runner,
        }
    }

    /// Pid of whatever holds a local LISTEN socket on `port`, or None if free
    /// (or lsof failed — same outcome as v1).
    pub async fn local_holder(&self, port: u16) -> Option<u32> {
        let args: Vec<String> = vec![
            "-nP".into(),
            format!("-iTCP:{port}"),
            "-sTCP:LISTEN".into(),
            "-t".into(),
        ];
        let out = self.runner.run(&self.path, &args, b"").await.ok()?;
        out.stdout_lossy()
            .lines()
            .find_map(|l| l.trim().parse::<u32>().ok())
            .filter(|&p| p > 0)
    }

    /// `ps -o comm= -p <pid>` for conflict log messages; empty on failure.
    pub async fn process_name(&self, pid: u32) -> String {
        if pid == 0 {
            return String::new();
        }
        let args: Vec<String> = vec!["-o".into(), "comm=".into(), "-p".into(), pid.to_string()];
        match self.runner.run("ps", &args, b"").await {
            Ok(out) => out.stdout_lossy().trim().to_string(),
            Err(_) => String::new(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runner::FakeRunner;

    #[tokio::test]
    async fn local_holder_parses_first_pid() {
        let fake = Arc::new(FakeRunner::new());
        fake.push_str("\n512\n900\n", "", 0);
        let lsof = LsofPorts::new("/usr/sbin/lsof", fake.clone());
        assert_eq!(lsof.local_holder(8000).await, Some(512));
        let (_, args) = &fake.calls()[0];
        assert_eq!(args, &["-nP", "-iTCP:8000", "-sTCP:LISTEN", "-t"]);
    }

    #[tokio::test]
    async fn failures_yield_none_or_empty() {
        let fake = Arc::new(FakeRunner::new());
        fake.push_err("spawn failed");
        let lsof = LsofPorts::new("/usr/sbin/lsof", fake.clone());
        assert_eq!(lsof.local_holder(8000).await, None);
        assert_eq!(lsof.process_name(0).await, "");
    }

    #[tokio::test]
    async fn process_name_trims() {
        let fake = Arc::new(FakeRunner::new());
        fake.push_str("node\n", "", 0);
        let lsof = LsofPorts::new("/usr/sbin/lsof", fake.clone());
        assert_eq!(lsof.process_name(512).await, "node");
    }
}
