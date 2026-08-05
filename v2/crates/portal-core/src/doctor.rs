//! `portal doctor` — per-box end-to-end self-test (port of pkg/doctor's
//! intent, v2 checks): connect → agent handshake state → shims deployed →
//! clipsync convergence → forwards table. Pure over the status snapshot +
//! transport execs so it's testable; the CLI renders the results.

use portal_transport::Transport;

use crate::supervisor::BoxStatus;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Verdict {
    Ok(String),
    Warn(String),
    Fail(String),
}

impl Verdict {
    pub fn is_fail(&self) -> bool {
        matches!(self, Verdict::Fail(_))
    }
    pub fn line(&self) -> String {
        match self {
            Verdict::Ok(m) => format!("  ok   {m}"),
            Verdict::Warn(m) => format!("  warn {m}"),
            Verdict::Fail(m) => format!("  FAIL {m}"),
        }
    }
}

/// Checks that only need the daemon's status snapshot (no box round-trip).
pub fn check_status(st: &BoxStatus) -> Vec<Verdict> {
    let mut out = Vec::new();
    out.push(if st.connected {
        Verdict::Ok(format!(
            "agent connected (sha {})",
            st.agent_sha.as_deref().unwrap_or("?")
        ))
    } else {
        Verdict::Fail("agent not connected (see portal logs)".into())
    });
    out.push(if st.forwards.is_empty() {
        Verdict::Warn("no forwards active (no remote listeners, or not converged yet)".into())
    } else {
        Verdict::Ok(format!(
            "{} forward(s): {}",
            st.forwards.len(),
            st.forwards
                .iter()
                .map(|(l, r)| format!("{r}→localhost:{l}"))
                .collect::<Vec<_>>()
                .join(", ")
        ))
    });
    out.push(if st.clipsync_synced {
        Verdict::Ok(format!(
            "clipsync in sync (change {})",
            st.clipsync_change_id
        ))
    } else if st.clipsync_change_id == 0 {
        Verdict::Warn("clipsync: nothing published yet (copy something on the Mac)".into())
    } else {
        Verdict::Warn(format!(
            "clipsync: change {} not yet acked by the box",
            st.clipsync_change_id
        ))
    });
    out
}

/// Box-side checks over live execs: shims win the PATH race and carry the
/// current version; portald answers; the clip store is readable.
pub async fn check_box(transport: &dyn Transport) -> Vec<Verdict> {
    let mut out = Vec::new();

    // Shims: which(1) must resolve to ~/.local/bin AND carry our marker.
    for shim in ["xclip", "wl-paste", "sudo", "xdg-open"] {
        let script = format!(
            "p=$(command -v {shim} 2>/dev/null) || {{ echo MISSING; exit 0; }}; \
             case \"$p\" in \"$HOME/.local/bin/\"*) ;; *) echo SHADOWED:$p; exit 0;; esac; \
             grep -qF {marker:?} \"$p\" && echo OK || echo STALE",
            marker = format!(
                "{} v{}",
                portald::shims::OWNERSHIP_MARKER,
                portald::shims::VERSION
            ),
        );
        let verdict = match transport
            .exec(b"", &["bash".into(), "-c".into(), shell_quote(&script)])
            .await
        {
            Err(e) => Verdict::Fail(format!("{shim}: probe failed: {e}")),
            Ok(o) => match o.stdout_lossy().trim() {
                "OK" => Verdict::Ok(format!("{shim} shim current")),
                "MISSING" => Verdict::Warn(format!(
                    "{shim}: not on PATH (rc files may not prepend ~/.local/bin)"
                )),
                "STALE" => Verdict::Warn(format!("{shim}: outdated shim (reconnect redeploys)")),
                other => Verdict::Fail(format!("{shim}: {other}")),
            },
        };
        out.push(verdict);
    }

    // portald responds + clip store state (the paste path's ground truth).
    match transport
        .exec(
            b"",
            &[
                "bash".into(),
                "-c".into(),
                shell_quote(
                    "\"$HOME/.cache/portal/portald\" clip status 2>&1 || echo PORTALD-MISSING",
                ),
            ],
        )
        .await
    {
        Err(e) => out.push(Verdict::Fail(format!("portald: {e}"))),
        Ok(o) => {
            let line = o.stdout_lossy().trim().to_string();
            if line.contains("PORTALD-MISSING") {
                out.push(Verdict::Fail(
                    "portald missing on the box (reconnect re-uploads)".into(),
                ));
            } else {
                out.push(Verdict::Ok(format!("box clip store: {line}")));
            }
        }
    }
    out
}

fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_transport::testing::FakeTransport;

    fn st(connected: bool, forwards: Vec<(u16, u16)>, synced: bool, cid: u64) -> BoxStatus {
        BoxStatus {
            name: "devbox1".into(),
            host: "h".into(),
            index: 1,
            connected,
            agent_sha: connected.then(|| "cafe".into()),
            forwards,
            clipsync_synced: synced,
            clipsync_change_id: cid,
        }
    }

    #[test]
    fn status_checks_cover_the_three_axes() {
        let good = check_status(&st(true, vec![(18000, 8000)], true, 5));
        assert!(good.iter().all(|v| !v.is_fail()), "{good:?}");
        assert!(good[1].line().contains("8000→localhost:18000"));

        let bad = check_status(&st(false, vec![], false, 3));
        assert!(bad[0].is_fail());
        assert!(matches!(bad[2], Verdict::Warn(_)));
    }

    #[tokio::test]
    async fn box_checks_classify_shim_and_portald_states() {
        let t = FakeTransport::new("devbox1");
        t.push_exec_ok("OK\n"); // xclip
        t.push_exec_ok("SHADOWED:/usr/bin/wl-paste\n"); // wl-paste
        t.push_exec_ok("MISSING\n"); // sudo
        t.push_exec_ok("kind=Text change_id=4 age=2s sha=ab size=5\n"); // portald
        let out = check_box(&*t).await;
        assert!(matches!(out[0], Verdict::Ok(_)), "{out:?}");
        assert!(out[1].line().contains("SHADOWED") || matches!(out[1], Verdict::Fail(_)));
        assert!(matches!(out[2], Verdict::Warn(_)));
        assert!(out[3].line().contains("change_id=4"));
    }
}
