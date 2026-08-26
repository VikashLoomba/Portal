# Portal build orchestration. One macOS application executable plus two
# embedded Linux agents:
#   portald (linux musl amd64/arm64) — embedded into PortalFFI at build time
#   Portal  (Swift arm64 macOS)      — GUI + Rust-backed CLI/daemon modes
#
# Every mode and embedded agent is stamped with the SAME git SHA per
# invocation. `make build` is THE build path: it packages the static BoltFFI
# XCFramework, compiles the Swift host, and verifies both agents landed in the
# final application executable.
#
# Targets:
#   make build                 agents + PortalFFI + Swift executable
#   make ffi                   agents + static macOS XCFramework
#   make app                   build + assemble unsigned Portal.app
#   make dmg                   app + assemble unsigned drag-to-install DMG
#   make test                  workspace tests
#   make lint                  clippy -D warnings + rustfmt check
#   make check                 test + lint (release prerequisite)
#   make install HOST=<ssh>    dev install (ad-hoc signed) + agent reload
#   make release TAG=v2.x.y    sign + exec-gate + notarize + publish
#   make release-install HOST=<ssh>   signed local install, no publish
#   make clean

# Defense in depth: rustup's shims must win even if a Homebrew rust ever
# reappears (its cargo has no musl std, ignores rust-toolchain.toml, and
# poisons target/ with mixed-compiler artifacts — E0514). Homebrew rust was
# uninstalled 2026-08-05; see AGENTS.md.
export PATH := $(HOME)/.cargo/bin:$(PATH)

SHA        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
# Release compatibility binary only: after an old upgrader installs it, the
# daemon completes migration to the app artifact. Source builds stay offline.
PORTAL_AUTO_APP_MIGRATION ?= 0
DARWIN     := aarch64-apple-darwin
MUSL_AMD64 := x86_64-unknown-linux-musl
MUSL_ARM64 := aarch64-unknown-linux-musl
AGENTS     := target/agents
SWIFT_BIN  := target/swift/arm64-apple-macosx/release/Portal
BIN        := $(SWIFT_BIN)
APP        := target/$(DARWIN)/release/Portal.app
DMG        := target/$(DARWIN)/release/Portal.dmg

.PHONY: build ffi app dmg agents verify-embed test lint check install release release-install clean

build: ffi
	@echo "==> building Portal SwiftUI host (darwin-arm64, sha $(SHA))"
	swift build -c release --package-path native --scratch-path target/swift
	@$(MAKE) --no-print-directory verify-embed

ffi: agents
	@echo "==> packaging PortalFFI (macOS arm64, sha $(SHA))"
	PORTAL_GIT_SHA="$(SHA)" \
	PORTAL_AUTO_APP_MIGRATION="$(PORTAL_AUTO_APP_MIGRATION)" \
	PORTAL_AGENT_AMD64_FILE="$(CURDIR)/$(AGENTS)/portald-$(MUSL_AMD64)" \
	PORTAL_AGENT_ARM64_FILE="$(CURDIR)/$(AGENTS)/portald-$(MUSL_ARM64)" \
	./scripts/build-portal-ffi.sh

agents:
	@command -v cargo-zigbuild >/dev/null || { echo "install: cargo install cargo-zigbuild (and brew install zig)" >&2; exit 1; }
	@mkdir -p $(AGENTS)
	@for t in $(MUSL_AMD64) $(MUSL_ARM64); do \
		echo "==> building portald ($$t, sha $(SHA))"; \
		PORTAL_GIT_SHA="$(SHA)" cargo zigbuild --release -p portald --target $$t --quiet || exit 1; \
		cp "target/$$t/release/portald" "$(AGENTS)/portald-$$t" || exit 1; \
	done

# The daemon cannot provision boxes without both embedded portald payloads.
# Assert they landed in the final Swift application executable, not merely in
# an intermediate Rust archive.
verify-embed:
	@python3 -c 'import sys; d=open(sys.argv[1],"rb").read(); agents=[open(p,"rb").read() for p in sys.argv[2:]]; missing=[p for p,a in zip(sys.argv[2:],agents) if a[:4096] not in d]; sys.exit("Portal: embedded agent bytes NOT found: "+", ".join(missing) if missing else 0)' "$(BIN)" "$(AGENTS)/portald-$(MUSL_AMD64)" "$(AGENTS)/portald-$(MUSL_ARM64)"
	@echo "==> $(BIN) (both embedded agents verified)"

app: build
	@./scripts/package-app.sh "$(BIN)" "$(APP)"
	@./scripts/verify-portal-app.sh "$(APP)"

dmg: app
	@rm -f "$(DMG)"
	@STAGE="$$(mktemp -d -t portal-dmg)"; \
	trap 'rm -rf "$$STAGE"' EXIT; \
	cp -R "$(APP)" "$$STAGE/Portal.app"; \
	ln -s /Applications "$$STAGE/Applications"; \
	hdiutil create -quiet -volname Portal -srcfolder "$$STAGE" -ov -format UDZO "$(DMG)"
	@echo "==> $(DMG)"

test: app
	cargo test --workspace
	swift test --package-path native --scratch-path target/swift
	./scripts/test-cli-launcher.sh
	./scripts/test-native-app-e2e.sh "$(APP)"
	./scripts/test-native-gui-lifecycle.sh "$(APP)"
	./scripts/test-native-prompt.sh "$(APP)"

lint: ffi
	cargo clippy --workspace --all-targets -- -D warnings
	cargo fmt --all --check
	swiftformat --lint --swift-version 6.0 native/Sources/PortalApp native/Sources/PortalFFI native/Tests/PortalFFITests
	./scripts/verify-swift-boundary.sh

# Every distributable path runs the same correctness gates first. release.sh
# still owns the one-SHA cross-build/sign/notarize/publish transaction.
check: test lint

install: app
	@test -n "$(HOST)" || { echo "usage: make install HOST=<ssh-host>" >&2; exit 1; }
	"$(APP)/Contents/MacOS/Portal" --cli install "$(HOST)"

release: check
	@test -n "$(TAG)" || { echo "usage: make release TAG=v2.x.y" >&2; exit 1; }
	./release.sh "$(TAG)"

release-install: check
	@test -n "$(HOST)" || { echo "usage: make release-install HOST=<ssh-host>" >&2; exit 1; }
	INSTALL=1 INSTALL_HOST="$(HOST)" ./release.sh local

clean:
	cargo clean
	rm -rf target/swift native/Dependencies native/Generated native/Sources/PortalFFIGenerated
