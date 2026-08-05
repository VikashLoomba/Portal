//! Embedded-agent → remote-cache lifecycle (port of internal/bootstrap).
//!
//! 1. Probe `~/.cache/portal/agent-<git-sha>` by BOTH byte-count and sha256
//!    (length-only verification was insufficient in v1's history: a same-size
//!    truncated leftover or swapped file must not bypass re-upload).
//! 2. If missing/mismatched, upload ATOMICALLY: unique .tmp + byte-count
//!    assert + rename, with an EXIT trap cleaning the .tmp on any abnormal
//!    end. The previous binary stays intact until the final mv.
//! 3. Maintain the stable `portald` symlink and prune stale agents.
//!
//! The uname arch probe is cached, keyed by the agent-reported boot id: a
//! reboot (or a first-ever probe) re-probes, a plain reconnect does not.

use std::sync::{Arc, Mutex};

use portal_transport::{Transport, TransportError};
use sha2::{Digest, Sha256};

use crate::agentclient::session::Bootstrapper;

pub const REMOTE_DIR: &str = "~/.cache/portal";

/// The per-arch agent binaries embedded in the Mac binary (include_bytes! in
/// portal-cli once the Rust portald ships; until then the Go-built portald
/// bytes can be embedded for a mixed-version rollout).
#[derive(Clone)]
pub struct EmbeddedAgent {
    /// 40-hex git commit SHA — keys the remote path AND the HelloAck SHA
    /// match, so it MUST equal the `main.gitSHA` the agent binary reports.
    pub git_sha: String,
    pub linux_amd64: Option<Arc<[u8]>>,
    pub linux_arm64: Option<Arc<[u8]>>,
}

impl EmbeddedAgent {
    /// Select bytes for a `uname -sm` output. Errors on non-Linux or an arch
    /// we did not embed.
    pub fn select(&self, uname_sm: &str) -> Result<Arc<[u8]>, String> {
        let tokens: Vec<&str> = uname_sm.split_whitespace().collect();
        let arch = match tokens.as_slice() {
            ["Linux", "x86_64"] => self.linux_amd64.clone(),
            ["Linux", "aarch64"] | ["Linux", "arm64"] => self.linux_arm64.clone(),
            _ => return Err(format!("unsupported dev box platform {uname_sm:?}")),
        };
        arch.ok_or_else(|| format!("no embedded agent for {uname_sm:?}"))
    }
}

#[derive(Default)]
struct ProbeState {
    selected: Option<Arc<[u8]>>,
    probed_boot: String,
    boot: String,
}

pub struct Manager {
    transport: Arc<dyn Transport>,
    agent: EmbeddedAgent,
    state: Mutex<ProbeState>,
}

impl Manager {
    pub fn new(transport: Arc<dyn Transport>, agent: EmbeddedAgent) -> Self {
        Self {
            transport,
            agent,
            state: Mutex::new(ProbeState::default()),
        }
    }

    async fn selected_bytes(&self) -> Result<Arc<[u8]>, String> {
        {
            let mut st = self.state.lock().unwrap();
            // An UNKNOWN probe boot id adopts the first later non-empty boot id
            // without re-probing; only a change between two KNOWN ids (a
            // reboot) invalidates the cached arch (v1 semantics).
            let reprobe = st.selected.is_none()
                || (!st.probed_boot.is_empty() && !st.boot.is_empty() && st.boot != st.probed_boot);
            if !reprobe {
                if st.probed_boot.is_empty() && !st.boot.is_empty() {
                    st.probed_boot = st.boot.clone();
                }
                return Ok(st.selected.clone().unwrap());
            }
        }
        let out = self
            .transport
            .exec(b"", &["uname".into(), "-sm".into()])
            .await
            .map_err(|e| format!("uname probe: {e}"))?;
        let uname = out.stdout_lossy().trim().to_string();
        let bytes = self.agent.select(&uname)?;
        let mut st = self.state.lock().unwrap();
        st.selected = Some(bytes.clone());
        st.probed_boot = st.boot.clone();
        Ok(bytes)
    }

    async fn bash(&self, script: String, stdin: &[u8]) -> Result<String, TransportError> {
        let out = self
            .transport
            .exec(stdin, &["bash".into(), "-c".into(), shell_quote(&script)])
            .await?;
        Ok(out.stdout_lossy())
    }
}

/// Probe printing a single `"<size> <digest>"` line or `MISSING`. Portable
/// sha256: sha256sum (Linux), sha256 -q (BSD/macOS), openssl as last resort.
pub fn probe_script(remote_path: &str) -> String {
    let p = remote_path;
    format!(
        "test -x {p} && printf '%s %s' \"$(stat -c %s {p} 2>/dev/null || stat -f %z {p})\" \
         \"$(sha256sum {p} 2>/dev/null | awk '{{print $1}}' || sha256 -q {p} 2>/dev/null || \
         openssl dgst -sha256 -hex {p} 2>/dev/null | awk '{{print $NF}}')\" || echo MISSING"
    )
}

/// Atomic upload: unique tmp + EXIT trap + byte-count assert + rename.
pub fn upload_script(remote_path: &str, size: usize) -> String {
    format!(
        "set -e; install -d -m 0700 {dir} && tmp=$(mktemp {dir}/.agent.tmp.XXXXXX) && \
         trap 'rm -f \"$tmp\"' EXIT && cat > \"$tmp\" && [ \"$(wc -c < \"$tmp\")\" = \"{size}\" ] && \
         chmod 0755 \"$tmp\" && mv \"$tmp\" {remote_path} && trap - EXIT",
        dir = REMOTE_DIR,
    )
}

#[async_trait::async_trait]
impl Bootstrapper for Manager {
    async fn ensure_uploaded(&self) -> Result<String, String> {
        let sha = &self.agent.git_sha;
        if sha.is_empty() {
            return Err("bootstrap: embedded agent has no SHA".into());
        }
        let bytes = self.selected_bytes().await?;
        if bytes.is_empty() {
            return Err("bootstrap: embedded agent is empty".into());
        }
        let remote_path = format!("{REMOTE_DIR}/agent-{sha}");
        let digest = hex::encode(Sha256::digest(bytes.as_ref()));

        // 1. Probe by size + content hash. A hit still has to converge the
        //    stable `portald` symlink: the binary being present says NOTHING
        //    about the link, and everything box-side (shims, xdg-open,
        //    doctor, keychain askpass) resolves `~/.cache/portal/portald`,
        //    not `agent-<sha>`. A v1→v2 upgrade is exactly this case — the
        //    hash matches on the second connect while the link still points
        //    at a pruned v1 path or does not exist at all.
        if let Ok(out) = self.bash(probe_script(&remote_path), b"").await
            && out.trim() == format!("{} {}", bytes.len(), digest)
        {
            self.ensure_portald_link(&remote_path).await;
            return Ok(remote_path);
        }

        // 2. Atomic verified upload.
        tracing::info!(target: "portal::bootstrap", remote = %remote_path,
            bytes = bytes.len(), "uploading agent");
        self.bash(upload_script(&remote_path, bytes.len()), bytes.as_ref())
            .await
            .map_err(|e| format!("bootstrap: upload failed: {e}"))?;

        // 3. Stable portald symlink (shims/xdg-open/doctor resolve it) — and
        // 4. best-effort prune of stale agents / tmp fragments.
        self.ensure_portald_link(&remote_path).await;
        let _ = self
            .bash(
                format!(
                    "find {d} -maxdepth 1 -name 'agent-*' ! -name 'agent-{sha}' -mtime +0 -delete 2>/dev/null; \
                     find {d} -maxdepth 1 -name '.agent.tmp.*' -delete 2>/dev/null; true",
                    d = REMOTE_DIR,
                ),
                b"",
            )
            .await;
        Ok(remote_path)
    }

    fn embedded_sha(&self) -> String {
        self.agent.git_sha.clone()
    }

    async fn ensure_box_converged(&self) -> Result<(), String> {
        let remote_path = self.ensure_uploaded().await?;
        self.ensure_portald_link(&remote_path).await;
        ensure_shims(&*self.transport).await.map(|_| ())
    }

    fn set_boot_id(&self, id: &str) {
        self.state.lock().unwrap().boot = id.to_string();
    }
}

impl Manager {
    /// Converge `~/.cache/portal/portald` → `agent-<sha>`.
    ///
    /// Best-effort by design: forwarding must never be held hostage to a
    /// symlink write. But it runs on EVERY session (probe hit included),
    /// because every box-side entry point — the clip shims, xdg-open,
    /// portal-askpass, and `portal doctor`'s portald check — resolves the
    /// stable path. `ln -sfn` with a preceding rm handles the case where the
    /// path exists as a directory or a stale regular file, which plain
    /// `ln -sf` silently turns into `portald/agent-<sha>`.
    async fn ensure_portald_link(&self, remote_path: &str) {
        let link = format!("{REMOTE_DIR}/portald");
        let script = format!(
            "set -e; install -d -m 0700 {dir}; \
             if [ \"$(readlink {link} 2>/dev/null)\" = {remote_path} ] && [ -x {link} ]; then exit 0; fi; \
             rm -rf {link}; ln -sfn {remote_path} {link}",
            dir = REMOTE_DIR,
        );
        if let Err(e) = self.bash(script, b"").await {
            tracing::warn!(target: "portal::bootstrap", link = %link,
                "portald symlink convergence failed — box-side shims, xdg-open and askpass \
                 will not resolve portald: {e}");
        }
    }
}

/// Deploy/refresh the PATH shims on the box (daemon-driven, v1 DESIGN §9.1):
/// steady state is ONE grep for the version marker; on mismatch each shim is
/// written atomically (tmp + chmod 0755 + rename) into ~/.local/bin. Failure
/// is non-fatal to the session (forwarding must not be held hostage to a
/// shim write) but LOUD — clipboard paste depends on these.
pub async fn ensure_shims(transport: &dyn Transport) -> Result<bool, String> {
    let marker = format!(
        "{} v{}",
        portald::shims::OWNERSHIP_MARKER,
        portald::shims::VERSION
    );
    let shims = portald::shims::all();
    let names: Vec<&str> = shims.iter().map(|(n, _)| *n).collect();

    // Steady-state probe: every shim present AND carrying the current marker.
    let probe = format!(
        "for f in {}; do grep -qF {} \"$HOME/.local/bin/$f\" 2>/dev/null || {{ echo STALE; exit 0; }}; done; echo OK",
        names.join(" "),
        shell_quote(&marker),
    );
    let out = transport
        .exec(b"", &["bash".into(), "-c".into(), shell_quote(&probe)])
        .await
        .map_err(|e| format!("shim probe: {e}"))?;
    if out.stdout_lossy().trim() == "OK" {
        // Fast path still converges the rc PATH blocks (v1 semantics): a
        // user who deleted one gets it back without a shim rewrite.
        ensure_path_blocks(transport).await?;
        return Ok(false);
    }

    for (name, script) in &shims {
        let install = format!(
            "set -e; mkdir -p \"$HOME/.local/bin\" && tmp=$(mktemp \"$HOME/.local/bin/.{name}.tmp.XXXXXX\") && \
             trap 'rm -f \"$tmp\"' EXIT && cat > \"$tmp\" && chmod 0755 \"$tmp\" && \
             mv \"$tmp\" \"$HOME/.local/bin/{name}\" && trap - EXIT",
        );
        transport
            .exec(
                script.as_bytes(),
                &["bash".into(), "-c".into(), shell_quote(&install)],
            )
            .await
            .map_err(|e| format!("shim {name}: {e}"))?;
    }
    tracing::info!(target: "portal::bootstrap", shims = shims.len(), "deployed clip shims");
    ensure_path_blocks(transport).await?;
    Ok(true)
}

/// Marker strings are SHIPPED STATE (v1 clipshim wrote them to user rc files);
/// they MUST stay byte-identical so v1-converged boxes are recognized instead
/// of accumulating duplicate blocks.
const PATH_MARKER_START: &str = "# >>> portal PATH (clip shims) >>>";
const PATH_MARKER_END: &str = "# <<< portal PATH (clip shims) <<<";
const EARLY_PATH_MARKER_START: &str = "# >>> portal PATH early (non-interactive) >>>";
const EARLY_PATH_MARKER_END: &str = "# <<< portal PATH early (non-interactive) <<<";

/// The dedup-prepend line (v1 DESIGN §9.2): remove any existing ~/.local/bin
/// from PATH and re-add it at the FRONT, so the shims win even on a box that
/// already has /usr/bin/xclip with ~/.local/bin later (or absent) on PATH.
const DEDUP_PREPEND: &str = r#"PATH="$HOME/.local/bin:$(printf '%s' "$PATH" | tr ':' '\n' | grep -vxF "$HOME/.local/bin" | paste -sd: -)"
export PATH"#;

fn path_prepend_snippet() -> String {
    format!(
        "{PATH_MARKER_START}\n\
         # Ensures portal's shims (~/.local/bin/xclip, wl-paste, pbpaste, wl-copy,\n\
         # pbcopy, sudo, portal-askpass) win on PATH.\n\
         {DEDUP_PREPEND}\n\
         {PATH_MARKER_END}"
    )
}

fn early_path_prepend_snippet() -> String {
    format!(
        "{EARLY_PATH_MARKER_START}\n\
         # Placed above the distro interactive guard so sshd-sourced non-interactive\n\
         # bash gets the shims; the bottom portal PATH block re-wins interactively.\n\
         {DEDUP_PREPEND}\n\
         {EARLY_PATH_MARKER_END}"
    )
}

/// Converge the rc-file PATH marker blocks (port of v1 clipshim's
/// ensureEarlyPathPrepend + ensurePathPrepend). PATH ordering is the single
/// make-or-break for shim interception (DESIGN §9.2):
/// - EARLY block at the TOP of ~/.bashrc — above Debian/Ubuntu's interactive
///   guard, so sshd-sourced non-interactive bash gets the shims;
/// - BOTTOM block appended to ~/.bashrc, ~/.zshrc, ~/.zshenv and ~/.profile
///   (created when missing) — multiple files because PATH managers
///   (nvm/asdf/mise/conda) re-export PATH later;
/// - ~/.bash_profile and ~/.bash_login receive the bottom block only when
///   they already EXIST — creating either would make bash skip ~/.profile.
///
/// Runs on every reconnect (cheap, idempotent by marker grep) so a user who
/// deleted a block gets it back without forcing a shim rewrite.
pub async fn ensure_path_blocks(transport: &dyn Transport) -> Result<(), String> {
    let early = format!(
        "block=$(cat)\nrc=~/.bashrc\n\
         if [ -f \"$rc\" ] && grep -qF '{EARLY_PATH_MARKER_START}' \"$rc\"; then exit 0; fi\n\
         touch \"$rc\" || exit 1\n\
         tmp=$(mktemp) || exit 1\n\
         if printf '%s\\n\\n' \"$block\" > \"$tmp\" && cat \"$rc\" >> \"$tmp\" && \
            cat \"$tmp\" > \"$rc\" && rm -f \"$tmp\"; then exit 0; fi\n\
         rm -f \"$tmp\"; exit 1",
    );
    transport
        .exec(
            early_path_prepend_snippet().as_bytes(),
            &["bash".into(), "-c".into(), shell_quote(&early)],
        )
        .await
        .map_err(|e| format!("early PATH block: {e}"))?;

    let bottom = format!(
        "block=$(cat)\n\
         for rc in ~/.bashrc ~/.zshrc ~/.zshenv ~/.profile; do\n\
           if [ -f \"$rc\" ] && grep -qF '{PATH_MARKER_START}' \"$rc\"; then continue; fi\n\
           printf '\\n%s\\n' \"$block\" >> \"$rc\"\n\
         done\n\
         for rc in ~/.bash_profile ~/.bash_login; do\n\
           [ -f \"$rc\" ] || continue\n\
           if grep -qF '{PATH_MARKER_START}' \"$rc\"; then continue; fi\n\
           printf '\\n%s\\n' \"$block\" >> \"$rc\"\n\
         done",
    );
    transport
        .exec(
            path_prepend_snippet().as_bytes(),
            &["bash".into(), "-c".into(), shell_quote(&bottom)],
        )
        .await
        .map_err(|e| format!("PATH block: {e}"))?;
    Ok(())
}

fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_transport::testing::FakeTransport;

    fn agent(sha: &str, bytes: &[u8]) -> EmbeddedAgent {
        EmbeddedAgent {
            git_sha: sha.into(),
            linux_amd64: Some(Arc::from(bytes)),
            linux_arm64: None,
        }
    }

    /// A probe hit skips the UPLOAD but must still converge the `portald`
    /// symlink — that link is what every box-side shim resolves, and a v1→v2
    /// upgrade hits exactly this path with a stale/absent link.
    #[tokio::test]
    async fn probe_hit_skips_upload_but_still_converges_link() {
        let t = FakeTransport::new("devbox1");
        let bytes = b"agent-binary";
        let digest = hex::encode(Sha256::digest(bytes));
        t.push_exec_ok("Linux x86_64\n"); // uname
        t.push_exec_ok(&format!("{} {}", bytes.len(), digest)); // probe
        t.push_exec_ok(""); // symlink convergence
        t.push_exec_ok(&format!("{} {}", bytes.len(), digest)); // probe (2nd call)
        t.push_exec_ok(""); // symlink convergence (2nd call)

        let m = Manager::new(t.clone(), agent("cafe", bytes));
        assert_eq!(
            m.ensure_uploaded().await.unwrap(),
            "~/.cache/portal/agent-cafe"
        );
        assert_eq!(
            m.ensure_uploaded().await.unwrap(),
            "~/.cache/portal/agent-cafe"
        );

        let calls = t.exec_calls();
        // uname probed ONCE (cached), then probe+link per ensure call, no upload.
        assert_eq!(calls.len(), 5);
        assert_eq!(calls[0].0, vec!["uname", "-sm"]);
        assert!(
            calls[1].0[2].contains("sha256sum"),
            "probe script: {}",
            calls[1].0[2]
        );
        assert!(
            calls[2].0[2].contains("ln -sfn"),
            "probe hit must converge the symlink: {}",
            calls[2].0[2]
        );
        assert!(
            !calls
                .iter()
                .any(|(a, _)| a.iter().any(|s| s.contains("mktemp"))),
            "probe hit must not re-upload"
        );
    }

    #[tokio::test]
    async fn missing_probe_uploads_atomically_then_links_and_prunes() {
        let t = FakeTransport::new("devbox1");
        let bytes = b"agent-binary";
        t.push_exec_ok("Linux x86_64\n"); // uname
        t.push_exec_ok("MISSING\n"); // probe
        t.push_exec_ok(""); // upload
        t.push_exec_ok(""); // symlink
        t.push_exec_ok(""); // prune

        let m = Manager::new(t.clone(), agent("cafe", bytes));
        m.ensure_uploaded().await.unwrap();

        let calls = t.exec_calls();
        assert_eq!(calls.len(), 5);
        let upload = &calls[2];
        assert_eq!(upload.1, bytes, "upload must feed the binary on stdin");
        let script = &upload.0[2];
        assert!(script.contains("mktemp"), "atomic tmp: {script}");
        assert!(
            script.contains(&format!("= \"{}\"", bytes.len())),
            "byte-count assert: {script}"
        );
        // The script rides inside shell_quote(), so the trap's inner quotes
        // appear in their '\'' escaped form.
        assert!(
            script.contains(r#"rm -f "$tmp""#) && script.contains("EXIT"),
            "EXIT trap: {script}"
        );
        assert!(
            calls[3].0[2].contains("ln -sf"),
            "symlink: {}",
            calls[3].0[2]
        );
        assert!(calls[4].0[2].contains("find"), "prune: {}", calls[4].0[2]);
    }

    #[tokio::test]
    async fn boot_id_change_reprobes_arch() {
        let t = FakeTransport::new("devbox1");
        let bytes = b"agent-binary";
        let digest = hex::encode(Sha256::digest(bytes));
        let hit = format!("{} {}", bytes.len(), digest);
        t.push_exec_ok("Linux x86_64\n"); // uname #1
        t.push_exec_ok(&hit); // probe #1
        t.push_exec_ok(""); // link #1
        t.push_exec_ok(&hit); // probe #2 (same boot: no uname)
        t.push_exec_ok(""); // link #2
        t.push_exec_ok("Linux x86_64\n"); // uname #2 (boot changed)
        t.push_exec_ok(&hit); // probe #3
        t.push_exec_ok(""); // link #3

        let m = Manager::new(t.clone(), agent("cafe", bytes));
        m.set_boot_id("boot-1");
        m.ensure_uploaded().await.unwrap();
        m.ensure_uploaded().await.unwrap();
        m.set_boot_id("boot-2");
        m.ensure_uploaded().await.unwrap();

        let unames = t
            .exec_calls()
            .iter()
            .filter(|(argv, _)| argv[0] == "uname")
            .count();
        assert_eq!(unames, 2);
    }

    #[tokio::test]
    async fn unsupported_platform_errors() {
        let t = FakeTransport::new("devbox1");
        t.push_exec_ok("Darwin arm64\n");
        let m = Manager::new(t, agent("cafe", b"x"));
        let err = m.ensure_uploaded().await.unwrap_err();
        assert!(err.contains("unsupported dev box platform"), "{err}");
    }

    #[tokio::test]
    async fn shims_probe_ok_is_one_grep() {
        let t = FakeTransport::new("devbox1");
        t.push_exec_ok("OK\n");
        t.push_exec_ok(""); // early PATH block
        t.push_exec_ok(""); // bottom PATH block
        assert!(!ensure_shims(&*t).await.unwrap());
        let calls = t.exec_calls();
        assert_eq!(calls.len(), 3, "probe + 2 rc-block convergences");
        let script = &calls[0].0[2];
        assert!(script.contains("grep -qF"), "{script}");
        assert!(
            script.contains(portald::shims::OWNERSHIP_MARKER),
            "{script}"
        );
        for (name, _) in portald::shims::all() {
            assert!(script.contains(name), "probe must cover {name}");
        }
    }

    #[tokio::test]
    async fn stale_shims_are_rewritten_atomically() {
        let t = FakeTransport::new("devbox1");
        t.push_exec_ok("STALE\n");
        let shims = portald::shims::all();
        for _ in &shims {
            t.push_exec_ok("");
        }
        t.push_exec_ok(""); // early PATH block
        t.push_exec_ok(""); // bottom PATH block
        assert!(ensure_shims(&*t).await.unwrap());
        let calls = t.exec_calls();
        assert_eq!(calls.len(), 1 + shims.len() + 2);
        for (i, (name, script)) in shims.iter().enumerate() {
            let (argv, stdin) = &calls[1 + i];
            assert_eq!(stdin, script.as_bytes(), "{name}: script rides stdin");
            let install = &argv[2];
            assert!(install.contains("mktemp"), "{name}: atomic tmp");
            assert!(
                install.contains(&format!(".local/bin/{name}")),
                "{name}: target"
            );
            assert!(install.contains("chmod 0755"), "{name}: executable");
        }
    }

    #[tokio::test]
    async fn shim_install_failure_is_an_error() {
        let t = FakeTransport::new("devbox1");
        t.push_exec_ok("STALE\n");
        t.push_exec_err("read-only filesystem");
        let err = ensure_shims(&*t).await.unwrap_err();
        assert!(err.contains("read-only filesystem"), "{err}");
    }

    #[tokio::test]
    async fn path_blocks_carry_v1_markers_and_dedup_prepend() {
        let t = FakeTransport::new("devbox1");
        t.push_exec_ok(""); // early
        t.push_exec_ok(""); // bottom
        ensure_path_blocks(&*t).await.unwrap();
        let calls = t.exec_calls();
        assert_eq!(calls.len(), 2);

        // Early block: inserted at the TOP of ~/.bashrc only, snippet on stdin.
        let (argv, stdin) = &calls[0];
        let early_stdin = String::from_utf8_lossy(stdin);
        assert!(argv[2].contains(EARLY_PATH_MARKER_START), "{}", argv[2]);
        assert!(
            argv[2].contains("cat \"$rc\" >> \"$tmp\""),
            "prepend, not append"
        );
        assert!(early_stdin.starts_with(EARLY_PATH_MARKER_START));
        assert!(early_stdin.trim_end().ends_with(EARLY_PATH_MARKER_END));
        assert!(
            early_stdin.contains("grep -vxF \"$HOME/.local/bin\""),
            "dedup"
        );

        // Bottom block: unconditional rc set + conditional bash_profile/login.
        let (argv, stdin) = &calls[1];
        let bottom_stdin = String::from_utf8_lossy(stdin);
        for rc in ["~/.bashrc", "~/.zshrc", "~/.zshenv", "~/.profile"] {
            assert!(argv[2].contains(rc), "missing {rc}");
        }
        assert!(
            argv[2].contains("[ -f \"$rc\" ] || continue"),
            "bash_profile/login must be conditional: {}",
            argv[2]
        );
        assert!(bottom_stdin.starts_with(PATH_MARKER_START));
        assert!(bottom_stdin.trim_end().ends_with(PATH_MARKER_END));
        // Shipped-state markers (v1 wrote these to user rc files) — never drift.
        assert_eq!(PATH_MARKER_START, "# >>> portal PATH (clip shims) >>>");
        assert_eq!(
            EARLY_PATH_MARKER_START,
            "# >>> portal PATH early (non-interactive) >>>"
        );
    }
}
