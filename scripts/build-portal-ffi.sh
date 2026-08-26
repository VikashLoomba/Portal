#!/bin/bash
# Generate and package Portal's macOS-only static BoltFFI boundary.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

EXPECTED="boltffi 0.30.1"
ACTUAL="$(boltffi --version 2>/dev/null || true)"
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "PortalFFI requires $EXPECTED; found ${ACTUAL:-nothing}" >&2
  echo "install: cargo install boltffi_cli --version 0.30.1 --locked" >&2
  exit 1
fi

boltffi check --apple

AGENT_AMD64="${PORTAL_AGENT_AMD64_FILE:-$ROOT/target/agents/portald-x86_64-unknown-linux-musl}"
AGENT_ARM64="${PORTAL_AGENT_ARM64_FILE:-$ROOT/target/agents/portald-aarch64-unknown-linux-musl}"
[ -s "$AGENT_AMD64" ] || { echo "PortalFFI: missing embedded amd64 agent: $AGENT_AMD64" >&2; exit 1; }
[ -s "$AGENT_ARM64" ] || { echo "PortalFFI: missing embedded arm64 agent: $AGENT_ARM64" >&2; exit 1; }

VERSION="$(sed -n 's/^version = "\(.*\)"/\1/p' "$ROOT/Cargo.toml" | head -1)"
[ -n "$VERSION" ] || { echo "PortalFFI: workspace version not found" >&2; exit 1; }

# BoltFFI 0.30.1 declares macOS 13 in its generated Package.swift but does
# not pass that deployment target to Cargo/cc while building a macOS slice.
# Setting the standard Apple build variable here ensures Rust dependencies
# with C/assembly objects are compatible with Portal's actual minimum OS.
MACOSX_DEPLOYMENT_TARGET=13.0 \
PORTAL_GIT_SHA="${PORTAL_GIT_SHA:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}" \
PORTAL_AGENT_AMD64_FILE="$AGENT_AMD64" \
PORTAL_AGENT_ARM64_FILE="$AGENT_ARM64" \
boltffi pack apple --release --deny-skipped --version "$VERSION"

"$ROOT/scripts/patch-boltffi-swift.sh"
