//! Per-label denial cooldown (port of credCooldown): after an explicit user
//! denial, repeat requests for the SAME label are auto-denied for
//! [`crate::DENY_COOLDOWN`] — the anti-prompt-fatigue guard that stops a
//! looping agent from re-raising the dialog every second.

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

#[derive(Debug)]
pub struct Cooldown {
    window: Duration,
    denied: Mutex<HashMap<String, Instant>>,
}

impl Default for Cooldown {
    fn default() -> Self {
        Self::new(crate::DENY_COOLDOWN)
    }
}

impl Cooldown {
    pub fn new(window: Duration) -> Self {
        Self {
            window,
            denied: Mutex::new(HashMap::new()),
        }
    }

    /// True while `label` is inside its denial window (expired entries are
    /// pruned on read, like the Go original). `now` is injected for tests.
    pub fn active(&self, label: &str, now: Instant) -> bool {
        let mut denied = self.denied.lock().unwrap();
        match denied.get(label) {
            Some(&at) if now < at + self.window => true,
            Some(_) => {
                denied.remove(label);
                false
            }
            None => false,
        }
    }

    /// Record an explicit denial at `now`.
    pub fn record(&self, label: &str, now: Instant) {
        self.denied.lock().unwrap().insert(label.to_string(), now);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn denial_window_applies_then_expires() {
        let cd = Cooldown::new(Duration::from_secs(10));
        let t0 = Instant::now();
        assert!(!cd.active("sudo", t0));
        cd.record("sudo", t0);
        assert!(cd.active("sudo", t0 + Duration::from_secs(9)));
        assert!(!cd.active("sudo", t0 + Duration::from_secs(10)));
        // pruned after expiry
        assert!(!cd.active("sudo", t0 + Duration::from_secs(10)));
    }

    #[test]
    fn labels_are_independent() {
        let cd = Cooldown::default();
        let t0 = Instant::now();
        cd.record("a", t0);
        assert!(cd.active("a", t0));
        assert!(!cd.active("b", t0));
    }
}
