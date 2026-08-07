//! Host resolution via `ssh -G`: the ONE way to honor the user's
//! ~/.ssh/config exactly as their ssh does (Include, Match, canonicalization,
//! ProxyJump, ProxyCommand, per-host IdentityFile). We parse the flat
//! key/value dump instead of re-implementing config semantics — the trick v1's
//! native transport proved out.

use std::path::PathBuf;

use crate::TransportError;
use crate::runner::Runner;

#[derive(Debug, Clone, Default, PartialEq)]
pub struct ResolvedTarget {
    /// Real hostname to dial (config `HostName`) — also the known_hosts key.
    pub hostname: String,
    pub user: String,
    pub port: u16,
    pub identity_files: Vec<PathBuf>,
    /// Ordered ProxyJump hop specs (`user@host:port` forms), empty = direct.
    pub proxy_jump: Vec<String>,
    /// ProxyCommand with %h/%p already unexpanded (we substitute at spawn).
    pub proxy_command: Option<String>,
}

/// Resolve `destination` ([user@]host, an alias or literal) through the
/// system ssh's config machinery.
pub async fn resolve(
    runner: &dyn Runner,
    destination: &str,
    port_override: Option<u16>,
    user_override: Option<&str>,
) -> Result<ResolvedTarget, TransportError> {
    let mut args: Vec<String> = vec!["-G".into()];
    if let Some(p) = port_override {
        args.push("-p".into());
        args.push(p.to_string());
    }
    if let Some(u) = user_override {
        args.push("-l".into());
        args.push(u.to_string());
    }
    args.push(destination.to_string());
    let out = runner.run("ssh", &args, b"").await?;
    if out.code != 0 {
        return Err(TransportError::Ssh(format!(
            "ssh -G {destination} failed: {}",
            out.stderr_lossy().trim()
        )));
    }
    let target = parse_ssh_g(&out.stdout_lossy());
    if target.hostname.is_empty() {
        return Err(TransportError::Ssh(format!(
            "ssh -G {destination} produced no hostname"
        )));
    }
    Ok(target)
}

/// Parse the `key value` lines of `ssh -G` output (keys are lowercased by ssh).
pub fn parse_ssh_g(text: &str) -> ResolvedTarget {
    let mut t = ResolvedTarget::default();
    for line in text.lines() {
        let Some((key, value)) = line.split_once(' ') else {
            continue;
        };
        let value = value.trim();
        match key {
            "hostname" => t.hostname = value.to_string(),
            "user" => t.user = value.to_string(),
            "port" => t.port = value.parse().unwrap_or(22),
            "identityfile" => t.identity_files.push(expand_tilde(value)),
            "proxyjump" if value != "none" => {
                t.proxy_jump = value.split(',').map(|s| s.trim().to_string()).collect();
            }
            "proxycommand" if value != "none" => {
                t.proxy_command = Some(value.to_string());
            }
            _ => {}
        }
    }
    if t.port == 0 {
        t.port = 22;
    }
    t
}

/// Parse a ProxyJump hop spec: `[user@]host[:port]`, with `[v6]:port` support.
pub fn parse_jump_spec(spec: &str) -> (Option<&str>, &str, Option<u16>) {
    let (user, rest) = match spec.split_once('@') {
        Some((u, r)) => (Some(u), r),
        None => (None, spec),
    };
    if let Some(stripped) = rest.strip_prefix('[') {
        // [v6-literal]:port or [v6-literal]
        if let Some((host, tail)) = stripped.split_once(']') {
            let port = tail.strip_prefix(':').and_then(|p| p.parse().ok());
            return (user, host, port);
        }
        return (user, rest, None);
    }
    // host:port only when the tail is purely numeric AND there is exactly one
    // ':' (a bare v6 literal without brackets stays intact).
    if rest.matches(':').count() == 1
        && let Some((host, p)) = rest.rsplit_once(':')
        && let Ok(port) = p.parse::<u16>()
    {
        return (user, host, Some(port));
    }
    (user, rest, None)
}

fn expand_tilde(path: &str) -> PathBuf {
    if let Some(rest) = path.strip_prefix("~/")
        && let Ok(home) = std::env::var("HOME")
    {
        return PathBuf::from(home).join(rest);
    }
    PathBuf::from(path)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runner::FakeRunner;
    use std::sync::Arc;

    const SSH_G_OUTPUT: &str = "\
user vikash
hostname devbox1.internal
port 2222
identityfile ~/.ssh/id_ed25519
identityfile /opt/keys/alt
proxyjump bastion.corp,vikash@edge:2200
addressfamily any
batchmode no
";

    #[test]
    fn parses_ssh_g_dump() {
        let t = parse_ssh_g(SSH_G_OUTPUT);
        assert_eq!(t.hostname, "devbox1.internal");
        assert_eq!(t.user, "vikash");
        assert_eq!(t.port, 2222);
        assert_eq!(t.identity_files.len(), 2);
        assert!(
            t.identity_files[0]
                .to_string_lossy()
                .ends_with(".ssh/id_ed25519")
        );
        assert!(!t.identity_files[0].to_string_lossy().starts_with('~'));
        assert_eq!(t.identity_files[1], PathBuf::from("/opt/keys/alt"));
        assert_eq!(t.proxy_jump, vec!["bastion.corp", "vikash@edge:2200"]);
        assert_eq!(t.proxy_command, None);
    }

    #[test]
    fn defaults_port_22_and_none_proxy() {
        let t = parse_ssh_g("hostname h\nuser u\nproxyjump none\nproxycommand none\n");
        assert_eq!(t.port, 22);
        assert!(t.proxy_jump.is_empty());
        assert_eq!(t.proxy_command, None);
    }

    #[test]
    fn jump_specs() {
        assert_eq!(parse_jump_spec("bastion"), (None, "bastion", None));
        assert_eq!(parse_jump_spec("u@bastion"), (Some("u"), "bastion", None));
        assert_eq!(
            parse_jump_spec("u@bastion:2200"),
            (Some("u"), "bastion", Some(2200))
        );
        assert_eq!(
            parse_jump_spec("u@[::1]:2200"),
            (Some("u"), "::1", Some(2200))
        );
        assert_eq!(parse_jump_spec("[::1]"), (None, "::1", None));
    }

    #[tokio::test]
    async fn resolve_invokes_ssh_g_with_overrides() {
        let fake = Arc::new(FakeRunner::new());
        fake.push_str("hostname h\nuser u\nport 22\n", "", 0);
        let t = resolve(&*fake, "alias", Some(2200), Some("root"))
            .await
            .unwrap();
        assert_eq!(t.hostname, "h");
        let calls = fake.calls();
        assert_eq!(calls[0].0, "ssh");
        assert_eq!(calls[0].1, vec!["-G", "-p", "2200", "-l", "root", "alias"]);
    }

    #[tokio::test]
    async fn resolve_fails_loudly_on_bad_alias() {
        let fake = Arc::new(FakeRunner::new());
        fake.push_str("", "ssh: Could not resolve", 255);
        let err = resolve(&*fake, "nope", None, None).await.unwrap_err();
        assert!(err.to_string().contains("ssh -G nope failed"), "{err}");
    }
}
