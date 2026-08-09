//! Supply-chain half of `portal upgrade`.
//!
//! New releases prefer the complete signed/notarized Portal.app archive. The
//! standalone Mach-O remains a compatibility asset so pre-app upgraders can
//! install a bridge release; that bridge then completes app migration in a
//! separate transaction. Nothing active is mutated until this module returns a
//! fully verified [`PreparedUpgrade`].

const REPO: &str = "VikashLoomba/Portal";
const APP_ARTIFACT: &str = "Portal-v2-darwin-arm64.app.zip";
const BINARY_ARTIFACT: &str = "portal-v2-darwin-arm64";

/// Embedded release-signing key; no external minisign executable is required.
const MINISIGN_PUB: &str = include_str!("../../../minisign.pub");

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Asset {
    pub url: String,
    pub sig_url: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Release {
    pub tag: String,
    pub app: Option<Asset>,
    pub binary: Option<Asset>,
}

/// Latest release metadata via the GitHub API.
pub async fn latest(runner: &dyn portal_transport::runner::Runner) -> Result<Release, String> {
    let url = format!("https://api.github.com/repos/{REPO}/releases/latest");
    let out = runner
        .run("curl", &["-sfL".to_string(), url], b"")
        .await
        .map_err(|e| e.to_string())?;
    if out.code != 0 {
        return Err(format!("github api: {}", out.stderr_lossy()));
    }
    let json: serde_json::Value =
        serde_json::from_str(&out.stdout_lossy()).map_err(|e| e.to_string())?;
    let tag = json["tag_name"].as_str().unwrap_or("").to_string();
    if tag.is_empty() {
        return Err("release has no tag".into());
    }
    let assets = json["assets"].as_array().cloned().unwrap_or_default();
    let find = |wanted: &str| {
        assets.iter().find_map(|asset| {
            (asset["name"].as_str()? == wanted)
                .then(|| {
                    asset["browser_download_url"]
                        .as_str()
                        .unwrap_or("")
                        .to_string()
                })
                .filter(|url| !url.is_empty())
        })
    };
    let app = find(APP_ARTIFACT).map(|url| Asset {
        url,
        sig_url: find(&format!("{APP_ARTIFACT}.minisig")),
    });
    let binary = find(BINARY_ARTIFACT).map(|url| Asset {
        url,
        sig_url: find(&format!("{BINARY_ARTIFACT}.minisig")),
    });
    if app.is_none() && binary.is_none() {
        return Err(format!(
            "release {tag} has neither {APP_ARTIFACT} nor {BINARY_ARTIFACT}"
        ));
    }
    Ok(Release { tag, app, binary })
}

pub enum UpgradePlan {
    NoChange(String),
    Candidate(PreparedUpgrade),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ArtifactKind {
    AppBundle,
    StandaloneBinary,
}

pub struct PreparedUpgrade {
    pub tag: String,
    pub kind: ArtifactKind,
    candidate: std::path::PathBuf,
    /// Owns cleanup until the stable helper takes over with `exec`.
    _staging: tempfile::TempDir,
}

impl PreparedUpgrade {
    pub fn candidate(&self) -> &std::path::Path {
        &self.candidate
    }

    pub fn staging_dir(&self) -> &std::path::Path {
        self._staging.path()
    }

    /// Transfer cleanup ownership to an independently submitted updater job.
    /// The staged helper removes this directory after applying the candidate.
    pub fn keep_staging(self) -> std::path::PathBuf {
        self._staging.keep()
    }
}

/// Resolve, download, signature-check, Gatekeeper-check and execute-check an
/// upgrade candidate without touching the active installation.
pub async fn prepare(
    runner: &dyn portal_transport::runner::Runner,
    install_dir: &std::path::Path,
    current_version: &str,
    check_only: bool,
    force: bool,
    app_needed: bool,
) -> Result<UpgradePlan, String> {
    let rel = latest(runner).await?;
    let newer = is_newer(&rel.tag, current_version);
    if !force && !newer && !app_needed {
        return Ok(UpgradePlan::NoChange(format!(
            "current ({current_version}) is up to date (latest {})",
            rel.tag
        )));
    }
    if check_only {
        let message = if app_needed && !newer {
            format!(
                "Portal.app migration available for current release {}",
                rel.tag
            )
        } else {
            format!(
                "new release available: {} (current {current_version})",
                rel.tag
            )
        };
        return Ok(UpgradePlan::NoChange(message));
    }

    std::fs::create_dir_all(install_dir)
        .map_err(|e| format!("create install directory {}: {e}", install_dir.display()))?;
    let staging = tempfile::Builder::new()
        .prefix("portal-upgrade-")
        .tempdir_in(install_dir)
        .map_err(|e| format!("create upgrade staging directory: {e}"))?;

    if let Some(app) = rel.app.as_ref() {
        let archive = staging.path().join("Portal.app.zip");
        download(runner, &app.url, &archive).await?;
        let sig = app
            .sig_url
            .as_deref()
            .ok_or_else(|| format!("release {} has no {APP_ARTIFACT}.minisig", rel.tag))?;
        verify_signature(
            runner,
            &archive,
            sig,
            staging.path(),
            "Portal.app.zip.minisig",
        )
        .await?;

        let extracted = staging.path().join("extracted");
        std::fs::create_dir(&extracted).map_err(|e| e.to_string())?;
        let out = runner
            .run(
                "ditto",
                &[
                    "-x".into(),
                    "-k".into(),
                    archive.display().to_string(),
                    extracted.display().to_string(),
                ],
                b"",
            )
            .await
            .map_err(|e| e.to_string())?;
        if out.code != 0 {
            return Err(format!("extract app archive: {}", out.stderr_lossy()));
        }
        let candidate = extracted.join("Portal.app");
        verify_app_bundle(runner, &candidate, &rel.tag).await?;
        return Ok(UpgradePlan::Candidate(PreparedUpgrade {
            tag: rel.tag,
            kind: ArtifactKind::AppBundle,
            candidate,
            _staging: staging,
        }));
    }

    if app_needed {
        return Err(format!(
            "release {} has no {APP_ARTIFACT}; cannot migrate this installation to Portal.app",
            rel.tag
        ));
    }

    let binary = rel
        .binary
        .as_ref()
        .ok_or_else(|| format!("release {} has no {BINARY_ARTIFACT}", rel.tag))?;
    let candidate = staging.path().join("portal.new");
    download(runner, &binary.url, &candidate).await?;
    if let Some(sig) = &binary.sig_url {
        verify_signature(
            runner,
            &candidate,
            sig,
            staging.path(),
            "portal.new.minisig",
        )
        .await?;
    }
    verify_binary(runner, &candidate, &rel.tag).await?;
    Ok(UpgradePlan::Candidate(PreparedUpgrade {
        tag: rel.tag,
        kind: ArtifactKind::StandaloneBinary,
        candidate,
        _staging: staging,
    }))
}

async fn download(
    runner: &dyn portal_transport::runner::Runner,
    url: &str,
    destination: &std::path::Path,
) -> Result<(), String> {
    let out = runner
        .run(
            "curl",
            &[
                "-sfL".into(),
                "-o".into(),
                destination.display().to_string(),
                url.to_string(),
            ],
            b"",
        )
        .await
        .map_err(|e| e.to_string())?;
    if out.code == 0 {
        Ok(())
    } else {
        Err(format!("download failed: {}", out.stderr_lossy()))
    }
}

async fn verify_binary(
    runner: &dyn portal_transport::runner::Runner,
    binary: &std::path::Path,
    tag: &str,
) -> Result<(), String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(binary, std::fs::Permissions::from_mode(0o755))
            .map_err(|e| e.to_string())?;
    }
    verify_reported_version(runner, binary, tag).await
}

async fn verify_app_bundle(
    runner: &dyn portal_transport::runner::Runner,
    app: &std::path::Path,
    tag: &str,
) -> Result<(), String> {
    let binary = app.join("Contents/MacOS/portal");
    if !app.join("Contents/Info.plist").is_file() || !binary.is_file() {
        return Err("app archive does not contain Portal.app/Contents/MacOS/portal".into());
    }
    for (program, args, label) in [
        (
            "codesign",
            vec![
                "--verify".into(),
                "--deep".into(),
                "--strict".into(),
                app.display().to_string(),
            ],
            "code signature",
        ),
        (
            "spctl",
            vec![
                "--assess".into(),
                "--type".into(),
                "execute".into(),
                app.display().to_string(),
            ],
            "Gatekeeper assessment",
        ),
    ] {
        let out = runner
            .run(program, &args, b"")
            .await
            .map_err(|e| e.to_string())?;
        if out.code != 0 {
            return Err(format!("app {label} failed: {}", out.stderr_lossy()));
        }
    }
    verify_reported_version(runner, &binary, tag).await
}

async fn verify_reported_version(
    runner: &dyn portal_transport::runner::Runner,
    binary: &std::path::Path,
    tag: &str,
) -> Result<(), String> {
    let out = runner
        .run(
            binary.to_string_lossy().as_ref(),
            &["--version".into()],
            b"",
        )
        .await
        .map_err(|e| e.to_string())?;
    let reported = out.stdout_lossy().trim().to_string();
    if out.code == 0 && reported.contains(tag.trim_start_matches('v')) {
        Ok(())
    } else {
        Err(format!(
            "candidate failed run-once verification (reported {reported:?}, want {tag})"
        ))
    }
}

async fn verify_signature(
    runner: &dyn portal_transport::runner::Runner,
    artifact: &std::path::Path,
    sig_url: &str,
    dir: &std::path::Path,
    signature_name: &str,
) -> Result<(), String> {
    let sig = dir.join(signature_name);
    download(runner, sig_url, &sig).await?;
    let pubkey = std::env::var("PORTAL_MINISIGN_PUB").unwrap_or_else(|_| MINISIGN_PUB.to_string());
    let sig_text =
        std::fs::read_to_string(&sig).map_err(|e| format!("signature download unreadable: {e}"))?;
    let data = std::fs::read(artifact).map_err(|e| e.to_string())?;
    verify_minisign(&data, &sig_text, &pubkey)
}

fn verify_minisign(data: &[u8], sig_text: &str, pubkey_text: &str) -> Result<(), String> {
    let key_b64 = pubkey_text
        .lines()
        .map(str::trim)
        .rfind(|line| !line.is_empty() && !line.starts_with("untrusted comment:"))
        .ok_or("minisign public key is empty")?;
    let key = minisign_verify::PublicKey::from_base64(key_b64)
        .map_err(|e| format!("bad minisign public key: {e}"))?;
    let signature = minisign_verify::Signature::decode(sig_text)
        .map_err(|e| format!("bad signature file: {e}"))?;
    key.verify(data, &signature, false)
        .map_err(|e| format!("signature verification FAILED: {e}"))
}

/// Compare semver-ish tags. A git-describe build after its base tag counts as
/// current; `--force` still reinstalls it.
pub fn is_newer(latest: &str, current: &str) -> bool {
    fn triple(version: &str) -> Option<(u64, u64, u64)> {
        let version = version.trim().trim_start_matches('v');
        let version = version.split('-').next()?;
        let mut parts = version.split('.');
        Some((
            parts.next()?.parse().ok()?,
            parts.next()?.parse().ok()?,
            parts.next()?.parse().ok()?,
        ))
    }
    match (triple(latest), triple(current)) {
        (Some(latest), Some(current)) => latest > current,
        _ => latest != current,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use portal_transport::runner::FakeRunner;

    #[test]
    fn pubkey_full_document_and_bare_line_both_parse() {
        let sig = "untrusted comment: sig\nRUS9mJ21KwF7+wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\ntrusted comment: t\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==\n";
        for key in [
            super::MINISIGN_PUB.to_string(),
            super::MINISIGN_PUB.lines().nth(1).unwrap().to_string(),
        ] {
            let err = verify_minisign(b"data", sig, &key).unwrap_err();
            assert!(!err.contains("bad minisign public key"), "{err}");
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
    async fn parses_release_with_app_and_compatibility_binary() {
        let fake = FakeRunner::new();
        fake.push_str(
            r#"{"tag_name":"v2.1.0","assets":[
                {"name":"Portal-v2-darwin-arm64.app.zip","browser_download_url":"https://dl/app"},
                {"name":"Portal-v2-darwin-arm64.app.zip.minisig","browser_download_url":"https://dl/app-sig"},
                {"name":"portal-v2-darwin-arm64","browser_download_url":"https://dl/bin"},
                {"name":"portal-v2-darwin-arm64.minisig","browser_download_url":"https://dl/bin-sig"}
            ]}"#,
            "",
            0,
        );
        let release = latest(&fake).await.unwrap();
        assert_eq!(release.tag, "v2.1.0");
        assert_eq!(release.app.unwrap().url, "https://dl/app");
        assert_eq!(release.binary.unwrap().url, "https://dl/bin");
    }

    #[tokio::test]
    async fn up_to_date_app_install_is_a_noop() {
        let fake = FakeRunner::new();
        fake.push_str(
            r#"{"tag_name":"v2.0.0","assets":[{"name":"Portal-v2-darwin-arm64.app.zip","browser_download_url":"https://dl/app"}]}"#,
            "",
            0,
        );
        let dir = tempfile::tempdir().unwrap();
        let plan = prepare(&fake, dir.path(), "v2.0.0", true, false, false)
            .await
            .unwrap();
        let UpgradePlan::NoChange(message) = plan else {
            panic!("expected no-op plan");
        };
        assert!(message.contains("up to date"));
    }

    #[tokio::test]
    async fn same_version_still_offers_missing_app_migration() {
        let fake = FakeRunner::new();
        fake.push_str(
            r#"{"tag_name":"v2.0.0","assets":[{"name":"Portal-v2-darwin-arm64.app.zip","browser_download_url":"https://dl/app"}]}"#,
            "",
            0,
        );
        let dir = tempfile::tempdir().unwrap();
        let plan = prepare(&fake, dir.path(), "v2.0.0", true, false, true)
            .await
            .unwrap();
        let UpgradePlan::NoChange(message) = plan else {
            panic!("expected check-only plan");
        };
        assert!(message.contains("migration"));
    }
}
