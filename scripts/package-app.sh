#!/bin/bash
# Assemble the native SwiftUI Portal.app around the one statically linked
# Swift/Rust executable. Signing and notarization remain release.sh's job.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${1:-$ROOT/target/swift/arm64-apple-macosx/release/Portal}"
APP="${2:-$ROOT/target/aarch64-apple-darwin/release/Portal.app}"
VERSION="$(sed -n 's/^version = "\(.*\)"/\1/p' "$ROOT/Cargo.toml" | head -1)"
ICON="$ROOT/assets/Portal.icns"
CLI_LAUNCHER="$ROOT/native/Resources/bin/portal"

[ -x "$BIN" ] || { echo "portal app: executable not found: $BIN" >&2; exit 1; }
[ -f "$ICON" ] || { echo "portal app: icon not found: $ICON" >&2; exit 1; }
[ -x "$CLI_LAUNCHER" ] || { echo "portal app: CLI launcher not found: $CLI_LAUNCHER" >&2; exit 1; }

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin"
cp "$BIN" "$APP/Contents/MacOS/Portal"
cp "$CLI_LAUNCHER" "$APP/Contents/Resources/bin/portal"
cp "$ICON" "$APP/Contents/Resources/Portal.icns"
chmod 0755 "$APP/Contents/MacOS/Portal" "$APP/Contents/Resources/bin/portal"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleDevelopmentRegion</key><string>en</string>
    <key>CFBundleExecutable</key><string>Portal</string>
    <key>CFBundleIdentifier</key><string>com.vikashloomba.portal</string>
    <key>CFBundleIconFile</key><string>Portal.icns</string>
    <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
    <key>CFBundleName</key><string>Portal</string>
    <key>CFBundleDisplayName</key><string>Portal</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>$VERSION</string>
    <key>CFBundleVersion</key><string>$VERSION</string>
    <key>LSMinimumSystemVersion</key><string>13.0</string>
    <key>NSHighResolutionCapable</key><true/>
    <key>NSSupportsAutomaticGraphicsSwitching</key><true/>
    <key>NSPrincipalClass</key><string>NSApplication</string>
</dict>
</plist>
PLIST
printf 'APPL????' > "$APP/Contents/PkgInfo"
plutil -lint "$APP/Contents/Info.plist" >/dev/null

# The final bundle has one compiled executable. The app-owned `portal`
# command is deliberately a script that execs this file with --cli.
compiled_count="$(find "$APP/Contents" -type f -perm -111 ! -path '*/Resources/bin/portal' | wc -l | tr -d ' ')"
[ "$compiled_count" = "1" ] || {
  echo "portal app: expected exactly one compiled executable, found $compiled_count" >&2
  exit 1
}

echo "==> $APP"
