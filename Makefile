# portal build orchestration. Three binaries:
#   portald (linux musl amd64/arm64) — embedded into portal at build time
#   portal  (darwin-arm64)           — the user-facing CLI + daemon
#
# Both are stamped with the SAME git SHA per invocation: the HelloAck SHA
# match and the ~/.cache/portal/agent-<sha> remote path both depend on it,
# and a mismatched pair reconnect-loops.
# `make build` is THE build path: release.sh delegates here, so a portal
# binary without embedded agents cannot come out of any supported flow.
#
# Targets:
#   make build                 agents + darwin binary (embedded, verified)
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
BIN        := target/$(DARWIN)/release/portal
APP        := target/$(DARWIN)/release/Portal.app
DMG        := target/$(DARWIN)/release/Portal.dmg

.PHONY: build app dmg agents verify-embed test lint check install release release-install clean

build: agents
	@echo "==> building portal (darwin-arm64, agents embedded, sha $(SHA))"
	PORTAL_GIT_SHA="$(SHA)" \
	PORTAL_AUTO_APP_MIGRATION="$(PORTAL_AUTO_APP_MIGRATION)" \
	PORTAL_AGENT_AMD64_FILE="$(CURDIR)/$(AGENTS)/portald-$(MUSL_AMD64)" \
	PORTAL_AGENT_ARM64_FILE="$(CURDIR)/$(AGENTS)/portald-$(MUSL_ARM64)" \
	cargo build --release -p portal-cli --target $(DARWIN) --quiet
	@$(MAKE) --no-print-directory verify-embed

agents:
	@command -v cargo-zigbuild >/dev/null || { echo "install: cargo install cargo-zigbuild (and brew install zig)" >&2; exit 1; }
	@mkdir -p $(AGENTS)
	@for t in $(MUSL_AMD64) $(MUSL_ARM64); do \
		echo "==> building portald ($$t, sha $(SHA))"; \
		PORTAL_GIT_SHA="$(SHA)" cargo zigbuild --release -p portald --target $$t --quiet || exit 1; \
		cp "target/$$t/release/portald" "$(AGENTS)/portald-$$t" || exit 1; \
	done

# The daemon cannot provision boxes without the embedded portald bytes —
# assert they actually landed in the Mac binary (fail here, not at the
# first user's reconnect loop).
verify-embed:
	@python3 -c 'import sys; d=open(sys.argv[1],"rb").read(); a=open(sys.argv[2],"rb").read(); sys.exit(0 if a[:4096] in d else "portal: embedded agent bytes NOT found in binary")' "$(BIN)" "$(AGENTS)/portald-$(MUSL_AMD64)"
	@echo "==> $(BIN) (embedded agents verified)"

app: build
	@./scripts/package-app.sh "$(BIN)" "$(APP)"

dmg: app
	@rm -f "$(DMG)"
	@STAGE="$$(mktemp -d -t portal-dmg)"; \
	trap 'rm -rf "$$STAGE"' EXIT; \
	cp -R "$(APP)" "$$STAGE/Portal.app"; \
	ln -s /Applications "$$STAGE/Applications"; \
	hdiutil create -quiet -volname Portal -srcfolder "$$STAGE" -ov -format UDZO "$(DMG)"
	@echo "==> $(DMG)"

test:
	cargo test --workspace

lint:
	cargo clippy --workspace --all-targets -- -D warnings
	cargo fmt --all --check

# Every distributable path runs the same correctness gates first. release.sh
# still owns the one-SHA cross-build/sign/notarize/publish transaction.
check: test lint

install: build
	@test -n "$(HOST)" || { echo "usage: make install HOST=<ssh-host>" >&2; exit 1; }
	"$(BIN)" install "$(HOST)"

release: check
	@test -n "$(TAG)" || { echo "usage: make release TAG=v2.x.y" >&2; exit 1; }
	./release.sh "$(TAG)"

release-install: check
	@test -n "$(HOST)" || { echo "usage: make release-install HOST=<ssh-host>" >&2; exit 1; }
	INSTALL=1 INSTALL_HOST="$(HOST)" ./release.sh local

clean:
	cargo clean
