//! Derived file/path locations.
//!
//! Env-var test seams: `PORTAL_CONFIG_DIR`, `PORTAL_API_SOCK`. There is
//! deliberately no `PORTAL_SOCK` — portal has no ControlMaster socket; the
//! native transport owns its connections in-process.

use std::path::{Path, PathBuf};

/// Single source of truth for the tool's identity; every path/label derives
/// from it.
pub const TOOL: &str = "portal";

#[derive(Debug, Clone)]
pub struct Paths {
    pub home: PathBuf,
    pub uid: u32,
    pub config_dir: PathBuf,
    /// v2 multi-box config document.
    pub config_file: PathBuf,
    /// v1 single-host file — read ONLY for migration.
    pub v1_host_file: PathBuf,
    /// v1 allowlist — read ONLY for migration.
    pub v1_allow_file: PathBuf,
    pub label: String,
    pub bin_dir: PathBuf,
    pub bin_path: PathBuf,
    pub plist: PathBuf,
    /// Menu bar status-item agent (second LaunchAgent; Aqua sessions only).
    pub tray_label: String,
    pub tray_plist: PathBuf,
    pub tray_log: PathBuf,
    pub log: PathBuf,
    pub api_sock: PathBuf,
    /// launchd domain, `gui/<uid>`.
    pub domain: String,
}

impl Paths {
    /// Derive from HOME + uid, honoring env seams via the provided lookup
    /// (pass [`env_lookup`] in production; a closure in tests).
    pub fn derive_with(home: &Path, uid: u32, env: impl Fn(&str) -> Option<String>) -> Paths {
        let config_dir = env("PORTAL_CONFIG_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(|| home.join(".config").join(TOOL));
        let api_sock = env("PORTAL_API_SOCK")
            .map(PathBuf::from)
            .unwrap_or_else(|| config_dir.join("api.sock"));
        let bin_dir = home.join(".local").join("bin");
        let label = format!("local.{TOOL}.autoforward");
        let tray_label = format!("local.{TOOL}.tray");
        Paths {
            home: home.to_path_buf(),
            uid,
            config_file: config_dir.join("config.toml"),
            v1_host_file: config_dir.join("host"),
            v1_allow_file: config_dir.join("allow"),
            bin_path: bin_dir.join(TOOL),
            bin_dir,
            plist: home
                .join("Library")
                .join("LaunchAgents")
                .join(format!("{label}.plist")),
            tray_plist: home
                .join("Library")
                .join("LaunchAgents")
                .join(format!("{tray_label}.plist")),
            tray_log: home
                .join("Library")
                .join("Logs")
                .join(format!("{TOOL}-tray.log")),
            log: home
                .join("Library")
                .join("Logs")
                .join(format!("{TOOL}.log")),
            api_sock,
            domain: format!("gui/{uid}"),
            label,
            tray_label,
            config_dir,
        }
    }

    /// Production derivation (reads the process environment).
    pub fn derive(home: &Path, uid: u32) -> Paths {
        Self::derive_with(home, uid, |k| std::env::var(k).ok())
    }

    /// Per-feature capability-gate file (same file-per-toggle contract as v1:
    /// missing file = ON, contents "off" = OFF, re-read live).
    pub fn feature_file(&self, feature: &str) -> PathBuf {
        self.config_dir.join(format!("feature.{feature}"))
    }
}

/// Standard env lookup for [`Paths::derive_with`].
pub fn env_lookup(key: &str) -> Option<String> {
    std::env::var(key).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derives_defaults() {
        let p = Paths::derive_with(Path::new("/Users/u"), 501, |_| None);
        assert_eq!(p.config_dir, Path::new("/Users/u/.config/portal"));
        assert_eq!(
            p.config_file,
            Path::new("/Users/u/.config/portal/config.toml")
        );
        assert_eq!(p.api_sock, Path::new("/Users/u/.config/portal/api.sock"));
        assert_eq!(p.label, "local.portal.autoforward");
        assert_eq!(p.domain, "gui/501");
        assert_eq!(
            p.plist,
            Path::new("/Users/u/Library/LaunchAgents/local.portal.autoforward.plist")
        );
        assert_eq!(
            p.feature_file("clip-image"),
            Path::new("/Users/u/.config/portal/feature.clip-image")
        );
    }

    #[test]
    fn honors_env_seams() {
        let p = Paths::derive_with(Path::new("/Users/u"), 501, |k| match k {
            "PORTAL_CONFIG_DIR" => Some("/tmp/pcfg".into()),
            _ => None,
        });
        assert_eq!(p.config_dir, Path::new("/tmp/pcfg"));
        // api.sock derives from the overridden config dir (isolation for free).
        assert_eq!(p.api_sock, Path::new("/tmp/pcfg/api.sock"));
    }
}
