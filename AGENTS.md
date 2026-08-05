# Agent notes — devportal

## Rust toolchain (v2/): one rule

**rustup is the only Rust on this machine.** Homebrew's `rust` formula was
uninstalled on 2026-08-05 after it repeatedly poisoned `v2/target/` with
mixed-compiler artifacts (`E0514: found crate compiled by an incompatible
version of rustc`). Do not `brew install rust` here.

- `cargo`/`rustc` resolve via `~/.cargo/bin` (rustup proxies); the active
  toolchain comes from `v2/rust-toolchain.toml` (`stable`).
- If `E0514` ever appears again: two compilers wrote to one `target/`.
  Run `which -a cargo rustc` — there must be exactly ONE of each
  (`~/.cargo/bin/...`). Then `cargo clean` once and rebuild.
- Old pinned toolchains (1.85/1.90/1.92/1.93) in `~/.rustup` belong to OTHER
  repos' `rust-toolchain.toml` files. Leave them.

## Build v2 through make, not raw cargo

`cd v2 && make build|test|lint|install|release`. The Makefile stamps one git
SHA across portal + embedded portald agents (mismatch = reconnect loop) and
verifies the agents actually embedded. `release.sh` gates: tag == crate
version, signed binary executes and reports the built SHA, notarization
Accepted — all fails-closed.

## Release credentials

Git-ignored `v2/.env` (notary key id/issuer/key path, minisign key path).
Developer ID cert lives in the login keychain.
