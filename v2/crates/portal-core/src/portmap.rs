//! Multi-box port mapping: which LOCAL port a remote listener lands on.
//!
//! The pretty scheme is a digit prefix: box index `n` maps remote port `p` to
//! local `n*10000 + p` — box 1's `:8000` → `localhost:18000`, box 2's
//! `:8000` → `localhost:28000`. Two hard limits make it a partial function:
//!
//! - COLLISION: remote ports >= 10000 would collide across boxes (box 1's
//!   remote 18000 → 28000 is box 2's remote 8000 → 28000), so the scheme
//!   only applies to remote ports 0..=9999.
//! - OVERFLOW: local ports are u16 (max 65535), so only indexes 1..=5 fit
//!   (5*10000 + 9999 = 59999).
//!
//! Everything the pretty scheme can't express falls back to a DETERMINISTIC
//! allocator: an FNV-1a hash of `box_name:remote` seeds a probe into
//! 60000..=64999, skipping taken ports. Determinism means a restart (or a
//! second machine reading the same config) converges on the same mapping
//! without persisted state in the common case; the daemon still records
//! live assignments (`PortMap`) so collisions within a run are stable.

use std::collections::BTreeMap;
use std::ops::RangeInclusive;

/// Width of one box's indexed range.
pub const INDEX_STRIDE: u16 = 10_000;
/// Largest remote port the indexed scheme can express.
pub const MAX_INDEXED_REMOTE: u16 = INDEX_STRIDE - 1;
/// Largest box index the indexed scheme can express (u16 overflow bound).
pub const MAX_INDEXED_BOX: u8 = 5;
/// Fallback allocator range — above every indexed range, below the top of the
/// ephemeral range so we're unlikely to fight the OS for listeners.
pub const FALLBACK_RANGE: RangeInclusive<u16> = 60_000..=64_999;

/// The pretty scheme: `local = index*10000 + remote`, defined only for
/// `1 <= index <= 5` and `remote <= 9999`. Returns `None` outside that domain
/// (callers then use [`fallback_local_port`]).
pub fn indexed_local_port(index: u8, remote: u16) -> Option<u16> {
    if index == 0 || index > MAX_INDEXED_BOX || remote > MAX_INDEXED_REMOTE {
        return None;
    }
    Some(u16::from(index) * INDEX_STRIDE + remote)
}

/// Deterministic fallback: hash `box_name:remote` into [`FALLBACK_RANGE`] and
/// linearly probe past taken ports. Returns `None` only if the entire range is
/// taken (5000 concurrent fallback forwards — effectively never).
pub fn fallback_local_port(
    box_name: &str,
    remote: u16,
    mut is_taken: impl FnMut(u16) -> bool,
) -> Option<u16> {
    let lo = *FALLBACK_RANGE.start() as u32;
    let len = (*FALLBACK_RANGE.end() - *FALLBACK_RANGE.start() + 1) as u32;
    let seed = fnv1a64(box_name.as_bytes(), remote);
    for i in 0..len {
        let candidate = (lo + ((seed as u32).wrapping_add(i)) % len) as u16;
        if !is_taken(candidate) {
            return Some(candidate);
        }
    }
    None
}

fn fnv1a64(name: &[u8], remote: u16) -> u64 {
    const OFFSET: u64 = 0xcbf2_9ce4_8422_2325;
    const PRIME: u64 = 0x0000_0100_0000_01b3;
    let mut h = OFFSET;
    for &b in name.iter().chain(b":".iter()) {
        h = (h ^ u64::from(b)).wrapping_mul(PRIME);
    }
    for b in remote.to_be_bytes() {
        h = (h ^ u64::from(b)).wrapping_mul(PRIME);
    }
    h
}

/// Live per-daemon assignment table for ONE box. Wraps the two schemes and
/// keeps decisions stable for the lifetime of the daemon: once a remote port
/// is assigned a local port, repeat lookups return the same answer.
///
/// `taken` must be shared truth across ALL boxes (the daemon owns it); this
/// struct only tracks its own box's assignments.
#[derive(Debug)]
pub struct PortMap {
    box_name: String,
    index: u8,
    assigned: BTreeMap<u16, u16>, // remote -> local
}

impl PortMap {
    pub fn new(box_name: impl Into<String>, index: u8) -> Self {
        Self {
            box_name: box_name.into(),
            index,
            assigned: BTreeMap::new(),
        }
    }

    /// Resolve the local port for `remote`, allocating on first sight.
    /// `is_taken` is consulted for NEW allocations only (the daemon passes a
    /// closure over its global in-use set plus actual local listeners).
    pub fn local_for(&mut self, remote: u16, mut is_taken: impl FnMut(u16) -> bool) -> Option<u16> {
        if let Some(&local) = self.assigned.get(&remote) {
            return Some(local);
        }
        let local = match indexed_local_port(self.index, remote) {
            Some(p) if !is_taken(p) => Some(p),
            // Indexed slot occupied or out of domain — deterministic fallback.
            _ => fallback_local_port(&self.box_name, remote, &mut is_taken),
        }?;
        self.assigned.insert(remote, local);
        Some(local)
    }

    /// Forget an assignment (remote listener went away and its forward was
    /// cancelled). The daemon also frees the port in its global taken-set.
    pub fn release(&mut self, remote: u16) -> Option<u16> {
        self.assigned.remove(&remote)
    }

    /// Current remote→local table (for `portal status` / `portal ports`).
    pub fn assignments(&self) -> impl Iterator<Item = (u16, u16)> + '_ {
        self.assigned.iter().map(|(&r, &l)| (r, l))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeSet;

    #[test]
    fn indexed_scheme_happy_path() {
        assert_eq!(indexed_local_port(1, 8000), Some(18000));
        assert_eq!(indexed_local_port(2, 8000), Some(28000));
        assert_eq!(indexed_local_port(1, 0), Some(10000));
        assert_eq!(indexed_local_port(5, 9999), Some(59999));
    }

    #[test]
    fn indexed_scheme_domain_limits() {
        // Collision guard: box 1 remote 18000 must NOT map to 28000 (that is
        // box 2's remote 8000). Out of domain → fallback allocator.
        assert_eq!(indexed_local_port(1, 18000), None);
        assert_eq!(indexed_local_port(1, 10000), None);
        // Overflow guard: index 6 would exceed u16 for high remotes.
        assert_eq!(indexed_local_port(6, 80), None);
        assert_eq!(indexed_local_port(0, 80), None);
    }

    #[test]
    fn fallback_is_deterministic_and_in_range() {
        let a = fallback_local_port("devbox1", 18000, |_| false).unwrap();
        let b = fallback_local_port("devbox1", 18000, |_| false).unwrap();
        assert_eq!(a, b);
        assert!(FALLBACK_RANGE.contains(&a));
        // Different box or port ⇒ (almost surely) different seed; assert the
        // specific stable values so an accidental hash change is loud.
        let c = fallback_local_port("devbox2", 18000, |_| false).unwrap();
        let d = fallback_local_port("devbox1", 18001, |_| false).unwrap();
        assert_ne!((a, a), (c, d));
    }

    #[test]
    fn fallback_probes_past_taken_ports() {
        let free = fallback_local_port("devbox1", 18000, |_| false).unwrap();
        let probed = fallback_local_port("devbox1", 18000, |p| p == free).unwrap();
        assert_ne!(probed, free);
        assert!(FALLBACK_RANGE.contains(&probed));
    }

    #[test]
    fn portmap_assigns_and_stays_stable() {
        let mut taken = BTreeSet::new();
        let mut pm = PortMap::new("devbox1", 1);

        let l1 = pm.local_for(8000, |p| taken.contains(&p)).unwrap();
        assert_eq!(l1, 18000);
        taken.insert(l1);

        // Out-of-domain remote goes to fallback, and repeat lookups are stable
        // even if the taken-set changes afterwards.
        let l2 = pm.local_for(18000, |p| taken.contains(&p)).unwrap();
        assert!(FALLBACK_RANGE.contains(&l2));
        taken.insert(l2);
        assert_eq!(pm.local_for(18000, |p| taken.contains(&p)), Some(l2));

        assert_eq!(pm.release(18000), Some(l2));
        assert_eq!(pm.assignments().collect::<Vec<_>>(), vec![(8000, 18000)]);
    }

    #[test]
    fn portmap_falls_back_when_indexed_slot_is_taken_locally() {
        // Something on the Mac already listens on 18000 (e.g. a local dev
        // server): the indexed slot is unusable, fallback keeps the forward.
        let mut pm = PortMap::new("devbox1", 1);
        let local = pm.local_for(8000, |p| p == 18000).unwrap();
        assert_ne!(local, 18000);
        assert!(FALLBACK_RANGE.contains(&local));
    }
}
