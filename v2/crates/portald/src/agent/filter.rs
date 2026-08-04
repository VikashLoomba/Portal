//! Port admission filter (port of pkg/agent/filter.go semantics):
//! a listening loopback port is DESIRED iff
//!   `allow.contains(port) || (!deny.contains(port) && !(exclude_ephemeral && in_ephem))`
//! — the allowlist FORCE-forwards (wins over deny and the ephemeral cut).

use std::collections::BTreeSet;

#[derive(Debug, Clone, Default)]
pub struct PortFilter {
    deny: BTreeSet<u16>,
    allow: BTreeSet<u16>,
    exclude_ephemeral: bool,
    ephem: (u16, u16),
}

impl PortFilter {
    pub fn new(
        deny: impl IntoIterator<Item = u16>,
        allow: impl IntoIterator<Item = u16>,
        exclude_ephemeral: bool,
        ephem: (u16, u16),
    ) -> Self {
        Self {
            deny: deny.into_iter().collect(),
            allow: allow.into_iter().collect(),
            exclude_ephemeral,
            ephem,
        }
    }

    pub fn admits(&self, port: u16) -> bool {
        if self.allow.contains(&port) {
            return true;
        }
        if self.deny.contains(&port) {
            return false;
        }
        if self.exclude_ephemeral && port >= self.ephem.0 && port <= self.ephem.1 {
            return false;
        }
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn allow_wins_over_deny_and_ephemeral() {
        let f = PortFilter::new([22, 8000], [8000, 45000], true, (32768, 60999));
        assert!(!f.admits(22), "denied");
        assert!(f.admits(8000), "allow overrides deny");
        assert!(f.admits(45000), "allow overrides ephemeral cut");
        assert!(!f.admits(33000), "ephemeral excluded");
        assert!(f.admits(3000), "plain dev port");
    }

    #[test]
    fn ephemeral_cut_only_when_requested() {
        let f = PortFilter::new([], [], false, (32768, 60999));
        assert!(f.admits(45000));
    }
}
