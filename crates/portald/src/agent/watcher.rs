//! Loopback-reachable LISTEN watcher. v2 ships the /proc/net/tcp[6] poller at
//! the same 75ms cadence v1's netlink dump ran at — identical observable
//! latency, no netlink dependency, and the parser is unit-testable on any OS.
//! Explicit loopback and wildcard binds are forwardable; a specific
//! non-loopback interface bind is not. (A netlink + destroy-multicast upgrade
//! is a possible later optimization; TASKS.md.)

use std::collections::{BTreeMap, BTreeSet};

use portal_proto::messages::Port;

/// Linux process identity for a listening socket. PID matching is the narrow
/// default used to discover companion listeners; process-group matching is an
/// optional per-box expansion for applications that delegate services to
/// helper children.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct ProcessIdentity {
    pub pid: u32,
    pub process_group: u32,
}

pub type ListenerOwners = BTreeMap<u32, BTreeSet<ProcessIdentity>>;

/// Something that can enumerate loopback-reachable LISTEN ports right now.
pub trait ListenerSource: Send {
    fn listening(&mut self) -> Vec<Port>;

    /// Resolve socket inode → owning process identities in one batch. Fakes
    /// and non-Linux sources may leave this empty; ordinary port discovery
    /// continues to work, while related-listener expansion becomes a no-op.
    fn process_identities(&mut self, _socket_inodes: &BTreeSet<u32>) -> ListenerOwners {
        BTreeMap::new()
    }
}

/// Production source: /proc/net/tcp + /proc/net/tcp6. On non-Linux (dev
/// hosts) both files are absent and this yields nothing.
#[derive(Debug, Default)]
pub struct ProcNetSource {
    /// Socket inodes are stable for a listener's lifetime. Scanning every
    /// process fd table on each 75ms poll would be wasteful, so resolve only
    /// newly observed inodes and discard cache entries when listeners vanish.
    owner_cache: ListenerOwners,
    resolved: BTreeSet<u32>,
}

impl ListenerSource for ProcNetSource {
    fn listening(&mut self) -> Vec<Port> {
        let mut out = Vec::new();
        if let Ok(v4) = std::fs::read_to_string("/proc/net/tcp") {
            out.extend(parse_proc_net(&v4, 4));
        }
        if let Ok(v6) = std::fs::read_to_string("/proc/net/tcp6") {
            out.extend(parse_proc_net(&v6, 6));
        }
        out
    }

    fn process_identities(&mut self, socket_inodes: &BTreeSet<u32>) -> ListenerOwners {
        self.owner_cache
            .retain(|inode, _| socket_inodes.contains(inode));
        self.resolved.retain(|inode| socket_inodes.contains(inode));

        let missing: BTreeSet<u32> = socket_inodes.difference(&self.resolved).copied().collect();
        if !missing.is_empty() {
            // Mark even unresolved inodes as attempted. Permission-restricted
            // /proc mounts degrade to normal filtering instead of rescanning
            // every 75ms forever.
            self.resolved.extend(missing.iter().copied());
            self.scan_process_fds(&missing);
        }
        self.owner_cache.clone()
    }
}

impl ProcNetSource {
    fn scan_process_fds(&mut self, wanted: &BTreeSet<u32>) {
        let Ok(processes) = std::fs::read_dir("/proc") else {
            return;
        };
        for process in processes.flatten() {
            let Some(pid) = process
                .file_name()
                .to_str()
                .and_then(|name| name.parse::<u32>().ok())
            else {
                continue;
            };
            let Ok(fds) = std::fs::read_dir(process.path().join("fd")) else {
                continue;
            };
            let mut matched = BTreeSet::new();
            for fd in fds.flatten() {
                let Ok(target) = std::fs::read_link(fd.path()) else {
                    continue;
                };
                let Some(inode) = socket_inode(&target) else {
                    continue;
                };
                if wanted.contains(&inode) {
                    matched.insert(inode);
                }
            }
            if matched.is_empty() {
                continue;
            }
            let Some(identity) = process_identity(pid) else {
                continue;
            };
            for inode in matched {
                self.owner_cache.entry(inode).or_default().insert(identity);
            }
        }
    }
}

fn socket_inode(target: &std::path::Path) -> Option<u32> {
    let target = target.to_str()?;
    target
        .strip_prefix("socket:[")?
        .strip_suffix(']')?
        .parse()
        .ok()
}

/// `/proc/<pid>/stat` fields after the final `)` are state, ppid, pgrp, … .
/// Using the final parenthesis handles process names containing spaces or `)`.
fn process_identity(pid: u32) -> Option<ProcessIdentity> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let fields: Vec<&str> = stat
        .get(stat.rfind(')')? + 1..)?
        .split_whitespace()
        .collect();
    let process_group = fields.get(2)?.parse().ok()?;
    Some(ProcessIdentity { pid, process_group })
}

/// Parse one /proc/net/tcp[6] dump: keep LISTEN (state 0A) rows whose local
/// address is explicitly loopback (127.0.0.0/8 or ::1) or wildcard
/// (0.0.0.0 or ::). Wildcard listeners are reachable through loopback, so a
/// forward targeting `127.0.0.1:port` works. A bind to a specific non-loopback
/// interface remains excluded because forwarding it through loopback is not
/// guaranteed to work and may be unintended.
///
/// Format per row: `sl local_address:port rem_address:port st ...` where the
/// v4 address is a little-endian u32 in hex (so `0100007F` = 127.0.0.1 —
/// 127 is the LAST byte pair) and the v6 address is 32 hex chars.
pub fn parse_proc_net(contents: &str, family: u8) -> Vec<Port> {
    let mut out = Vec::new();
    for line in contents.lines().skip(1) {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 4 {
            continue;
        }
        if fields[3] != "0A" {
            continue; // not LISTEN
        }
        let Some((addr_hex, port_hex)) = fields[1].rsplit_once(':') else {
            continue;
        };
        let Ok(port) = u16::from_str_radix(port_hex, 16) else {
            continue;
        };
        if port == 0 {
            continue;
        }
        let addr = match family {
            4 if addr_hex.eq_ignore_ascii_case("00000000") => "0.0.0.0",
            4 if addr_hex.len() == 8 && addr_hex[6..8].eq_ignore_ascii_case("7F") => "127.0.0.1",
            6 if addr_hex.eq_ignore_ascii_case("00000000000000000000000000000000") => "::",
            6 if addr_hex.eq_ignore_ascii_case("00000000000000000000000001000000") => "::1",
            _ => continue,
        };
        out.push(Port {
            port,
            family,
            addr: addr.to_string(),
            inode_ns: fields
                .get(9)
                .and_then(|s| s.parse().ok())
                .unwrap_or_default(),
        });
    }
    out
}

/// Linux ephemeral port range (/proc/sys). Fallback: the kernel default.
pub fn ephemeral_range() -> (u16, u16) {
    if let Ok(s) = std::fs::read_to_string("/proc/sys/net/ipv4/ip_local_port_range") {
        let parts: Vec<u16> = s
            .split_whitespace()
            .filter_map(|p| p.parse().ok())
            .collect();
        if parts.len() == 2 && parts[0] <= parts[1] {
            return (parts[0], parts[1]);
        }
    }
    (32768, 60999)
}

/// /proc boot id (empty off-Linux) — feeds HelloAck.boot for the Mac's
/// arch-probe cache invalidation.
pub fn boot_id() -> String {
    std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    const TCP4: &str = "\
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 123456 1 0000000000000000 100 0 0 10 0
   1: 3500007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000   101        0 23456 1 0000000000000000 100 0 0 10 0
   2: 00000000:1F4A 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3456 1 0000000000000000 100 0 0 10 0
   3: 0101A8C0:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5678 1 0000000000000000 100 0 0 10 0
   4: 0100007F:8CA0 0100007F:1F40 01 00000000:00000000 00:00000000 00000000  1000        0 45678 1 0000000000000000 20 4 30 10 -1
";

    const TCP6: &str = "\
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F41 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 7777 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1F4A 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 8888 1 0000000000000000 100 0 0 10 0
   2: 0000000000000000FE80000000000001:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9999 1 0000000000000000 100 0 0 10 0
";

    #[test]
    fn v4_keeps_loopback_and_wildcard_listeners() {
        let ports = parse_proc_net(TCP4, 4);
        let nums: Vec<u16> = ports.iter().map(|p| p.port).collect();
        // 0x1F40=8000 (127.0.0.1), 0x0035=53 (127.0.0.53), and
        // 0x1F4A=8010 (0.0.0.0) are forwardable. The specific-interface bind
        // and ESTABLISHED row are not.
        assert_eq!(nums, vec![8000, 53, 8010]);
        assert_eq!(ports[0].addr, "127.0.0.1");
        assert_eq!(ports[0].inode_ns, 123456);
        assert_eq!(ports[2].addr, "0.0.0.0");
        assert_eq!(ports[2].inode_ns, 3456);
    }

    #[test]
    fn v6_keeps_loopback_and_wildcard_listeners() {
        let ports = parse_proc_net(TCP6, 6);
        let nums: Vec<u16> = ports.iter().map(|p| p.port).collect();
        assert_eq!(nums, vec![8001, 8010]);
        assert_eq!(ports[0].addr, "::1");
        assert_eq!(ports[1].addr, "::");
    }

    #[test]
    fn ephemeral_fallback_is_kernel_default() {
        let (lo, hi) = ephemeral_range();
        assert!(lo >= 1024 && hi > lo);
    }

    #[test]
    fn parses_socket_fd_target() {
        assert_eq!(
            socket_inode(std::path::Path::new("socket:[66793217]")),
            Some(66793217)
        );
        assert_eq!(socket_inode(std::path::Path::new("pipe:[42]")), None);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn reads_current_process_group() {
        let identity = process_identity(std::process::id()).expect("read own /proc stat");
        assert_eq!(identity.pid, std::process::id());
        assert!(identity.process_group > 0);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn resolves_a_live_listener_inode_to_its_owner() {
        let socket = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = socket.local_addr().unwrap().port();
        let mut source = ProcNetSource::default();
        let listener = source
            .listening()
            .into_iter()
            .find(|listener| listener.port == port)
            .expect("live listener appears in /proc/net/tcp");
        let owners = source.process_identities(&BTreeSet::from([listener.inode_ns]));
        assert!(owners.get(&listener.inode_ns).is_some_and(|identities| {
            identities
                .iter()
                .any(|identity| identity.pid == std::process::id())
        }));
    }
}
