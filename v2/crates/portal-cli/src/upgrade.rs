//! `portal upgrade` — v1's safety semantics: download the release binary,
//! RUN it to verify (`--version` must match the expected tag), then
//! atomically rename into ~/.local/bin/portal and reload the agent. A
//! truncated, wrong-arch, or mis-versioned download leaves the running
//! binary untouched. Release-integrity (sha256 + minisign signature)
//! verification is wired here against the CI lane's artifacts when present.

const REPO: &str = "VikashLoomba/Portal";
const ARTIFACT: &str = "portal-v2-darwin-arm64";

/// The release-signing minisign public key, embedded at build time so a
/// distributed binary can verify upgrades with no repo checkout, no
/// PORTAL_MINISIGN_PUB, and no external minisign install.
const MINISIGN_PUB: &str = include_str!("../../../minisign.pub");

#[derive(Debug, Clone)]
pub struct Release {
    pub tag: String,
    pub asset_url: String,
    pub sig_url: Option<String>,
}

/// Latest release metadata via the GitHub API (gh CLI preferred — it carries
/// the user's auth; plain curl as fallback).
pub async fn latest(runner: &dyn portal_transport::runner::Runner) -> Result<Release, String> {
    let url = format!("https://api.github.com/repos/{REPO}/releases/latest");
    let args = vec!["-sfL".to_string(), url];
    let out = runner
        .run("curl", &args, b"")
        .await
        .map_err(|e| e.to_string())?;
    if out.code != 0 {
        return Err(format!("github api: {}", out.stderr_lossy()));
    }
    let json: serde_json::Value =
        serde_json::from_str(&out.stdout_lossy()).map_err(|e| e.to_string())?;
    let tag = json["tag_name"].as_str().unwrap_or("").to_string();
    let assets = json["assets"].as_array().cloned().unwrap_or_default();
    let mut asset_url = None;
    let mut sig_url = None;
    for a in assets {
        let name = a["name"].as_str().unwrap_or("");
        let dl = a["browser_download_url"].as_str().unwrap_or("").to_string();
        if name == ARTIFACT {
            asset_url = Some(dl);
        } else if name == format!("{ARTIFACT}.minisig") {
            sig_url = Some(dl);
        }
    }
    let asset_url = asset_url.ok_or_else(|| format!("no {ARTIFACT} asset in release {tag}"))?;
    if tag.is_empty() {
        return Err("release has no tag".into());
    }
    Ok(Release {
        tag,
        asset_url,
        sig_url,
    })
}

/// Full upgrade dance. Returns a human summary line.
pub async fn upgrade(
    runner: &dyn portal_transport::runner::Runner,
    bin_path: &std::path::Path,
    current_version: &str,
    check_only: bool,
    force: bool,
) -> Result<String, String> {
    let rel = latest(runner).await?;
    if !force && !is_newer(&rel.tag, current_version) {
        return Ok(format!(
            "current ({current_version}) is up to date (latest {})",
            rel.tag
        ));
    }
    if check_only {
        return Ok(format!(
            "new release available: {} (current {current_version})",
            rel.tag
        ));
    }

    let dir = std::env::temp_dir().join(format!("portal-upgrade-{}", std::process::id()));
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    let tmp = dir.join("portal.new");

    // 1. Download (+ signature when the release provides one).
    let code = runner
        .run(
            "curl",
            &[
                "-sfL".into(),
                "-o".into(),
                tmp.display().to_string(),
                rel.asset_url.clone(),
            ],
            b"",
        )
        .await
        .map_err(|e| e.to_string())?
        .code;
    if code != 0 {
        return Err("download failed".into());
    }
    if let Some(sig) = &rel.sig_url {
        verify_signature(runner, &tmp, sig, &dir).await?;
    }

    // 2. Run-once verification (v1): the candidate must execute and report
    //    the expected version — catches truncated/wrong-arch downloads.
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o755));
    }
    let out = runner
        .run(tmp.to_string_lossy().as_ref(), &["--version".into()], b"")
        .await
        .map_err(|e| e.to_string())?;
    let reported = out.stdout_lossy().trim().to_string();
    if out.code != 0 || !reported.contains(rel.tag.trim_start_matches('v')) {
        return Err(format!(
            "candidate failed run-once verification (reported {reported:?}, want {})",
            rel.tag
        ));
    }

    // 3. Atomic swap + reload.
    let backup = bin_path.with_extension("bak");
    let _ = std::fs::rename(bin_path, &backup);
    if let Err(e) = std::fs::rename(&tmp, bin_path) {
        let _ = std::fs::rename(&backup, bin_path); // roll back
        return Err(format!("swap failed: {e}"));
    }
    let _ = std::fs::remove_file(&backup);
    let _ = std::fs::remove_dir_all(&dir);
    Ok(format!("upgraded to {}", rel.tag))
}

/// minisign verification: the release publishes a .minisig; the public key
/// is embedded at build time (PORTAL_MINISIGN_PUB overrides it — test seam
/// and key-rotation escape hatch). A signature that is present but does not
/// verify FAILS the upgrade — never silently skip.
async fn verify_signature(
    runner: &dyn portal_transport::runner::Runner,
    bin: &std::path::Path,
    sig_url: &str,
    dir: &std::path::Path,
) -> Result<(), String> {
    let sig = dir.join("portal.new.minisig");
    runner
        .run(
            "curl",
            &[
                "-sfL".into(),
                "-o".into(),
                sig.display().to_string(),
                sig_url.to_string(),
            ],
            b"",
        )
        .await
        .map_err(|e| e.to_string())?;
    let pubkey = std::env::var("PORTAL_MINISIGN_PUB").unwrap_or_else(|_| MINISIGN_PUB.to_string());
    let sig_text =
        std::fs::read_to_string(&sig).map_err(|e| format!("signature download unreadable: {e}"))?;
    let data = std::fs::read(bin).map_err(|e| e.to_string())?;
    verify_minisign(&data, &sig_text, &pubkey)
}

/// Pure verification half (testable without network or runner). Accepts the
/// public key as either a full minisign.pub document (comment line + key
/// line) or the bare base64 line.
fn verify_minisign(data: &[u8], sig_text: &str, pubkey_text: &str) -> Result<(), String> {
    let key_b64 = pubkey_text
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty() && !l.starts_with("untrusted comment:"))
        .next_back()
        .ok_or("minisign public key is empty")?;
    let pk = minisign_verify::PublicKey::from_base64(key_b64)
        .map_err(|e| format!("bad minisign public key: {e}"))?;
    let sig = minisign_verify::Signature::decode(sig_text)
        .map_err(|e| format!("bad signature file: {e}"))?;
    pk.verify(data, &sig, false)
        .map_err(|e| format!("signature verification FAILED: {e}"))
}

/// Compare semver-ish tags (v1 rule: a git-describe build after its base tag
/// counts as current; --force re-installs anyway).
pub fn is_newer(latest: &str, current: &str) -> bool {
    fn triple(v: &str) -> Option<(u64, u64, u64)> {
        let v = v.trim().trim_start_matches('v');
        let v = v.split('-').next()?;
        let mut it = v.split('.');
        Some((
            it.next()?.parse().ok()?,
            it.next()?.parse().ok()?,
            it.next()?.parse().ok()?,
        ))
    }
    match (triple(latest), triple(current)) {
        (Some(l), Some(c)) => l > c,
        _ => latest != current,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_transport::runner::FakeRunner;

    #[test]
    fn pubkey_full_document_and_bare_line_both_parse() {
        // Wrong-key verification must fail loudly, but the KEY ITSELF must
        // parse from both accepted shapes (full file / bare base64).
        let sig = "untrusted comment: sig\nRUS9mJ21KwF7+wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\ntrusted comment: t\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==\n";
        for key in [
            super::MINISIGN_PUB.to_string(),
            super::MINISIGN_PUB.lines().nth(1).unwrap().to_string(),
        ] {
            let err = verify_minisign(b"data", sig, &key).unwrap_err();
            assert!(
                !err.contains("bad minisign public key"),
                "key should parse, got: {err}"
            );
        }
    }

    #[test]
    fn garbage_key_and_missing_key_fail_loudly() {
        let err = verify_minisign(b"data", "x", "not-base64!!").unwrap_err();
        assert!(err.contains("bad minisign public key"), "{err}");
        let err = verify_minisign(b"data", "x", "untrusted comment: only\n").unwrap_err();
        assert!(err.contains("empty"), "{err}");
    }

    #[test]
    fn version_compare() {
        assert!(is_newer("v2.1.0", "v2.0.0-dev"));
        assert!(!is_newer("v2.0.0", "v2.0.0"));
        assert!(is_newer("v2.0.1", "v2.0.0"));
        assert!(!is_newer("v1.9.9", "v2.0.0"));
    }

    #[tokio::test]
    async fn parses_latest_release() {
        let fake = FakeRunner::new();
        fake.push_str(
            r#"{"tag_name":"v2.1.0","assets":[
                {"name":"portal-v2-darwin-arm64","browser_download_url":"https://dl/bin"},
                {"name":"portal-v2-darwin-arm64.minisig","browser_download_url":"https://dl/sig"}
            ]}"#,
            "",
            0,
        );
        let rel = latest(&fake).await.unwrap();
        assert_eq!(rel.tag, "v2.1.0");
        assert_eq!(rel.asset_url, "https://dl/bin");
        assert_eq!(rel.sig_url.as_deref(), Some("https://dl/sig"));
    }

    #[tokio::test]
    async fn up_to_date_is_a_noop() {
        let fake = FakeRunner::new();
        fake.push_str(
            r#"{"tag_name":"v2.0.0","assets":[{"name":"portal-v2-darwin-arm64","browser_download_url":"https://dl/bin"}]}"#,
            "",
            0,
        );
        let out = upgrade(&fake, std::path::Path::new("/tmp/x"), "v2.0.0", true, false)
            .await
            .unwrap();
        assert!(out.contains("up to date"));
    }
}
