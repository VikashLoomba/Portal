//! Callback-URL relay: making a box-side loopback URL work on the Mac.
//!
//! # The problem
//!
//! Box-side tools (OAuth device flows, `gh auth login`, Vite/Next dev servers,
//! Jupyter, `claude` login) print or launch a URL pointing at their own
//! loopback: `http://localhost:53219/callback?code=...`. On the box, `xdg-open`
//! is our shim, so the request is relayed to the Mac and opened in the Mac's
//! browser. But `localhost:53219` on the Mac is NOT the box's listener — it is
//! whatever happens to be running on the Mac, usually nothing. The user gets
//! "connection refused", or worse, a *different* local service.
//!
//! Opening the URL verbatim is therefore wrong, and so is skipping the open:
//! the user asked for a login page, and the whole point of the product is that
//! the box's ports are reachable from the Mac. The URL must be REWRITTEN to
//! the local port that forwards to that remote port, and the forward must be
//! guaranteed to exist before the browser is pointed at it.
//!
//! # The rules
//!
//! - Only loopback hosts are rewritten (`localhost`, `127.0.0.0/8`, `[::1]`).
//!   A public URL (`https://github.com/login/device`) is already correct from
//!   the Mac and is opened untouched. This is also the security boundary: we
//!   never redirect a non-loopback host.
//! - The port is taken from the URL, defaulting per scheme (http→80,
//!   https→443), because `http://localhost/callback` means port 80 on the box.
//! - Everything else (path, query, fragment) is preserved byte-for-byte. OAuth
//!   `state`/`code` values are single-use and encoding-sensitive; this is why
//!   the rewrite goes through the `url` crate rather than string surgery.
//! - The rewritten host is always `127.0.0.1`, not `localhost`: the forward
//!   binds a specific loopback address, and `localhost` can resolve to `::1`
//!   first on a Mac, which would miss an IPv4-only listener.

use url::{Host, Url};

/// What the daemon should do with a relayed URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    /// Not loopback (or not rewritable): open exactly as received.
    AsIs(String),
    /// Loopback on the box: ensure a forward for `remote_port`, then open the
    /// URL with its port replaced by the local port.
    Loopback { url: Url, remote_port: u16 },
}

/// Classify a relayed URL. Unparseable input is passed through as-is rather
/// than dropped: the box shim already vetted the scheme, and a URL we cannot
/// parse is still better handed to the OS opener than silently discarded.
pub fn classify(raw: &str) -> Target {
    let Ok(url) = Url::parse(raw) else {
        return Target::AsIs(raw.to_string());
    };
    // port_or_known_default covers http/https/ws/wss; a scheme with no known
    // default and no explicit port has no listener to forward to.
    let Some(port) = url.port_or_known_default() else {
        return Target::AsIs(raw.to_string());
    };
    match url.host() {
        Some(Host::Domain(h)) if is_loopback_domain(h) => Target::Loopback {
            url,
            remote_port: port,
        },
        Some(Host::Ipv4(ip)) if ip.is_loopback() => Target::Loopback {
            url,
            remote_port: port,
        },
        Some(Host::Ipv6(ip)) if ip.is_loopback() => Target::Loopback {
            url,
            remote_port: port,
        },
        _ => Target::AsIs(raw.to_string()),
    }
}

/// `localhost` and its subdomains are loopback by RFC 6761. Matching the
/// suffix (not just equality) covers `foo.localhost`, which some dev servers
/// and OAuth redirect registrations use.
fn is_loopback_domain(host: &str) -> bool {
    let h = host.trim_end_matches('.').to_ascii_lowercase();
    h == "localhost" || h.ends_with(".localhost")
}

/// Point a loopback URL at `local_port` on the Mac.
///
/// Sets the host to `127.0.0.1` and the port to the local end. Note that `url`
/// normalizes away a port equal to the scheme default, so a local port of 80
/// on http yields `http://127.0.0.1/cb` — semantically the same target.
pub fn rewrite(url: &Url, local_port: u16) -> String {
    let mut out = url.clone();
    // Both setters only fail for cannot-be-a-base URLs (mailto:, data:), which
    // classify() already excluded by requiring a host. Failure ⇒ return the
    // original rather than a half-rewritten URL.
    if out
        .set_ip_host(std::net::Ipv4Addr::LOCALHOST.into())
        .is_err()
        || out.set_port(Some(local_port)).is_err()
    {
        return url.to_string();
    }
    out.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn loopback_port(raw: &str) -> Option<u16> {
        match classify(raw) {
            Target::Loopback { remote_port, .. } => Some(remote_port),
            Target::AsIs(_) => None,
        }
    }

    #[test]
    fn loopback_forms_are_detected() {
        assert_eq!(
            loopback_port("http://localhost:53219/callback"),
            Some(53219)
        );
        assert_eq!(loopback_port("http://127.0.0.1:8080/x"), Some(8080));
        // 127.0.0.0/8 is all loopback, not just .1.
        assert_eq!(loopback_port("http://127.0.0.53:9000/"), Some(9000));
        assert_eq!(loopback_port("http://[::1]:3000/"), Some(3000));
        assert_eq!(loopback_port("http://LocalHost:1234/"), Some(1234));
        assert_eq!(loopback_port("http://app.localhost:1234/"), Some(1234));
        // Trailing-dot FQDN form.
        assert_eq!(loopback_port("http://localhost.:1234/"), Some(1234));
    }

    #[test]
    fn scheme_defaults_supply_the_port() {
        assert_eq!(loopback_port("http://localhost/callback"), Some(80));
        assert_eq!(loopback_port("https://localhost/callback"), Some(443));
    }

    #[test]
    fn public_urls_are_untouched() {
        let raw = "https://github.com/login/device?user_code=ABCD-1234";
        assert_eq!(classify(raw), Target::AsIs(raw.into()));
    }

    /// The security boundary: a host that merely CONTAINS "localhost" must not
    /// be treated as loopback, or we would forward-and-open an attacker host.
    #[test]
    fn lookalike_hosts_are_not_loopback() {
        for raw in [
            "http://localhost.evil.com/x",
            "http://notlocalhost/x",
            "http://127.0.0.1.evil.com/x",
            "http://evil.com#@localhost/x",
        ] {
            assert!(loopback_port(raw).is_none(), "must not rewrite {raw}");
        }
    }

    #[test]
    fn unparseable_and_schemeless_pass_through() {
        assert_eq!(classify("not a url"), Target::AsIs("not a url".into()));
        // No host, no default port ⇒ nothing to forward.
        assert_eq!(
            classify("mailto:someone@example.com"),
            Target::AsIs("mailto:someone@example.com".into())
        );
    }

    #[test]
    fn rewrite_preserves_path_query_fragment() {
        let Target::Loopback { url, remote_port } =
            classify("http://localhost:53219/cb?code=a%2Fb&state=xy#frag")
        else {
            panic!("expected loopback");
        };
        assert_eq!(remote_port, 53219);
        assert_eq!(
            rewrite(&url, 18080),
            "http://127.0.0.1:18080/cb?code=a%2Fb&state=xy#frag"
        );
    }

    /// `localhost` may resolve to ::1 first on macOS; the forward listens on
    /// 127.0.0.1, so the rewritten URL must pin the IPv4 literal.
    #[test]
    fn rewrite_pins_ipv4_literal() {
        let Target::Loopback { url, .. } = classify("http://localhost/cb") else {
            panic!("expected loopback");
        };
        // Port 80 == http default, so `url` elides it; same target either way.
        assert_eq!(rewrite(&url, 80), "http://127.0.0.1/cb");
        assert_eq!(rewrite(&url, 18080), "http://127.0.0.1:18080/cb");
        // An IPv6 loopback source is rewritten to the IPv4 literal too.
        let Target::Loopback { url, .. } = classify("http://[::1]:3000/cb") else {
            panic!("expected loopback");
        };
        assert_eq!(rewrite(&url, 13000), "http://127.0.0.1:13000/cb");
    }
}
