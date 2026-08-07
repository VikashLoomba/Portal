//! Multi-box configuration: `~/.config/portal/config.toml`.
//!
//! v1 kept one `host` file plus per-setting files (`allow`, `transport`,
//! `feature.*`). v2 moves box definitions into one TOML document because a
//! box is now a compound value (name, host, index, transport, allowlist).
//! Capability-gate feature files keep their v1 file-per-toggle form (they are
//! Mac-global, not per-box, and `rm`/`touch` editability is a feature).
//!
//! ```toml
//! [[boxes]]
//! name  = "devbox1"          # unique, [a-z0-9-], used in status/logs/paths
//! host  = "vikash@devbox1"   # ssh alias or user@host (resolved via ssh -G)
//! index = 1                  # port-mapping index (1..=5 get the pretty scheme)
//! allow = [9000]             # per-box force-forwarded ports
//! deny  = []                 # per-box extra denies (on top of the defaults)
//! ```
//!
//! There is deliberately NO transport selection: v2 has one transport
//! (native-ssh). v1's system/native toggle existed because the native
//! implementation was the newcomer; in v2 it is the design.

use serde::{Deserialize, Serialize};

/// Remote system services never forwarded, regardless of per-box config.
/// 22 ssh, 25 smtp, 53 dns, 631 cups, 139/445 smb/netbios.
pub const DEFAULT_DENY_PORTS: [u16; 6] = [22, 25, 53, 139, 445, 631];

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct BoxConfig {
    pub name: String,
    pub host: String,
    /// Port-mapping index, used only when the identity mapping is unavailable:
    /// 1..=5 get a reserved `1xxxx`..`5xxxx` slot, higher indexes fall straight
    /// through to the allocator (see `portmap`).
    pub index: u8,
    #[serde(default)]
    pub allow: Vec<u16>,
    #[serde(default)]
    pub deny: Vec<u16>,
    /// Disabled boxes stay configured but get no stack (no master, no agent).
    #[serde(default = "default_true")]
    pub enabled: bool,
}

fn default_true() -> bool {
    true
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Config {
    #[serde(default, rename = "boxes")]
    pub boxes: Vec<BoxConfig>,
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("config parse: {0}")]
    Parse(#[from] toml::de::Error),
    #[error("box {0}: name must be non-empty, [a-z0-9-], and start with a letter")]
    BadName(String),
    #[error("box {0}: host must be non-empty")]
    EmptyHost(String),
    #[error("box {0}: index must be >= 1")]
    ZeroIndex(String),
    #[error("duplicate box name {0:?}")]
    DuplicateName(String),
    #[error("boxes {0:?} and {1:?} share index {2}")]
    DuplicateIndex(String, String, u8),
}

impl Config {
    /// Parse and validate a config document.
    pub fn parse(s: &str) -> Result<Config, ConfigError> {
        let cfg: Config = toml::from_str(s)?;
        cfg.validate()?;
        Ok(cfg)
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        for b in &self.boxes {
            if !valid_name(&b.name) {
                return Err(ConfigError::BadName(b.name.clone()));
            }
            if b.host.trim().is_empty() {
                return Err(ConfigError::EmptyHost(b.name.clone()));
            }
            if b.index == 0 {
                return Err(ConfigError::ZeroIndex(b.name.clone()));
            }
        }
        for (i, a) in self.boxes.iter().enumerate() {
            for b in &self.boxes[i + 1..] {
                if a.name == b.name {
                    return Err(ConfigError::DuplicateName(a.name.clone()));
                }
                if a.index == b.index {
                    return Err(ConfigError::DuplicateIndex(
                        a.name.clone(),
                        b.name.clone(),
                        a.index,
                    ));
                }
            }
        }
        Ok(())
    }

    pub fn enabled_boxes(&self) -> impl Iterator<Item = &BoxConfig> {
        self.boxes.iter().filter(|b| b.enabled)
    }

    /// Migrate a v1 single-host install (`~/.config/portal/host` + `allow`)
    /// into a one-box config. The box gets index 1, but v1's same-port
    /// behaviour is preserved: identity is the preferred mapping, and index 1
    /// only supplies the fallback slot when a local port is already held.
    pub fn migrate_from_v1(host: &str, allow: &[u16]) -> Config {
        Config {
            boxes: vec![BoxConfig {
                name: sanitize_name(host),
                host: host.trim().to_string(),
                index: 1,
                allow: allow.to_vec(),
                deny: Vec::new(),
                enabled: true,
            }],
        }
    }
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.chars().next().is_some_and(|c| c.is_ascii_lowercase())
        && name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
}

/// Derive a legal box name from an ssh host/alias ("vikash@Dev_Box.1" → "dev-box-1").
pub fn sanitize_name(host: &str) -> String {
    let base = host.rsplit('@').next().unwrap_or(host).to_ascii_lowercase();
    let mut out = String::with_capacity(base.len());
    for c in base.chars() {
        if c.is_ascii_lowercase() || c.is_ascii_digit() {
            out.push(c);
        } else if !out.is_empty() && !out.ends_with('-') {
            out.push('-');
        }
    }
    let trimmed = out.trim_matches('-');
    let named = if trimmed.is_empty() { "box" } else { trimmed };
    if named.chars().next().is_some_and(|c| c.is_ascii_digit()) {
        format!("box-{named}")
    } else {
        named.to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = r#"
[[boxes]]
name = "devbox1"
host = "vikash@devbox1"
index = 1
allow = [9000]

[[boxes]]
name = "gpu-box"
host = "gpu.internal"
index = 2
enabled = false
"#;

    #[test]
    fn parses_sample() {
        let cfg = Config::parse(SAMPLE).unwrap();
        assert_eq!(cfg.boxes.len(), 2);
        assert_eq!(cfg.enabled_boxes().count(), 1);
        assert_eq!(cfg.boxes[0].allow, vec![9000]);
    }

    #[test]
    fn rejects_duplicate_index() {
        let doc = r#"
[[boxes]]
name = "a"
host = "a"
index = 1
[[boxes]]
name = "b"
host = "b"
index = 1
"#;
        assert!(matches!(
            Config::parse(doc),
            Err(ConfigError::DuplicateIndex(_, _, 1))
        ));
    }

    #[test]
    fn rejects_duplicate_name_and_bad_name() {
        let dup = r#"
[[boxes]]
name = "a"
host = "h1"
index = 1
[[boxes]]
name = "a"
host = "h2"
index = 2
"#;
        assert!(matches!(
            Config::parse(dup),
            Err(ConfigError::DuplicateName(_))
        ));

        let bad = r#"
[[boxes]]
name = "Bad Name"
host = "h"
index = 1
"#;
        assert!(matches!(Config::parse(bad), Err(ConfigError::BadName(_))));
    }

    #[test]
    fn migrates_v1() {
        let cfg = Config::migrate_from_v1("vikash@Dev_Box.1", &[8080]);
        assert_eq!(cfg.boxes.len(), 1);
        let b = &cfg.boxes[0];
        assert_eq!(b.name, "dev-box-1");
        assert_eq!(b.index, 1);
        assert_eq!(b.allow, vec![8080]);
        cfg.validate().unwrap();
    }

    #[test]
    fn sanitize_name_edge_cases() {
        assert_eq!(sanitize_name("user@10.0.0.5"), "box-10-0-0-5");
        assert_eq!(sanitize_name("---"), "box");
        assert_eq!(sanitize_name("devbox"), "devbox");
    }
}
