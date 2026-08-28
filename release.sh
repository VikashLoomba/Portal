#!/bin/bash
# portal local release — builds, signs, notarizes, minisigns, and either
# publishes a GitHub release or installs locally. No CI secrets: everything
# uses this machine's credentials (Developer ID in the keychain, the App
# Store Connect API key, the minisign private key), all OUTSIDE the repo.
#
# Prerequisites (one-time):
#   brew install zig cargo-zigbuild minisign gh
#   rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl
#   gh auth login
#   minisign -G -p minisign.pub -s ~/.portal-minisign.key
#
# Credentials: git-ignored .env (sourced automatically):
#   NOTARY_KEY_ID  — App Store Connect Team key id
#   NOTARY_ISSUER  — App Store Connect issuer UUID
#   NOTARY_KEY     — path to AuthKey_<id>.p8
#   MINISIGN_KEY   — path to the minisign private key (default ~/.portal-minisign.key)
#   DEVELOPER_ID   — optional; auto-detected from the keychain if unset
#
# Usage:
#   ./release.sh v2.0.0                         build + sign + notarize + publish
#   INSTALL=1 INSTALL_HOST=<ssh-host> ./release.sh local   build + sign + install (no publish)

set -euo pipefail

TAG="${1:?usage: release.sh <tag> | INSTALL=1 INSTALL_HOST=<host> release.sh local}"

# rustup's shims MUST win over a Homebrew rust: Homebrew's cargo has no musl
# std and ignores rust-toolchain.toml, which breaks the portald cross-builds
# with E0463 "can't find crate for core".
[ -x "$HOME/.cargo/bin/cargo" ] && export PATH="$HOME/.cargo/bin:$PATH"
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

# Load git-ignored release credentials if present.
if [ -f "$ROOT/.env" ]; then
  set -a; . "$ROOT/.env"; set +a
fi

OUT="$ROOT/target/aarch64-apple-darwin/release"
BIN="$ROOT/target/swift/arm64-apple-macosx/release/Portal"
ARTIFACT="portal-v2-darwin-arm64"
APP="$OUT/Portal.app"
APP_ARTIFACT="$OUT/Portal-v2-darwin-arm64.app.zip"
DMG_ARTIFACT="$OUT/Portal-v2-darwin-arm64.dmg"
NOTARY_ZIP="$(mktemp -t portal-notarize).zip"
trap 'rm -f "$NOTARY_ZIP"' EXIT

SHA="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)"
echo "==> portal release: $TAG (sha $SHA)"

# --- 0. Tag must match the crate version (fails-closed) --------------------
# `portal upgrade` compares CARGO_PKG_VERSION against the latest GitHub tag
# (upgrade::is_newer). A binary shipping a version BELOW its own tag sees
# itself as perpetually out of date and re-installs the same release on every
# check. The SHA smoke test below CANNOT catch this: a stale version with a
# correct SHA passes it. So gate on the version too.
CRATE_VER="$(sed -n 's/^version = "\(.*\)"/\1/p' "$ROOT/Cargo.toml" | head -1)"
if [ "$TAG" != "local" ] && [ "$TAG" != "v$CRATE_VER" ]; then
  echo "portal: tag $TAG != crate version v$CRATE_VER (Cargo.toml) — aborting release" >&2
  echo "portal: bump [workspace.package] version to ${TAG#v}, commit, then re-run" >&2
  exit 1
fi

# --- 0b. Tree must be committed AND pushed (fails-closed) ------------------
# Two distinct ways a release can ship code that nobody can find again:
#   dirty tree  -> the binary is built from working-copy code, but the SHA
#                  stamped into it (and reported by --version, and pinned by
#                  the agent handshake) comes from HEAD. The stamp lies.
#   unpushed    -> `gh release create` below passes no --target, so GitHub cuts
#                  the tag from the REMOTE default branch. A local-only commit
#                  gets a tag pointing at its parent: published bytes and
#                  tagged source silently disagree.
# `local` (the INSTALL=1 test flow) is exempt on purpose — building uncommitted
# work is the entire point there, and it publishes nothing.
if [ "$TAG" != "local" ]; then
  if ! git -C "$ROOT" diff-index --quiet HEAD --; then
    echo "portal: uncommitted changes to tracked files — aborting release" >&2
    echo "portal: the SHA stamp comes from HEAD; commit first, then re-run" >&2
    git -C "$ROOT" status --short >&2
    exit 1
  fi
  BRANCH="$(git -C "$ROOT" symbolic-ref --quiet --short HEAD || echo HEAD)"
  git -C "$ROOT" fetch --quiet origin "$BRANCH" 2>/dev/null || true
  LOCAL_HEAD="$(git -C "$ROOT" rev-parse HEAD)"
  REMOTE_HEAD="$(git -C "$ROOT" rev-parse "origin/$BRANCH" 2>/dev/null || echo missing)"
  if [ "$LOCAL_HEAD" != "$REMOTE_HEAD" ]; then
    echo "portal: HEAD is not pushed to origin/$BRANCH — aborting release" >&2
    echo "portal: gh cuts the tag from the remote branch, so it would tag the wrong commit" >&2
    echo "portal: run: git push origin $BRANCH" >&2
    exit 1
  fi
fi

# --- 1+2. Build (agents + Mac binary, embed-verified) -----------------------
# ONE build path: the Makefile owns cross-compile + embed + verification, so
# a portal binary without embedded agents cannot come out of any flow.
AUTO_APP_MIGRATION=1
[ "$TAG" = "local" ] && AUTO_APP_MIGRATION=0
PORTAL_SIGNED=1 make --no-print-directory build SHA="$SHA" PORTAL_AUTO_APP_MIGRATION="$AUTO_APP_MIGRATION"

# --- 3. Sign (Developer ID, hardened runtime) ------------------------------
DEVELOPER_ID="${DEVELOPER_ID:-$(security find-identity -v -p codesigning | grep -o 'Developer ID Application: [^"]*' | head -1)}"
[ -n "$DEVELOPER_ID" ] || { echo "no Developer ID Application identity in keychain" >&2; exit 1; }
echo "==> signing with: $DEVELOPER_ID"
codesign --sign "$DEVELOPER_ID" --options runtime --timestamp --force \
  --entitlements "$ROOT/portal.entitlements" "$BIN"
codesign --verify --deep --strict --verbose=2 "$BIN"

# Assemble the app only after the single multi-mode executable is signed,
# then sign the complete bundle. The installed CLI points at the bundled
# launcher, which execs this same file with --cli.
"$ROOT/scripts/package-app.sh" "$BIN" "$APP"
codesign --sign "$DEVELOPER_ID" --options runtime --timestamp --force \
  --entitlements "$ROOT/portal.entitlements" "$APP/Contents/MacOS/Portal"
codesign --sign "$DEVELOPER_ID" --options runtime --timestamp --force \
  --entitlements "$ROOT/portal.entitlements" "$APP"
codesign --verify --deep --strict --verbose=2 "$APP"

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
MIGRATION_MODE="$("$BIN" _app-migration-mode)"
EXPECTED_MODE=enabled
[ "$TAG" = "local" ] && EXPECTED_MODE=disabled
[ "$MIGRATION_MODE" = "$EXPECTED_MODE" ] || {
  echo "portal: app migration bridge is $MIGRATION_MODE (want $EXPECTED_MODE)" >&2
  exit 1
}
SIGNED_MODE="$("$BIN" _signed-build-mode)"
[ "$SIGNED_MODE" = enabled ] || {
  echo "portal: signed credential build mode is $SIGNED_MODE (want enabled)" >&2
  exit 1
}
# LAContext is reached through objc2's dynamic Objective-C class lookup, so a
# static Rust archive can link successfully even when the final Swift binary
# omitted LocalAuthentication.framework. Exercise the real initialization path
# before notarization; that missing load command otherwise panics only when a
# remote sudo request arrives.
KEYCHAIN_SMOKE="$("$BIN" --cli keychain list)" || {
  echo "portal: signed credential runtime failed to initialize — aborting release" >&2
  exit 1
}
case "$KEYCHAIN_SMOKE" in
  "touch id: "*) echo "==> signed credential runtime initializes" ;;
  *) echo "portal: unexpected keychain smoke output — aborting release" >&2; exit 1 ;;
esac

# --- 4. Notarize the application bundle ------------------------------------
NOTARY_KEY_ID="${NOTARY_KEY_ID:?set NOTARY_KEY_ID in .env (App Store Connect key id)}"
NOTARY_ISSUER="${NOTARY_ISSUER:?set NOTARY_ISSUER in .env (issuer UUID)}"
NOTARY_KEY="${NOTARY_KEY:-$HOME/Downloads/AuthKey_${NOTARY_KEY_ID}.p8}"
[ -f "$NOTARY_KEY" ] || { echo "notary key not found: $NOTARY_KEY" >&2; exit 1; }
echo "==> notarizing Portal.app (key $NOTARY_KEY_ID)"
ditto -c -k --keepParent "$APP" "$NOTARY_ZIP"
NOTARY_OUT="$(xcrun notarytool submit "$NOTARY_ZIP" \
  --key "$NOTARY_KEY" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER" \
  --wait --timeout 20m)"
echo "$NOTARY_OUT" | tail -4
echo "$NOTARY_OUT" | grep -q "status: Accepted" || {
  echo "portal: app notarization NOT accepted" >&2
  echo "$NOTARY_OUT" | grep -iE "status|issue|error" >&2 || true
  exit 1
}
xcrun stapler staple "$APP"
xcrun stapler validate "$APP"

# Pre-app upgraders still fetch the standalone bridge artifact. It is signed
# separately from the app executable, so submit it explicitly.
rm -f "$NOTARY_ZIP"
ditto -c -k --keepParent "$BIN" "$NOTARY_ZIP"
BIN_NOTARY_OUT="$(xcrun notarytool submit "$NOTARY_ZIP" \
  --key "$NOTARY_KEY" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER" \
  --wait --timeout 20m)"
echo "$BIN_NOTARY_OUT" | tail -4
echo "$BIN_NOTARY_OUT" | grep -q "status: Accepted" || {
  echo "portal: standalone binary notarization NOT accepted" >&2
  echo "$BIN_NOTARY_OUT" | grep -iE "status|issue|error" >&2 || true
  exit 1
}

# The downloadable archive preserves the signed+stapled bundle. The DMG is
# the normal drag-to-Applications experience and receives its own ticket.
rm -f "$APP_ARTIFACT" "$DMG_ARTIFACT"
ditto -c -k --keepParent "$APP" "$APP_ARTIFACT"
DMG_STAGE="$(mktemp -d -t portal-dmg)"
cp -R "$APP" "$DMG_STAGE/Portal.app"
ln -s /Applications "$DMG_STAGE/Applications"
hdiutil create -quiet -volname Portal -srcfolder "$DMG_STAGE" -ov -format UDZO "$DMG_ARTIFACT"
rm -rf "$DMG_STAGE"
echo "==> notarizing Portal DMG"
DMG_NOTARY_OUT="$(xcrun notarytool submit "$DMG_ARTIFACT" \
  --key "$NOTARY_KEY" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER" \
  --wait --timeout 20m)"
echo "$DMG_NOTARY_OUT" | tail -4
echo "$DMG_NOTARY_OUT" | grep -q "status: Accepted" || {
  echo "portal: DMG notarization NOT accepted" >&2
  echo "$DMG_NOTARY_OUT" | grep -iE "status|issue|error" >&2 || true
  exit 1
}
xcrun stapler staple "$DMG_ARTIFACT"
xcrun stapler validate "$DMG_ARTIFACT"
# The legacy single-file binary cannot be stapled; its ticket lives in cloud.

# --- 5. Stage artifacts + minisign ------------------------------------------
# Compatibility assets remain mandatory for pre-app upgraders: v2 CLI builds
# fetch $ARTIFACT, while v1 hardcodes portal-darwin-arm64. The release bridge
# they install automatically finishes migration to the app archive.
ART="$OUT/$ARTIFACT"
V1ART="$OUT/portal-darwin-arm64"
cp "$BIN" "$ART"
cp "$BIN" "$V1ART"
MINISIGN_KEY="${MINISIGN_KEY:-$HOME/.portal-minisign.key}"
rm -f "$ART.minisig" "$V1ART.minisig" "$APP_ARTIFACT.minisig" "$DMG_ARTIFACT.minisig"
if command -v minisign >/dev/null && [ -f "$MINISIGN_KEY" ]; then
  echo "==> minisign"
  minisign -S -s "$MINISIGN_KEY" -x "$ART.minisig" -m "$ART"
  minisign -S -s "$MINISIGN_KEY" -x "$V1ART.minisig" -m "$V1ART"
  minisign -S -s "$MINISIGN_KEY" -x "$APP_ARTIFACT.minisig" -m "$APP_ARTIFACT"
  minisign -S -s "$MINISIGN_KEY" -x "$DMG_ARTIFACT.minisig" -m "$DMG_ARTIFACT"
else
  echo "portal: minisign unavailable ($MINISIGN_KEY missing?) — aborting release" >&2
  exit 1
fi

# --- 6. Publish or install --------------------------------------------------
if [ "${INSTALL:-0}" = "1" ]; then
  : "${INSTALL_HOST:?set INSTALL_HOST=<ssh-host> to install locally}"
  echo "==> installing local signed Portal.app (no publish)"
  "$BIN" _install-verified-app "$APP" "v$CRATE_VER"

  # Add/converge the requested smoke-test box through the app-owned CLI. A
  # repeated local release is intentionally idempotent: an existing matching
  # box is restarted rather than turning a healthy reinstall into a failure.
  INSTALLED="$HOME/.local/bin/portal"
  set +e
  INSTALL_OUT="$("$INSTALLED" install "$INSTALL_HOST" 2>&1)"
  INSTALL_CODE=$?
  set -e
  if [ "$INSTALL_CODE" -ne 0 ]; then
    case "$INSTALL_OUT" in
      *"already exists with host"*)
        echo "==> smoke-test box already configured; restarting services"
        "$INSTALLED" restart
        ;;
      *) printf '%s\n' "$INSTALL_OUT" >&2; exit "$INSTALL_CODE" ;;
    esac
  fi

  # A command-line smoke test cannot catch launchd's cached code-requirement
  # failures. Assert the app-owned launcher resolves to the installed bundle,
  # the exact signed executable survived the transaction, and BOTH freshly
  # registered jobs are running.
  CLI_TARGET="$(readlink "$INSTALLED")"
  INSTALLED_APP="${CLI_TARGET%%.app/*}.app"
  INSTALLED_EXECUTABLE="$INSTALLED_APP/Contents/MacOS/Portal"
  cmp "$APP/Contents/MacOS/Portal" "$INSTALLED_EXECUTABLE" || {
    echo "portal: installed app executable differs from signed app artifact" >&2
    exit 1
  }
  codesign --verify --deep --strict --verbose=2 "$INSTALLED_APP"
  "$INSTALLED" status >/dev/null
  UID_NOW="$(id -u)"
  for LABEL in local.portal.autoforward local.portal.tray; do
    STATE="$(launchctl print "gui/$UID_NOW/$LABEL" | awk '$1 == "state" && $2 == "=" { print $3; exit }')"
    [ "$STATE" = "running" ] || {
      echo "portal: $LABEL is not running after signed install (state=${STATE:-missing})" >&2
      launchctl print "gui/$UID_NOW/$LABEL" >&2 || true
      exit 1
    }
  done
  echo "==> signed LaunchAgents healthy; waiting for remote convergence"
  # The local API is ready before SSH/agent/forward convergence by design.
  # Retry the real doctor condition until it passes or a bounded deadline is
  # reached. Each doctor attempt performs its own SSH probe, which naturally
  # paces this loop; there is no arbitrary post-install sleep.
  DOCTOR_DEADLINE=$((SECONDS + 45))
  DOCTOR_OUT=""
  until DOCTOR_OUT="$("$INSTALLED" doctor 2>&1)"; do
    if (( SECONDS >= DOCTOR_DEADLINE )); then
      echo "portal: doctor did not pass within the convergence deadline" >&2
      printf '%s\n' "$DOCTOR_OUT" >&2
      exit 1
    fi
  done
  printf '%s\n' "$DOCTOR_OUT"

  # Exercise the real transactional rollback path after both the app and CLI
  # link have been swapped. The injected failure is deliberately later than
  # the filesystem replacement and must restore byte-identical signed bytes,
  # the original launcher target, manifests, and healthy LaunchAgents.
  echo "==> injecting post-swap update failure to verify rollback"
  BEFORE_APP_SHA="$(shasum -a 256 "$INSTALLED_EXECUTABLE" | awk '{print $1}')"
  BEFORE_CLI_TARGET="$(readlink "$INSTALLED")"
  set +e
  ROLLBACK_OUT="$(PORTAL_INSTALL_FAULT=after-cli-swap \
    "$INSTALLED_EXECUTABLE" --cli _install-verified-app "$APP" "v$CRATE_VER" 2>&1)"
  ROLLBACK_CODE=$?
  set -e
  [ "$ROLLBACK_CODE" -ne 0 ] || {
    echo "portal: injected update failure unexpectedly succeeded" >&2
    exit 1
  }
  case "$ROLLBACK_OUT" in
    *"previous installation restored"*) ;;
    *) echo "portal: rollback did not report restoration: $ROLLBACK_OUT" >&2; exit 1 ;;
  esac
  AFTER_APP_SHA="$(shasum -a 256 "$INSTALLED_EXECUTABLE" | awk '{print $1}')"
  AFTER_CLI_TARGET="$(readlink "$INSTALLED")"
  [ "$AFTER_APP_SHA" = "$BEFORE_APP_SHA" ] || {
    echo "portal: rollback changed the installed app executable" >&2
    exit 1
  }
  [ "$AFTER_CLI_TARGET" = "$BEFORE_CLI_TARGET" ] || {
    echo "portal: rollback changed the installed CLI target" >&2
    exit 1
  }
  codesign --verify --deep --strict --verbose=2 "$INSTALLED_APP"
  for LABEL in local.portal.autoforward local.portal.tray; do
    STATE="$(launchctl print "gui/$UID_NOW/$LABEL" | awk '$1 == "state" && $2 == "=" { print $3; exit }')"
    [ "$STATE" = "running" ] || {
      echo "portal: $LABEL did not recover after rollback (state=${STATE:-missing})" >&2
      exit 1
    }
  done
  "$INSTALLED" status >/dev/null
  echo "==> local signed install and rollback verified"
  exit 0
fi
echo "==> publishing $TAG"
ASSETS=("$DMG_ARTIFACT" "$APP_ARTIFACT" "$ART" "$V1ART")
[ -f "$DMG_ARTIFACT.minisig" ] && ASSETS+=("$DMG_ARTIFACT.minisig")
[ -f "$APP_ARTIFACT.minisig" ] && ASSETS+=("$APP_ARTIFACT.minisig")
[ -f "$ART.minisig" ] && ASSETS+=("$ART.minisig")
[ -f "$V1ART.minisig" ] && ASSETS+=("$V1ART.minisig")
if gh release view "$TAG" >/dev/null 2>&1; then
  gh release upload "$TAG" --clobber "${ASSETS[@]}"
else
  # Keep the release invisible to /releases/latest until every mutually
  # dependent app + compatibility asset is uploaded. Otherwise an old updater
  # could install the bridge while the app archive is not available yet.
  gh release create "$TAG" --draft --title "portal $TAG" --generate-notes "${ASSETS[@]}"
  gh release edit "$TAG" --draft=false
fi
echo "==> done: $TAG published"
gh release view "$TAG" --json url -q .url
