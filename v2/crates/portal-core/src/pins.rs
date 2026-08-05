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
//! A pin's lifecycle is OBSERVATION-DRIVEN with a TTL backstop. Pinned ports
//! are also pushed into the Subscribe allowlist (the supervisor composes
//! base-filter ∪ pins), and the allowlist force-forwards past the agent's
//! ephemeral cut — so a pinned port becomes VISIBLE in snapshots while its
//! listener is alive. That gives the box a way to say "done":
//!
//! - pin → allowlisted → agent reports the listener → pin marked `observed`;
//! - listener exits → next snapshot/delta omits it → an observed pin that
//!   disappears is DEAD and is dropped at once — the forward retires within
//!   one agent poll instead of idling out a timer;
//! - a pin that is NEVER observed (agent never saw the listener: session
//!   down, race, wrong port) falls back to the TTL, because an unobserved
//!   port yields no death signal either — an abandoned flow must not leak a
//!   forward for the life of the daemon.

use std::collections::{BTreeMap, BTreeSet};
use std::time::{Duration, Instant};

/// TTL backstop for pins the agent never confirms. Long enough for a human to
/// finish an OAuth consent screen (password manager, 2FA); short enough that
/// an abandoned flow cleans itself up.
pub const DEFAULT_PIN_TTL: Duration = Duration::from_secs(300);

#[derive(Debug, Clone, Copy)]
struct PinState {
    deadline: Instant,
    /// The agent has reported this port listening at least once since the
    /// pin was taken. From then on its absence means the listener died.
    observed: bool,
}

/// Remote ports held open independently of the agent snapshot.
#[derive(Debug, Default)]
pub struct PinSet {
    pins: BTreeMap<u16, PinState>,
}

impl PinSet {
    pub fn new() -> Self {
        Self::default()
    }

    /// Pin `remote` for `ttl` from `now`. Re-pinning an existing port extends
    /// its deadline (never shortens it, so a fresh request cannot be undercut
    /// by a stale one) and resets `observed`: a re-pin means a NEW callback
    /// flow, whose listener must be seen again before absence means death.
    pub fn pin(&mut self, remote: u16, now: Instant, ttl: Duration) {
        let deadline = now + ttl;
        self.pins
            .entry(remote)
            .and_modify(|st| {
                if deadline > st.deadline {
                    st.deadline = deadline;
                }
                st.observed = false;
            })
            .or_insert(PinState {
                deadline,
                observed: false,
            });
    }

    /// Drop expired pins. Returns the ports that just expired so the caller
    /// can log them; their forwards are then removed by the normal plan (they
    /// are simply no longer in the desired set).
    pub fn expire(&mut self, now: Instant) -> Vec<u16> {
        let dead: Vec<u16> = self
            .pins
            .iter()
            .filter(|&(_, st)| st.deadline <= now)
            .map(|(&p, _)| p)
            .collect();
        for p in &dead {
            self.pins.remove(p);
        }
        dead
    }

    /// Reconcile pins against the agent's current listener set (one snapshot
    /// or coalesced view). A listed pin becomes `observed`; an observed pin
    /// that is no longer listed is DEAD — the box said done — and is dropped.
    /// Returns the dropped ports. Unobserved pins are left to the TTL.
    pub fn observe(&mut self, listening: &BTreeSet<u16>) -> Vec<u16> {
        let mut dead = Vec::new();
        self.pins.retain(|&port, st| {
            if listening.contains(&port) {
                st.observed = true;
                true
            } else if st.observed {
                dead.push(port);
                false
            } else {
                true
            }
        });
        dead
    }

    /// Currently pinned remote ports.
    pub fn ports(&self) -> impl Iterator<Item = u16> + '_ {
        self.pins.keys().copied()
    }

    /// Earliest pin deadline, if any.
    ///
    /// The reconcile loop sleeps on this so a lapsed pin is collected when it
    /// actually expires rather than whenever the next unrelated event happens
    /// to arrive. Without it, expiry is observable only at the next delta,
    /// kick, or safety tick — an idle forward can outlive its TTL by up to a
    /// full safety interval, which makes the TTL advisory rather than real.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.pins.values().map(|st| st.deadline).min()
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

    /// Expiry is driven by [`PinSet::next_deadline`], so the loop wakes on the
    /// earliest lapse instead of waiting for an unrelated event.
    #[test]
    fn expire_reports_every_lapsed_port_at_once() {
        let t0 = Instant::now();
        let mut pins = PinSet::new();
        pins.pin(1, t0, Duration::from_secs(5));
        pins.pin(2, t0, Duration::from_secs(5));
        pins.pin(3, t0, Duration::from_secs(30));
        assert_eq!(pins.next_deadline(), Some(t0 + Duration::from_secs(5)));
        assert_eq!(pins.expire(t0 + Duration::from_secs(10)), vec![1, 2]);
        assert!(pins.contains(3));
        // With the short pins gone the next wakeup moves out to port 3's.
        assert_eq!(pins.next_deadline(), Some(t0 + Duration::from_secs(30)));
        pins.expire(t0 + Duration::from_secs(31));
        assert_eq!(pins.next_deadline(), None);
    }

    /// The observation lifecycle: seen once → absence means the listener died
    /// and the pin drops immediately; never seen → absence proves nothing and
    /// the TTL governs.
    #[test]
    fn observed_pin_drops_on_disappearance_unobserved_waits_for_ttl() {
        let t0 = Instant::now();
        let mut pins = PinSet::new();
        pins.pin(53219, t0, Duration::from_secs(300)); // will be observed
        pins.pin(60001, t0, Duration::from_secs(300)); // never observed

        // Listener up: 53219 reported, 60001 not.
        let listening: BTreeSet<u16> = [53219].into();
        assert!(pins.observe(&listening).is_empty());

        // Listener exits: observed pin drops NOW; unobserved one survives.
        let listening: BTreeSet<u16> = BTreeSet::new();
        assert_eq!(pins.observe(&listening), vec![53219]);
        assert!(!pins.contains(53219));
        assert!(pins.contains(60001), "unobserved pin is TTL-governed");
        assert_eq!(pins.expire(t0 + Duration::from_secs(300)), vec![60001]);
    }

    /// Re-pinning starts a NEW flow: the old observation must not let a
    /// pre-listener snapshot kill the fresh pin.
    #[test]
    fn repin_resets_observation() {
        let t0 = Instant::now();
        let mut pins = PinSet::new();
        pins.pin(53219, t0, Duration::from_secs(300));
        pins.observe(&[53219].into());
        // Second login on the same port: listener not up yet.
        pins.pin(
            53219,
            t0 + Duration::from_secs(60),
            Duration::from_secs(300),
        );
        assert!(
            pins.observe(&BTreeSet::new()).is_empty(),
            "fresh pin must not be treated as a dead observed one"
        );
        assert!(pins.contains(53219));
    }
}
