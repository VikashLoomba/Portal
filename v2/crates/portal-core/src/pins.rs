//! Pinned remote ports: the second source of truth for "what should be
//! forwarded".
//!
//! The reconcile engine derives desired state from ONE input: the set of
//! listening ports the agent observed (the Snapshot). `plan()` cancels any
//! forward outside that set, which is correct for its purpose — a listener that
//! went away should not leave a dangling forward.
//!
//! But an explicitly REQUESTED forward is a different kind of fact, and the
//! callback-URL relay produces exactly that. When the box asks the Mac to open
//! `http://localhost:53219/callback`, that port may not be in any snapshot yet:
//!
//! - snapshots are polled, so a listener that just bound is not visible until
//!   the next poll (and the URL relay is faster than the poll);
//! - the OAuth listener's port is usually in the ephemeral range, which the
//!   snapshot filter excludes by default;
//! - the listener is short-lived by design and may be gone before a poll.
//!
//! Without pins, an on-demand forward gets cancelled by the next pass (~50ms
//! later, and every snapshot delta triggers one) — long before the browser
//! connects. The forward would exist just long enough to look like it worked.
//!
//! A pin says "keep this remote port forwarded until `expires`, whether or not
//! it shows up in a snapshot". TTL rather than forever, because nothing tells
//! us the callback listener died; an OAuth flow that is never completed must
//! not leak a forward for the life of the daemon.

use std::collections::BTreeMap;
use std::time::{Duration, Instant};

/// How long a callback pin survives. Long enough for a human to finish an
/// OAuth consent screen (including a password manager and a 2FA prompt),
/// short enough that an abandoned flow cleans itself up.
pub const DEFAULT_PIN_TTL: Duration = Duration::from_secs(300);

/// Remote ports held open independently of the agent snapshot.
#[derive(Debug, Default)]
pub struct PinSet {
    /// remote port -> expiry. Re-pinning extends (latest wins).
    pins: BTreeMap<u16, Instant>,
}

impl PinSet {
    pub fn new() -> Self {
        Self::default()
    }

    /// Pin `remote` for `ttl` from `now`. Re-pinning an existing port extends
    /// its deadline; it never shortens it, so a fresh request cannot be
    /// undercut by a stale one still in flight.
    pub fn pin(&mut self, remote: u16, now: Instant, ttl: Duration) {
        let deadline = now + ttl;
        self.pins
            .entry(remote)
            .and_modify(|d| {
                if deadline > *d {
                    *d = deadline;
                }
            })
            .or_insert(deadline);
    }

    /// Drop expired pins. Returns the ports that just expired so the caller
    /// can log them; their forwards are then removed by the normal plan (they
    /// are simply no longer in the desired set).
    pub fn expire(&mut self, now: Instant) -> Vec<u16> {
        let dead: Vec<u16> = self
            .pins
            .iter()
            .filter(|&(_, &d)| d <= now)
            .map(|(&p, _)| p)
            .collect();
        for p in &dead {
            self.pins.remove(p);
        }
        dead
    }

    /// Currently pinned remote ports.
    pub fn ports(&self) -> impl Iterator<Item = u16> + '_ {
        self.pins.keys().copied()
    }

    pub fn contains(&self, remote: u16) -> bool {
        self.pins.contains_key(&remote)
    }

    pub fn is_empty(&self) -> bool {
        self.pins.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pins_are_held_then_expire() {
        let t0 = Instant::now();
        let mut pins = PinSet::new();
        pins.pin(53219, t0, Duration::from_secs(10));

        assert!(pins.contains(53219));
        assert_eq!(pins.ports().collect::<Vec<_>>(), vec![53219]);
        assert!(pins.expire(t0 + Duration::from_secs(9)).is_empty());
        assert!(pins.contains(53219));

        assert_eq!(pins.expire(t0 + Duration::from_secs(10)), vec![53219]);
        assert!(!pins.contains(53219));
        assert!(pins.is_empty());
    }

    #[test]
    fn repinning_extends_but_never_shortens() {
        let t0 = Instant::now();
        let mut pins = PinSet::new();
        pins.pin(8080, t0, Duration::from_secs(10));
        // A later, shorter request must not undercut the existing deadline.
        pins.pin(8080, t0 + Duration::from_secs(1), Duration::from_secs(2));
        assert!(pins.expire(t0 + Duration::from_secs(5)).is_empty());
        // A longer one extends it.
        pins.pin(8080, t0 + Duration::from_secs(5), Duration::from_secs(30));
        assert!(pins.expire(t0 + Duration::from_secs(20)).is_empty());
        assert_eq!(pins.expire(t0 + Duration::from_secs(35)), vec![8080]);
    }

    /// Expiry is observed at the next reconcile pass (a delta, a kick, or the
    /// safety tick), so a lapsed pin's forward may outlive its TTL by up to
    /// one tick. That is deliberate: an extra idle forward is harmless, and
    /// waking the loop precisely on a pin deadline is not worth the machinery.
    #[test]
    fn expire_reports_every_lapsed_port_at_once() {
        let t0 = Instant::now();
        let mut pins = PinSet::new();
        pins.pin(1, t0, Duration::from_secs(5));
        pins.pin(2, t0, Duration::from_secs(5));
        pins.pin(3, t0, Duration::from_secs(30));
        assert_eq!(pins.expire(t0 + Duration::from_secs(10)), vec![1, 2]);
        assert!(pins.contains(3));
    }
}
