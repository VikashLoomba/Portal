# Agent notes — devportal

## Rust toolchain: one rule

**rustup is the only Rust on this machine.** Homebrew's `rust` formula was
uninstalled on 2026-08-05 after it repeatedly poisoned `target/` with
mixed-compiler artifacts (`E0514: found crate compiled by an incompatible
version of rustc`). Do not `brew install rust` here.

- `cargo`/`rustc` resolve via `~/.cargo/bin` (rustup proxies); the active
  toolchain comes from `rust-toolchain.toml` (`stable`).
- If `E0514` ever appears again: two compilers wrote to one `target/`.
  Run `which -a cargo rustc` — there must be exactly ONE of each
  (`~/.cargo/bin/...`). Then `cargo clean` once and rebuild.
- Old pinned toolchains (1.85/1.90/1.92/1.93) in `~/.rustup` belong to OTHER
  repos' `rust-toolchain.toml` files. Leave them.

## Build through make, not raw cargo

The crates live at the **repo root** (they were under `v2/` until 2026-08-07;
any surviving `cd v2` instruction is stale). `make build|test|lint|install|release`
from the root. The Makefile stamps one git SHA across portal + embedded portald
agents (mismatch = reconnect loop) and verifies the agents actually embedded.

## Releasing

`make release TAG=v2.x.y`. Do these three things first — `release.sh` gates all
of them and fails closed, but knowing them saves a round trip:

1. **Bump** `[workspace.package] version` in `Cargo.toml` to match the tag, and
   refresh `Cargo.lock` (`cargo update -w --offline`). `portal upgrade` compares
   `CARGO_PKG_VERSION` against the latest tag with a strict `>`, so re-releasing
   the current version ships bytes no installed copy will ever take.
2. **Commit** everything. The SHA stamped into the binary comes from HEAD, so a
   dirty tree produces a binary whose own `--version` misreports its source.
3. **Push** to `origin/main`. `gh release create` passes no `--target`, so the
   tag is cut from the *remote* default branch — an unpushed commit gets a tag
   pointing at its parent.

Tags exist only on GitHub (`gh` creates them server-side); `git tag -l` is empty
locally, so check shipped versions with `gh release list`, never the tag list.
The remaining gates: signed binary executes and reports the built SHA,
notarization Accepted.

## Release credentials

Git-ignored `.env` (notary key id/issuer/key path, minisign key path).
Developer ID cert lives in the login keychain.
