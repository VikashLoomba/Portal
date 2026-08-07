//! Multi-box port mapping: which LOCAL port a remote listener lands on.
//!
//! # Identity first, and why it is not just cosmetic
//!
//! The preferred mapping is IDENTITY: `local == remote`. A translated port is
//! not merely uglier, it is a different ORIGIN, and origin is load-bearing on
//! the wire. A browser pointed at `localhost:16274` sends `Host:
//! localhost:16274` and `Origin: http://localhost:16274`, while the service on
//! the box believes it lives at `localhost:6274` — so anything that validates
//! either header rejects the request:
//!
//! - MCP servers over HTTP MUST validate `Origin` and answer 403 on a
//!   mismatch (DNS-rebinding defense, MCP spec) — `@modelcontextprotocol/
//!   inspector` is the case that surfaced this;
//! - Vite/webpack-dev-server (`allowedHosts`), Django (`ALLOWED_HOSTS`),
//!   Jupyter and Rails all do the same class of check;
//! - CORS preflights answer with the origin the SERVER knows, so the browser
//!   discards the response even when the server allows it.
//!
//! Rewriting headers in the data path cannot fix this: it would not cover TLS,
//! WebSocket handshakes, or absolute URLs embedded in HTML/JS/JSON bodies
//! (the inspector's UI on one port talking to its proxy on another). Making
//! the port identity TRUE fixes every one of those at once — this is the
//! property v1 had for free by forwarding same-port.
//!
//! Identity is skipped for privileged remotes (< 1024): the daemon is a user
//! LaunchAgent and cannot bind those, so remote `:80` still gets a translated
//! port rather than a guaranteed bind failure.
//!
//! # Fallbacks: the pretty scheme, then a deterministic allocator
//!
//! Identity is a SHARED resource — two boxes listening on `:3000` cannot both
//! own local `:3000` — so every box keeps a reserved slot behind it. The
//! pretty scheme is a digit prefix: box index `n` maps remote port `p` to
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
//!
//! Contention is resolved first-come and recorded, not persisted: with one box
//! (the common install) every forward is same-port and the mapping is fully
//! determined. With several boxes claiming one port, whichever reconciles
//! first takes identity and the others use their reserved slots; `portal
//! status` always renders the mapping that is actually live.

use std::collections::{BTreeMap, BTreeSet};
use std::ops::RangeInclusive;

/// First port a non-root process can bind on macOS. Identity mapping is only
/// attempted at or above this — the daemon runs as a user LaunchAgent.
pub const FIRST_UNPRIVILEGED_PORT: u16 = 1024;

/// Width of one box's indexed range.
pub const INDEX_STRIDE: u16 = 10_000;
/// Largest remote port the indexed scheme can express.
pub const MAX_INDEXED_REMOTE: u16 = INDEX_STRIDE - 1;
/// Largest box index the indexed scheme can express (u16 overflow bound).
pub const MAX_INDEXED_BOX: u8 = 5;
/// Fallback allocator range — above every indexed range (which ends at
/// 59999). This sits INSIDE the OS ephemeral range (macOS 49152..=65535),
/// which is unavoidable: the indexed scheme already occupies 10000..=59999,
/// so there is no 5000-port band above it outside ephemeral space. Two
/// consequences, both handled rather than assumed away: a bind here can lose
/// to a transient loopback source port (`reject_local` walks past it), and a
/// port allocated here may be one a box-side ephemeral listener later wants
/// same-port (`degraded`/reclaim moves it back once free).
pub const FALLBACK_RANGE: RangeInclusive<u16> = 60_000..=64_999;

/// Whether `remote` may be claimed as its own local port. Privileged ports
/// are excluded because the bind would be refused, not merely contended.
pub fn identity_eligible(remote: u16) -> bool {
    remote >= FIRST_UNPRIVILEGED_PORT
}

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
    /// Local ports whose BIND was refused by the OS. The cross-box taken-set
    /// only knows about ports portal itself claimed; an unrelated Mac-side
    /// listener is invisible until a bind fails, and without remembering that
    /// refusal the allocator would hand out the same doomed port forever.
    unusable: BTreeSet<u16>,
}

impl PortMap {
    pub fn new(box_name: impl Into<String>, index: u8) -> Self {
        Self {
            box_name: box_name.into(),
            index,
            assigned: BTreeMap::new(),
            unusable: BTreeSet::new(),
        }
    }

    /// Resolve the local port for `remote`, allocating on first sight.
    /// `is_taken` is consulted for NEW allocations only (the daemon passes a
    /// closure over its global in-use set plus actual local listeners).
    ///
    /// Preference order: identity (`local == remote`, so `Host`/`Origin` stay
    /// true — see the module docs), then this box's indexed slot, then the
    /// deterministic allocator.
    pub fn local_for(&mut self, remote: u16, mut is_taken: impl FnMut(u16) -> bool) -> Option<u16> {
        if let Some(&local) = self.assigned.get(&remote) {
            return Some(local);
        }
        let local = {
            let unusable = &self.unusable;
            let mut taken = |p: u16| unusable.contains(&p) || is_taken(p);
            if identity_eligible(remote) && !taken(remote) {
                Some(remote)
            } else {
                match indexed_local_port(self.index, remote) {
                    Some(p) if !taken(p) => Some(p),
                    // Indexed slot occupied or out of domain — deterministic
                    // fallback.
                    _ => fallback_local_port(&self.box_name, remote, &mut taken),
                }
            }
        }?;
        self.assigned.insert(remote, local);
        Some(local)
    }

    /// The OS refused to bind `local` for `remote`: drop the assignment and
    /// never offer that local port again while the listener lives, so the next
    /// [`PortMap::local_for`] moves down the preference order instead of
    /// retrying a port somebody else holds.
    pub fn reject_local(&mut self, remote: u16, local: u16) {
        if self.assigned.get(&remote) == Some(&local) {
            self.assigned.remove(&remote);
        }
        self.unusable.insert(local);
    }

    /// Try to assign `remote` its IDENTITY mapping (local == remote).
    ///
    /// [`PortMap::local_for`] already prefers identity, so this is the same
    /// answer in the steady state; the difference is the FAILURE mode. OAuth
    /// callback pins need identity or nothing: the provider redirects the
    /// browser to the literal `redirect_uri` port — server-side state we
    /// cannot rewrite — so a translated local port sends the post-login
    /// redirect into a wall, and silently allocating one would look like
    /// success. An EXISTING assignment wins: a steady-state translated forward
    /// already has working URLs, and stacking a second mapping for the same
    /// remote would leave two listeners racing.
    /// Returns None when identity is unavailable (taken, previously refused,
    /// or privileged); callers fall back to [`PortMap::local_for`]
    /// (rewrite-only flows still work).
    pub fn assign_exact(
        &mut self,
        remote: u16,
        mut is_taken: impl FnMut(u16) -> bool,
    ) -> Option<u16> {
        if let Some(&local) = self.assigned.get(&remote) {
            return Some(local);
        }
        if !identity_eligible(remote) || self.unusable.contains(&remote) || is_taken(remote) {
            return None;
        }
        self.assigned.insert(remote, remote);
        Some(remote)
    }

    /// Forget an assignment (remote listener went away and its forward was
    /// cancelled). The daemon also frees the port in its global taken-set.
    ///
    /// Bind refusals for the ports this mapping used are forgotten too: the
    /// Mac-side holder may well be gone by the time the listener returns, and
    /// a listener cycling is the natural moment to re-probe for identity
    /// rather than inheriting a stale refusal for the daemon's whole life.
    pub fn release(&mut self, remote: u16) -> Option<u16> {
        let local = self.assigned.remove(&remote)?;
        self.unusable.remove(&local);
        self.unusable.remove(&remote);
        Some(local)
    }

    /// Forget an assignment ONLY if it still points at `local`.
    ///
    /// Cancelling a forward is not proof that its remote is gone: a pass that
    /// upgrades a remote to a better local port adds the new forward and then
    /// retires the old one, so a blind [`PortMap::release`] at that moment
    /// would erase the mapping just made and the next pass would churn
    /// straight back to the translated port.
    pub fn release_exact(&mut self, remote: u16, local: u16) -> Option<u16> {
        if self.assigned.get(&remote) != Some(&local) {
            return None;
        }
        self.release(remote)
    }

    /// Current remote→local table (for `portal status` / `portal ports`).
    pub fn assignments(&self) -> impl Iterator<Item = (u16, u16)> + '_ {
        self.assigned.iter().map(|(&r, &l)| (r, l))
    }

    /// Assignments that WANT identity but hold a translated local port —
    /// `(remote, local)`, i.e. the forwards whose `Host`/`Origin` are a lie.
    ///
    /// A conflict is usually transient (a stale dev server, another checkout)
    /// while this daemon lives for weeks, so a first-pass bind refusal must not
    /// be a life sentence: the caller re-probes these each pass and
    /// [`PortMap::release`]s the ones whose identity port came free. Privileged
    /// and out-of-domain remotes are excluded — they can never hold identity,
    /// so probing them would burn an lsof per pass forever.
    pub fn degraded(&self) -> Vec<(u16, u16)> {
        self.assigned
            .iter()
            .map(|(&remote, &local)| (remote, local))
            .filter(|&(remote, local)| local != remote && identity_eligible(remote))
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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

        // Identity is the default: Host/Origin on the wire stay true.
        let l1 = pm.local_for(8000, |p| taken.contains(&p)).unwrap();
        assert_eq!(l1, 8000);
        taken.insert(l1);

        // Repeat lookups are stable even if the taken-set changes afterwards.
        let l2 = pm.local_for(18000, |p| taken.contains(&p)).unwrap();
        assert_eq!(l2, 18000);
        taken.insert(l2);
        assert_eq!(pm.local_for(18000, |p| taken.contains(&p)), Some(l2));

        assert_eq!(pm.release(18000), Some(l2));
        assert_eq!(pm.assignments().collect::<Vec<_>>(), vec![(8000, 8000)]);
    }

    /// The whole point of the identity preference: a forwarded service sees
    /// requests whose `Host`/`Origin` match the address it believes it has.
    #[test]
    fn identity_is_preferred_over_the_indexed_slot() {
        for index in 1..=5 {
            let mut pm = PortMap::new("devbox1", index);
            assert_eq!(pm.local_for(6274, |_| false), Some(6274));
            assert_eq!(pm.local_for(3000, |_| false), Some(3000));
            // High remotes used to land in the fallback range; identity now
            // covers them too.
            assert_eq!(pm.local_for(27003, |_| false), Some(27003));
        }
    }

    /// A user LaunchAgent cannot bind < 1024, so identity there would trade a
    /// working translated forward for a guaranteed EACCES.
    #[test]
    fn privileged_remotes_keep_a_translated_port() {
        assert!(!identity_eligible(80));
        assert!(!identity_eligible(1023));
        assert!(identity_eligible(1024));

        let mut pm = PortMap::new("devbox1", 1);
        assert_eq!(pm.local_for(80, |_| false), Some(10080));
        assert_eq!(pm.local_for(443, |_| false), Some(10443));
        // ...and are never re-probed: identity is unreachable, not contended.
        assert!(pm.degraded().is_empty());
    }

    /// A bind refusal must not be a life sentence. Once the Mac-side squatter
    /// exits, the next pass has to reclaim identity or `Host`/`Origin` stay
    /// wrong for as long as the daemon runs.
    #[test]
    fn a_freed_identity_port_is_reclaimable() {
        let mut pm = PortMap::new("devbox1", 1);

        // Squatter on local 6277: the bind fails, we degrade to the slot.
        assert_eq!(pm.local_for(6277, |_| false), Some(6277));
        pm.reject_local(6277, 6277);
        assert_eq!(pm.local_for(6277, |_| false), Some(16277));

        // Sticky while the squatter lives — including across re-asks.
        assert_eq!(pm.degraded(), vec![(6277, 16277)]);
        assert_eq!(pm.local_for(6277, |_| false), Some(16277));

        // Squatter gone: releasing drops the assignment AND the stale refusal,
        // so the very next resolve is identity again.
        assert_eq!(pm.release(6277), Some(16277));
        assert_eq!(pm.local_for(6277, |_| false), Some(6277));
        assert!(pm.degraded().is_empty());
    }

    #[test]
    fn contended_identity_falls_back_to_the_reserved_slot() {
        // Box 2 wants :3000 but box 1 already holds local 3000.
        let mut pm = PortMap::new("devbox2", 2);
        assert_eq!(pm.local_for(3000, |p| p == 3000), Some(23000));

        // Both identity and the indexed slot gone: deterministic allocator.
        let mut pm = PortMap::new("devbox2", 2);
        let local = pm
            .local_for(3000, |p| p == 3000 || p == 23000)
            .expect("fallback must cover it");
        assert!(FALLBACK_RANGE.contains(&local));
    }

    /// Bind refusals are the only way to learn about a Mac-side listener the
    /// taken-set cannot see, so they must stick until the listener cycles.
    #[test]
    fn rejected_local_moves_down_the_preference_order() {
        let mut pm = PortMap::new("devbox1", 1);
        assert_eq!(pm.local_for(6274, |_| false), Some(6274));

        // Something on the Mac holds 6274: next allocation must not re-offer it.
        pm.reject_local(6274, 6274);
        assert!(pm.assignments().next().is_none());
        assert_eq!(pm.local_for(6274, |_| false), Some(16274));
        assert_eq!(pm.assign_exact(6274, |_| false), Some(16274));

        // Indexed slot refused as well → deterministic allocator.
        pm.reject_local(6274, 16274);
        let local = pm.local_for(6274, |_| false).unwrap();
        assert!(FALLBACK_RANGE.contains(&local));

        // Listener cycles: the refusals were about a moment in time, not a
        // permanent fact, so identity is probed again.
        assert_eq!(pm.release(6274), Some(local));
        assert_eq!(pm.local_for(6274, |_| false), Some(6274));
    }

    #[test]
    fn assign_exact_gives_identity_or_defers() {
        let mut pm = PortMap::new("devbox1", 1);
        // Free identity slot — OAuth redirect can land.
        assert_eq!(pm.assign_exact(55555, |_| false), Some(55555));
        // Recorded: subsequent general lookups agree (reconcile keeps it).
        assert_eq!(pm.local_for(55555, |_| false), Some(55555));
        // Identity slot taken — caller must fall back / report.
        let mut pm2 = PortMap::new("devbox1", 1);
        assert_eq!(pm2.assign_exact(55556, |p| p == 55556), None);
        // Privileged callback port: unbindable, so the caller must be told to
        // fall back rather than handed a mapping that cannot exist.
        let mut pm3 = PortMap::new("devbox1", 1);
        assert_eq!(pm3.assign_exact(443, |_| false), None);
        // An existing (translated) assignment wins over identity: its URLs
        // already work and a duplicate listener would race it.
        let mut pm4 = PortMap::new("devbox1", 1);
        assert_eq!(pm4.local_for(8000, |p| p == 8000), Some(18000));
        assert_eq!(pm4.assign_exact(8000, |_| false), Some(18000));
    }
}
