//! Embed the cross-compiled portald binaries at BUILD time when the release
//! pipeline provides them (PORTAL_AGENT_AMD64_FILE / PORTAL_AGENT_ARM64_FILE
//! env at compile time → include_bytes! via OUT_DIR indirection). Dev builds
//! without the env produce empty placeholders and the daemon falls back to
//! the RUNTIME env override (PORTAL_AGENT_AMD64/ARM64 paths).

use std::env;
use std::fs;
use std::path::PathBuf;

fn main() {
    let out_dir = PathBuf::from(env::var("OUT_DIR").unwrap());
    for (var, name) in [
        ("PORTAL_AGENT_AMD64_FILE", "agent-amd64.bin"),
        ("PORTAL_AGENT_ARM64_FILE", "agent-arm64.bin"),
    ] {
        println!("cargo:rerun-if-env-changed={var}");
        let dst = out_dir.join(name);
        match env::var(var) {
            Ok(path) if !path.is_empty() => {
                println!("cargo:rerun-if-changed={path}");
                fs::copy(&path, &dst)
                    .unwrap_or_else(|e| panic!("{var}: copy {path} -> {}: {e}", dst.display()));
            }
            _ => {
                // Placeholder so include_bytes! always has a target.
                fs::write(&dst, []).expect("write placeholder");
            }
        }
    }
    println!("cargo:rerun-if-env-changed=PORTAL_GIT_SHA");
}
