//! Golden vectors: every `docs/vectors/protocol_*.hex` frame must decode AND
//! re-encode byte-exactly. These files are the frozen definition of
//! ProtoVersion 4 — a `portal` build must interoperate with `portald` agents
//! already deployed on boxes, so a diff here is a breaking wire change, not a
//! test to update.
//!
//! (The `exec_*.hex` vectors describe a reserved exec WebSocket subprotocol
//! that no current binary serves; see docs/wire.cddl. Nothing asserts them.)

use std::fs;
use std::path::PathBuf;

use portal_proto::messages::{CredRequest, Notify, unmarshal_payload};
use portal_proto::{Envelope, read_frame, write_frame};

fn vectors_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../docs/vectors")
}

fn load(name: &str) -> Vec<u8> {
    let path = vectors_dir().join(name);
    let hex_str =
        fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
    hex::decode(hex_str.trim()).unwrap_or_else(|e| panic!("hex {name}: {e}"))
}

fn decode(name: &str, bytes: &[u8]) -> Envelope {
    read_frame(&mut &bytes[..]).unwrap_or_else(|e| panic!("{name}: decode failed: {e}"))
}

#[test]
fn all_protocol_vectors_roundtrip_byte_exact() {
    let dir = vectors_dir();
    let mut checked = 0;
    for entry in fs::read_dir(&dir).expect("docs/vectors missing — run from the repo checkout") {
        let path = entry.unwrap().path();
        let name = path.file_name().unwrap().to_string_lossy().into_owned();
        if !name.starts_with("protocol_") || !name.ends_with(".hex") {
            continue;
        }
        let bytes = load(&name);
        let env = decode(&name, &bytes);
        assert_eq!(
            env.populated(),
            1,
            "{name}: expected exactly one populated field"
        );
        let mut out = Vec::new();
        write_frame(&mut out, &env).unwrap_or_else(|e| panic!("{name}: encode: {e}"));
        assert_eq!(
            hex::encode(&out),
            hex::encode(&bytes),
            "{name}: re-encode is not byte-exact"
        );
        checked += 1;
    }
    assert!(
        checked >= 15,
        "only {checked} protocol vectors found in {}",
        dir.display()
    );
}

#[test]
fn hello_vector_fields() {
    let env = decode("protocol_hello.hex", &load("protocol_hello.hex"));
    let hello = env.hello.expect("hello populated");
    assert_eq!(hello.proto_version, portal_proto::PROTO_VERSION);
    assert_eq!(hello.client_git_sha, "client-sha-u10");
    assert_eq!(hello.client_pid, 4242);
    assert_eq!(hello.poll_interval_ms, 75);
    assert!(hello.want_destroy_mc);
    assert_eq!(hello.services.unwrap().get("notify"), Some(&1));
}

#[test]
fn subscribe_vector_defaults_process_group_discovery_off() {
    let env = decode("protocol_subscribe.hex", &load("protocol_subscribe.hex"));
    let subscribe = env.subscribe.expect("subscribe populated");
    assert!(!subscribe.follow_process_group);
}

#[test]
fn msg_vector_notify_payload_decodes() {
    let env = decode("protocol_msg.hex", &load("protocol_msg.hex"));
    let msg = env.msg.expect("msg populated");
    assert_eq!(msg.service, "notify");
    assert_eq!(msg.kind, "event");
    assert_eq!(msg.seq, Some(77));
    let notify: Notify = unmarshal_payload(msg.payload.as_ref().unwrap()).unwrap();
    assert_eq!(notify.title, "deploy complete");
    assert_eq!(notify.urgency, Some(2));
    assert_eq!(notify.verified, Some(true));
    assert_eq!(notify.source.as_deref(), Some("claude_hook"));
}

#[test]
fn cred_request_minimal_omits_optional_fields() {
    let env = decode(
        "protocol_cred_request_minimal.hex",
        &load("protocol_cred_request_minimal.hex"),
    );
    let msg = env.msg.expect("msg populated");
    let req: CredRequest = unmarshal_payload(msg.payload.as_ref().unwrap()).unwrap();
    assert_eq!(req.label, "sudo");
    assert_eq!(req.mode, "askpass");
    assert_eq!(req.requester, None);
    assert_eq!(req.target, None);
}
