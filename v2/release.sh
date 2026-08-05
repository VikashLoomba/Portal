#!/bin/bash
# portal v2 local release — builds, signs, notarizes, minisigns, and either
# publishes a GitHub release or installs locally. No CI secrets: everything
# uses this machine's credentials (Developer ID in the keychain, the App
# Store Connect API key, the minisign private key), all OUTSIDE the repo.
#
# Prerequisites (one-time):
#   brew install zig cargo-zigbuild minisign gh
#   rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl
#   gh auth login
#   minisign -G -p v2/minisign.pub -s ~/.portal-minisign.key
#
# Credentials: git-ignored v2/.env (sourced automatically):
#   NOTARY_KEY_ID  — App Store Connect Team key id
#   NOTARY_ISSUER  — App Store Connect issuer UUID
#   NOTARY_KEY     — path to AuthKey_<id>.p8
#   MINISIGN_KEY   — path to the minisign private key (default ~/.portal-minisign.key)
#   DEVELOPER_ID   — optional; auto-detected from the keychain if unset
#
# Usage:
#   ./v2/release.sh v2.0.0                         build + sign + notarize + publish
#   INSTALL=1 INSTALL_HOST=<ssh-host> ./v2/release.sh local   build + sign + install (no publish)

set -euo pipefail

TAG="${1:?usage: release.sh <tag> | INSTALL=1 INSTALL_HOST=<host> release.sh local}"

# rustup's shims MUST win over a Homebrew rust: Homebrew's cargo has no musl
# std and ignores rust-toolchain.toml, which breaks the portald cross-builds
# with E0463 "can't find crate for core".
[ -x "$HOME/.cargo/bin/cargo" ] && export PATH="$HOME/.cargo/bin:$PATH"
V2="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$V2")"
cd "$V2"

# Load git-ignored release credentials if present.
if [ -f "$V2/.env" ]; then
  set -a; . "$V2/.env"; set +a
fi

OUT="$V2/target/aarch64-apple-darwin/release"
BIN="$OUT/portal"
ARTIFACT="portal-v2-darwin-arm64"
NOTARY_ZIP="$(mktemp -t portal-notarize).zip"
trap 'rm -f "$NOTARY_ZIP"' EXIT

SHA="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)"
echo "==> portal v2 release: $TAG (sha $SHA)"

# --- 0. Tag must match the crate version (fails-closed) --------------------
# `portal upgrade` compares CARGO_PKG_VERSION against the latest GitHub tag
# (upgrade::is_newer). A binary shipping a version BELOW its own tag sees
# itself as perpetually out of date and re-installs the same release on every
# check. The SHA smoke test below CANNOT catch this: a stale version with a
# correct SHA passes it. So gate on the version too.
CRATE_VER="$(sed -n 's/^version = "\(.*\)"/\1/p' "$V2/Cargo.toml" | head -1)"
if [ "$TAG" != "local" ] && [ "$TAG" != "v$CRATE_VER" ]; then
  echo "portal: tag $TAG != crate version v$CRATE_VER (v2/Cargo.toml) — aborting release" >&2
  echo "portal: bump [workspace.package] version to ${TAG#v}, commit, then re-run" >&2
  exit 1
fi

# --- 1+2. Build (agents + Mac binary, embed-verified) -----------------------
# ONE build path: the Makefile owns cross-compile + embed + verification, so
# a portal binary without embedded agents cannot come out of any flow.
make --no-print-directory build SHA="$SHA"

# --- 3. Sign (Developer ID, hardened runtime) ------------------------------
DEVELOPER_ID="${DEVELOPER_ID:-$(security find-identity -v -p codesigning | grep -o 'Developer ID Application: [^"]*' | head -1)}"
[ -n "$DEVELOPER_ID" ] || { echo "no Developer ID Application identity in keychain" >&2; exit 1; }
echo "==> signing with: $DEVELOPER_ID"
codesign --sign "$DEVELOPER_ID" --options runtime --timestamp --force \
  --entitlements "$V2/portal.entitlements" "$BIN"
codesign --verify --deep --strict --verbose=2 "$BIN"

# --- 3b. Prove the signed binary actually EXECUTES (fails-closed) ----------
# Static verification is NOT enough: restricted entitlements (e.g. the old
# keychain-access-groups) pass codesign --verify AND notarization, but AMFI
# SIGKILLs the process at exec ("Code has restricted entitlements, but the
# validation of its code signature failed" → Killed: 9). Exec the signed
# artifact and check it reports the SHA we just built — a dead or stale
# binary must never reach notarization, publish, or a user's LaunchAgent.
SMOKE="$("$BIN" --version)" || {
  echo "portal: signed binary was KILLED on exec (AMFI policy?) — aborting release" >&2
  echo "portal: check: log show --last 5m --predicate 'process == \"kernel\"' | grep -i 'code signature'" >&2
  exit 1
}
case "$SMOKE" in
  *"sha $SHA"*) echo "==> signed artifact runs: $SMOKE" ;;
  *) echo "portal: signed binary reports '$SMOKE' (want sha $SHA) — stale build?" >&2; exit 1 ;;
esac

# --- 4. Notarize (notarytool; zip required) --------------------------------
NOTARY_KEY_ID="${NOTARY_KEY_ID:?set NOTARY_KEY_ID in v2/.env (App Store Connect key id)}"
NOTARY_ISSUER="${NOTARY_ISSUER:?set NOTARY_ISSUER in v2/.env (issuer UUID)}"
NOTARY_KEY="${NOTARY_KEY:-$HOME/Downloads/AuthKey_${NOTARY_KEY_ID}.p8}"
[ -f "$NOTARY_KEY" ] || { echo "notary key not found: $NOTARY_KEY" >&2; exit 1; }
echo "==> notarizing (key $NOTARY_KEY_ID)"
ditto -c -k --keepParent "$BIN" "$NOTARY_ZIP"
NOTARY_OUT="$(xcrun notarytool submit "$NOTARY_ZIP" \
  --key "$NOTARY_KEY" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER" \
  --wait --timeout 20m)"
echo "$NOTARY_OUT" | tail -4
echo "$NOTARY_OUT" | grep -q "status: Accepted" || {
  echo "portal: notarization NOT accepted" >&2
  echo "$NOTARY_OUT" | grep -iE "status|issue|error" >&2 || true
  exit 1
}
# Single-file binaries can't be stapled; the ticket is in Apple's cloud.

# --- 5. Stage artifacts + minisign ------------------------------------------
# Two asset names, same signed+notarized bytes: v2's upgrader fetches
# $ARTIFACT; v1's `portal upgrade` hardcodes portal-darwin-arm64 — without
# that alias every v1 install errors on upgrade ("release publishes no
# portal-darwin-arm64 asset") instead of moving to v2.
ART="$OUT/$ARTIFACT"
V1ART="$OUT/portal-darwin-arm64"
cp "$BIN" "$ART"
cp "$BIN" "$V1ART"
MINISIGN_KEY="${MINISIGN_KEY:-$HOME/.portal-minisign.key}"
if command -v minisign >/dev/null && [ -f "$MINISIGN_KEY" ]; then
  echo "==> minisign"
  minisign -S -s "$MINISIGN_KEY" -x "$ART.minisig" -m "$ART"
  minisign -S -s "$MINISIGN_KEY" -x "$V1ART.minisig" -m "$V1ART"
else
  echo "portal: minisign unavailable ($MINISIGN_KEY missing?) — release will be unsigned" >&2
fi

# --- 6. Publish or install --------------------------------------------------
if [ "${INSTALL:-0}" = "1" ]; then
  : "${INSTALL_HOST:?set INSTALL_HOST=<ssh-host> to install locally}"
  echo "==> installing locally (no publish) on $INSTALL_HOST"
  "$BIN" install "$INSTALL_HOST"
  exit 0
fi
echo "==> publishing $TAG"
ASSETS=("$ART" "$V1ART")
[ -f "$ART.minisig" ] && ASSETS+=("$ART.minisig")
[ -f "$V1ART.minisig" ] && ASSETS+=("$V1ART.minisig")
if gh release view "$TAG" >/dev/null 2>&1; then
  gh release upload "$TAG" --clobber "${ASSETS[@]}"
else
  gh release create "$TAG" --title "portal v2 $TAG" --generate-notes "${ASSETS[@]}"
fi
echo "==> done: $TAG published"
gh release view "$TAG" --json url -q .url
