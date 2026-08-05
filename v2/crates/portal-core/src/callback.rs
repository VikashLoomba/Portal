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

/// Everything the relay must know about one URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Classified {
    pub target: Target,
    /// Loopback ports found INSIDE query parameters (`redirect_uri` and
    /// friends). This is the standard OAuth shape — `aws sso login`,
    /// `gcloud auth login`, `gh auth login -w` all open a PUBLIC authorize
    /// URL that carries `redirect_uri=http://127.0.0.1:<port>/...`; after
    /// login the PROVIDER redirects the browser to that literal URL. The
    /// port is server-side state we cannot rewrite, so these must be
    /// forwarded SAME-PORT before the browser opens, or the post-login
    /// redirect dies with "site cannot be reached".
    pub embedded_callback_ports: Vec<u16>,
}

/// Classify a relayed URL. Unparseable input is passed through as-is rather
/// than dropped: the box shim already vetted the scheme, and a URL we cannot
/// parse is still better handed to the OS opener than silently discarded.
pub fn classify(raw: &str) -> Classified {
    let Ok(url) = Url::parse(raw) else {
        return Classified {
            target: Target::AsIs(raw.to_string()),
            embedded_callback_ports: Vec::new(),
        };
    };
    // Any query value that itself parses as a loopback URL is a callback
    // target the provider will redirect to verbatim. False positives cost an
    // idle same-port forward (TTL/observation reclaims it); false negatives
    // break logins — so extraction is deliberately permissive on the KEY
    // (redirect_uri/redirect_url/callback/… all exist in the wild) and
    // strict on the VALUE (must parse as a URL with a loopback host).
    let mut embedded_callback_ports: Vec<u16> = url
        .query_pairs()
        .filter_map(|(_, v)| {
            let u = Url::parse(&v).ok()?;
            let port = u.port_or_known_default()?;
            is_loopback_url(&u).then_some(port)
        })
        .collect();
    embedded_callback_ports.sort_unstable();
    embedded_callback_ports.dedup();

    // port_or_known_default covers http/https/ws/wss; a scheme with no known
    // default and no explicit port has no listener to forward to.
    let target = match url.port_or_known_default() {
        Some(port) if is_loopback_url(&url) => Target::Loopback {
            url,
            remote_port: port,
        },
        _ => Target::AsIs(raw.to_string()),
    };
    Classified {
        target,
        embedded_callback_ports,
    }
}

fn is_loopback_url(url: &Url) -> bool {
    match url.host() {
        Some(Host::Domain(h)) => is_loopback_domain(h),
        Some(Host::Ipv4(ip)) => ip.is_loopback(),
        Some(Host::Ipv6(ip)) => ip.is_loopback(),
        None => false,
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
        match classify(raw).target {
            Target::Loopback { remote_port, .. } => Some(remote_port),
            Target::AsIs(_) => None,
        }
    }

    fn embedded(raw: &str) -> Vec<u16> {
        classify(raw).embedded_callback_ports
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
        assert_eq!(classify(raw).target, Target::AsIs(raw.into()));
        assert!(embedded(raw).is_empty());
    }

    /// The aws-sso shape (and gcloud/gh -w): PUBLIC authorize URL carrying a
    /// percent-encoded loopback redirect_uri. The provider redirects to that
    /// literal port after login — it must be extracted for same-port
    /// forwarding while the URL itself opens untouched.
    #[test]
    fn oauth_redirect_uri_ports_are_extracted_from_public_urls() {
        let raw = "https://oidc.us-east-1.amazonaws.com/authorize?\
                   response_type=code&client_id=abc&\
                   redirect_uri=http%3A%2F%2F127.0.0.1%3A55555%2Foauth%2Fcallback&\
                   state=xyz&code_challenge=cc&code_challenge_method=S256";
        let c = classify(raw);
        assert_eq!(c.target, Target::AsIs(raw.into()), "public URL opens as-is");
        assert_eq!(c.embedded_callback_ports, vec![55555]);

        // localhost form + scheme-default port.
        assert_eq!(
            embedded("https://p.example/auth?redirect_uri=http%3A%2F%2Flocalhost%3A8400%2Fcb"),
            vec![8400]
        );
        assert_eq!(
            embedded("https://p.example/auth?redirect_uri=http%3A%2F%2Flocalhost%2Fcb"),
            vec![80]
        );
        // Multiple loopback params dedupe.
        assert_eq!(
            embedded(
                "https://p.example/a?redirect_uri=http%3A%2F%2F127.0.0.1%3A9001%2Fx&\
                 callback=http%3A%2F%2F127.0.0.1%3A9001%2Fy"
            ),
            vec![9001]
        );
    }

    /// A public redirect_uri is the provider's business, not ours: never
    /// forward or touch it. Only loopback values are callback targets.
    #[test]
    fn non_loopback_redirect_uris_are_ignored() {
        for raw in [
            "https://p.example/auth?redirect_uri=https%3A%2F%2Fmyapp.example%2Fcallback",
            "https://p.example/auth?redirect_uri=https%3A%2F%2Flocalhost.evil.com%2Fcb",
            "https://p.example/auth?state=notaurl&scope=openid",
        ] {
            assert!(embedded(raw).is_empty(), "must not extract from {raw}");
        }
    }

    /// A loopback page can itself carry a loopback redirect_uri (local IdP
    /// dev setups): both the page port and the embedded port surface.
    #[test]
    fn loopback_page_with_embedded_callback_reports_both() {
        let c = classify(
            "http://localhost:8400/authorize?redirect_uri=http%3A%2F%2F127.0.0.1%3A9200%2Fcb",
        );
        assert!(matches!(
            c.target,
            Target::Loopback {
                remote_port: 8400,
                ..
            }
        ));
        assert_eq!(c.embedded_callback_ports, vec![9200]);
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
        assert_eq!(
            classify("not a url").target,
            Target::AsIs("not a url".into())
        );
        // No host, no default port ⇒ nothing to forward.
        assert_eq!(
            classify("mailto:someone@example.com").target,
            Target::AsIs("mailto:someone@example.com".into())
        );
    }

    #[test]
    fn rewrite_preserves_path_query_fragment() {
        let Target::Loopback { url, remote_port } =
            classify("http://localhost:53219/cb?code=a%2Fb&state=xy#frag").target
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
        let Target::Loopback { url, .. } = classify("http://localhost/cb").target else {
            panic!("expected loopback");
        };
        // Port 80 == http default, so `url` elides it; same target either way.
        assert_eq!(rewrite(&url, 80), "http://127.0.0.1/cb");
        assert_eq!(rewrite(&url, 18080), "http://127.0.0.1:18080/cb");
        // An IPv6 loopback source is rewritten to the IPv4 literal too.
        let Target::Loopback { url, .. } = classify("http://[::1]:3000/cb").target else {
            panic!("expected loopback");
        };
        assert_eq!(rewrite(&url, 13000), "http://127.0.0.1:13000/cb");
    }
}
