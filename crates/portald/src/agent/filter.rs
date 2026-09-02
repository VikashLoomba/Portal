//! Base port admission filter: a loopback-reachable listener is admitted iff
//! `allow.contains(port) || (!deny.contains(port) && !(exclude_ephemeral && in_ephem))`.
//! The agent then expands that base to ephemeral companion listeners owned by
//! the same PID (and optionally process group); expansion never bypasses deny.

use std::collections::BTreeSet;

#[derive(Debug, Clone, Default)]
pub struct PortFilter {
    deny: BTreeSet<u16>,
    allow: BTreeSet<u16>,
    exclude_ephemeral: bool,
    follow_process_group: bool,
    ephem: (u16, u16),
}

impl PortFilter {
    pub fn new(
        deny: impl IntoIterator<Item = u16>,
        allow: impl IntoIterator<Item = u16>,
        exclude_ephemeral: bool,
        follow_process_group: bool,
        ephem: (u16, u16),
    ) -> Self {
        Self {
            deny: deny.into_iter().collect(),
            allow: allow.into_iter().collect(),
            exclude_ephemeral,
            follow_process_group,
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

    /// Whether this port was rejected only by the ephemeral-range cut. A
    /// listener related by PID (or optionally process group) may bypass that
    /// one heuristic, but never an explicit deny.
    pub fn is_related_candidate(&self, port: u16) -> bool {
        !self.allow.contains(&port)
            && !self.deny.contains(&port)
            && self.exclude_ephemeral
            && port >= self.ephem.0
            && port <= self.ephem.1
    }

    pub fn follows_process_group(&self) -> bool {
        self.follow_process_group
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn allow_wins_over_deny_and_ephemeral() {
        let f = PortFilter::new([22, 8000], [8000, 45000], true, false, (32768, 60999));
        assert!(!f.admits(22), "denied");
        assert!(f.admits(8000), "allow overrides deny");
        assert!(f.admits(45000), "allow overrides ephemeral cut");
        assert!(!f.admits(33000), "ephemeral excluded");
        assert!(f.admits(3000), "plain dev port");
    }

    #[test]
    fn ephemeral_cut_only_when_requested() {
        let f = PortFilter::new([], [], false, false, (32768, 60999));
        assert!(f.admits(45000));
    }

    #[test]
    fn related_listener_may_only_bypass_ephemeral_cut() {
        let f = PortFilter::new([22, 45001], [45002], true, false, (32768, 60999));
        assert!(f.is_related_candidate(45000));
        assert!(!f.is_related_candidate(45001), "deny remains authoritative");
        assert!(!f.is_related_candidate(45002), "allow is already admitted");
        assert!(
            !f.is_related_candidate(8000),
            "ordinary ports are already admitted"
        );
    }
}
