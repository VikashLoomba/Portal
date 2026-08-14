//! Box-side credential surface:
//!
//! - `portald keychain run --label L [--env VAR | --stdin] -- cmd args…`
//!   asks the Mac for the secret, then runs cmd with it in the child's env
//!   or stdin. The secret NEVER touches argv, disk, logs, or this process's
//!   environment (only the child's).
//! - `portald keychain askpass [prompt]` prints the secret to stdout — the
//!   SUDO_ASKPASS contract (label is fixed to "sudo").
//! - cmd-socket verb `cred\t<base64(json CredShimReq)>` — the one-shot CLI
//!   relays through the RUNNING agent, which forwards a CredRequest up the
//!   pipe and resolves the FIFO head by nonce (see `agent::start_next_cred`).
//!   Reply: `ok\t<base64(secret)>\n` or `deny\t<reason>\n`.
//!
//! Wire caps enforced HERE too (agent-side defense in depth): label ≤ 200,
//! requester/target ≤ 300, secret ≤ 4096 bytes.

use serde::{Deserialize, Serialize};

pub const LABEL_MAX: usize = 200;
pub const CONTEXT_MAX: usize = 300;
pub const SECRET_MAX: usize = 4096;

/// The socket request (JSON, base64-wrapped onto the tab-framed line).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CredShimReq {
    pub label: String,
    #[serde(default)]
    pub requester: String,
    /// "env" | "stdin" | "askpass"
    pub mode: String,
    #[serde(default)]
    pub target: String,
}

impl CredShimReq {
    pub fn validate(&self) -> Result<(), String> {
        if self.label.is_empty() || self.label.len() > LABEL_MAX {
            return Err("label-invalid".into());
        }
        if self.requester.len() > CONTEXT_MAX || self.target.len() > CONTEXT_MAX {
            return Err("label-invalid".into());
        }
        match self.mode.as_str() {
            "env" | "stdin" | "askpass" => Ok(()),
            _ => Err("label-invalid".into()),
        }
    }

    pub fn encode_line(&self) -> String {
        format!(
            "cred\t{}",
            b64(serde_json::to_string(self).unwrap().as_bytes())
        )
    }

    pub fn decode(rest: &str) -> Option<Self> {
        let raw = b64d(rest.trim())?;
        let req: CredShimReq = serde_json::from_slice(&raw).ok()?;
        req.validate().ok()?;
        Some(req)
    }
}

/// Parse the agent's reply line: `ok\t<b64 secret>` | `deny\t<reason>`.
pub fn parse_reply(line: &str) -> Result<Vec<u8>, String> {
    let line = line.trim_end_matches(['\r', '\n']);
    match line.split_once('\t') {
        Some(("ok", b64secret)) => {
            let secret = b64d(b64secret).ok_or("denied")?;
            if secret.len() > SECRET_MAX {
                return Err("denied".into());
            }
            Ok(secret)
        }
        Some(("deny", reason)) => Err(reason.to_string()),
        _ => Err("denied".into()),
    }
}

pub fn encode_reply_ok(secret: &[u8]) -> String {
    format!("ok\t{}\n", b64(secret))
}

pub fn encode_reply_deny(reason: &str) -> String {
    format!("deny\t{reason}\n")
}

/// Human hints for deny reasons, written to stderr.
pub fn explain_deny(reason: &str) -> &'static str {
    match reason {
        "denied" => "denied by user on the Mac",
        "timeout" => "no decision before the timeout",
        "disabled" => "credential sharing is disabled (portal features cred on)",
        "cooldown" => "approval cooldown active — wait a few seconds, then retry",
        "gui-unavailable" => "no GUI session on the Mac to show the consent dialog",
        "label-invalid" => "invalid credential label",
        "no-client" => "no Mac client connected",
        "busy" => "credential request queue is full — retry shortly",
        _ => "request denied or unavailable",
    }
}

// Minimal base64 (std has none; avoid a dep for two ~20-line functions).
const B64: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

pub fn b64(data: &[u8]) -> String {
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let b = [
            chunk[0],
            *chunk.get(1).unwrap_or(&0),
            *chunk.get(2).unwrap_or(&0),
        ];
        let n = (u32::from(b[0]) << 16) | (u32::from(b[1]) << 8) | u32::from(b[2]);
        out.push(B64[(n >> 18) as usize & 63] as char);
        out.push(B64[(n >> 12) as usize & 63] as char);
        out.push(if chunk.len() > 1 {
            B64[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            B64[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}

pub fn b64d(s: &str) -> Option<Vec<u8>> {
    let s = s.trim_end_matches('=');
    let mut out = Vec::with_capacity(s.len() * 3 / 4);
    let mut acc: u32 = 0;
    let mut bits = 0;
    for c in s.bytes() {
        let v = B64.iter().position(|&b| b == c)? as u32;
        acc = (acc << 6) | v;
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((acc >> bits) as u8);
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn b64_roundtrip() {
        for data in [&b""[..], b"a", b"ab", b"abc", b"s3kr3t-\x00\xff-bytes"] {
            assert_eq!(b64d(&b64(data)).unwrap(), data, "{data:?}");
        }
        assert_eq!(b64(b"sudo"), "c3Vkbw==");
    }

    #[test]
    fn shim_req_roundtrip_and_validation() {
        let req = CredShimReq {
            label: "staging admin".into(),
            requester: "pid 42: sh -c curl".into(),
            mode: "env".into(),
            target: "PW".into(),
        };
        let line = req.encode_line();
        let (verb, rest) = line.split_once('\t').unwrap();
        assert_eq!(verb, "cred");
        assert_eq!(CredShimReq::decode(rest).unwrap(), req);

        // Oversized/invalid shapes are rejected at decode.
        let bad = CredShimReq {
            label: "x".repeat(201),
            ..req.clone()
        };
        assert!(CredShimReq::decode(bad.encode_line().split_once('\t').unwrap().1).is_none());
        let bad = CredShimReq {
            mode: "root".into(),
            ..req
        };
        assert!(CredShimReq::decode(bad.encode_line().split_once('\t').unwrap().1).is_none());
    }

    #[test]
    fn reply_parsing() {
        assert_eq!(parse_reply(&encode_reply_ok(b"pw")).unwrap(), b"pw");
        assert_eq!(
            parse_reply(&encode_reply_deny("cooldown")).unwrap_err(),
            "cooldown"
        );
        assert_eq!(parse_reply("garbage").unwrap_err(), "denied");
        // Oversized secret fails closed.
        let big = encode_reply_ok(&vec![b'x'; SECRET_MAX + 1]);
        assert_eq!(parse_reply(&big).unwrap_err(), "denied");
    }
}
