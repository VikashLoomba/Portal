//! Loopback LISTEN watcher. v2 ships the /proc/net/tcp[6] poller at the same
//! 75ms cadence v1's netlink dump ran at — identical observable latency, no
//! netlink dependency, and the parser is unit-testable on any OS. (A netlink
//! + destroy-multicast upgrade is a possible later optimization; TASKS.md.)

use portal_proto::messages::Port;

/// Something that can enumerate loopback LISTEN ports right now.
pub trait ListenerSource: Send {
    fn listening(&mut self) -> Vec<Port>;
}

/// Production source: /proc/net/tcp + /proc/net/tcp6. On non-Linux (dev
/// hosts) both files are absent and this yields nothing.
#[derive(Debug, Default)]
pub struct ProcNetSource;

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
}

/// Parse one /proc/net/tcp[6] dump: keep LISTEN (state 0A) rows whose local
/// address is loopback (127.0.0.0/8 for v4, ::1 for v6).
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
        let loopback = match family {
            4 => addr_hex.len() == 8 && addr_hex[6..8].eq_ignore_ascii_case("7F"),
            6 => addr_hex.eq_ignore_ascii_case("00000000000000000000000001000000"),
            _ => false,
        };
        if !loopback {
            continue;
        }
        out.push(Port {
            port,
            family,
            addr: if family == 4 { "127.0.0.1" } else { "::1" }.to_string(),
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
   2: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3456 1 0000000000000000 100 0 0 10 0
   3: 0100007F:8CA0 0100007F:1F40 01 00000000:00000000 00:00000000 00000000  1000        0 45678 1 0000000000000000 20 4 30 10 -1
";

    const TCP6: &str = "\
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F41 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 7777 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1BB9 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 8888 1 0000000000000000 100 0 0 10 0
";

    #[test]
    fn v4_keeps_loopback_listen_only() {
        let ports = parse_proc_net(TCP4, 4);
        let nums: Vec<u16> = ports.iter().map(|p| p.port).collect();
        // 0x1F40=8000 (127.0.0.1), 0x0035=53 (127.0.0.53 — resolvers count),
        // NOT 0x0016=22 (wildcard bind), NOT the ESTABLISHED row.
        assert_eq!(nums, vec![8000, 53]);
        assert_eq!(ports[0].addr, "127.0.0.1");
        assert_eq!(ports[0].inode_ns, 123456);
    }

    #[test]
    fn v6_keeps_only_v6_loopback() {
        let ports = parse_proc_net(TCP6, 6);
        let nums: Vec<u16> = ports.iter().map(|p| p.port).collect();
        assert_eq!(nums, vec![0x1F41]); // ::1 listener; wildcard :: excluded
        assert_eq!(ports[0].addr, "::1");
    }

    #[test]
    fn ephemeral_fallback_is_kernel_default() {
        let (lo, hi) = ephemeral_range();
        assert!(lo >= 1024 && hi > lo);
    }
}
