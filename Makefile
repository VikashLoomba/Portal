# Portal build orchestration. Two binaries:
#   portald (linux-amd64) — embedded into portal via go:embed
#   portal  (host-native) — the user-facing CLI
#
# Both binaries are stamped with the SAME git SHA at build time:
#   - portald via -X main.gitSHA=$(GIT_SHA) (reported in HelloAck)
#   - portal  via -X .../bootstrap.gitSHA=$(GIT_SHA) (used to name the
#     remote cache file ~/.cache/portal/agent-<sha>)
# Mismatch is impossible because Make sets GIT_SHA once per invocation.

GIT_SHA       := $(shell git rev-parse HEAD 2>/dev/null || echo dev-$(shell date +%s))
AGENT_DIR     := internal/bootstrap/agent
AGENT_PATH    := $(AGENT_DIR)/portald-linux-amd64
SHA_PATH      := $(AGENT_DIR)/sha.txt

LDFLAGS_AGENT  := -s -w -X main.gitSHA=$(GIT_SHA)
LDFLAGS_PORTAL := -X github.com/vikashl/portal/internal/bootstrap.gitSHA=$(GIT_SHA)

# Cross-compilation target for the Mac client binary.
# The agent is always linux-amd64 (it runs on the dev box).
# The Mac client ships as darwin-arm64 (Apple Silicon only).
PORTAL_DARWIN_ARM64  := portal-darwin-arm64

.PHONY: build agent portal portal-all test clean print-sha

build: portal

agent:
	@mkdir -p $(AGENT_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS_AGENT)" \
		-o $(AGENT_PATH) ./cmd/portald
	@printf "%s" "$(GIT_SHA)" > $(SHA_PATH)
	@echo "built agent $(AGENT_PATH) (sha=$(GIT_SHA), $$(stat -f%z $(AGENT_PATH) 2>/dev/null || stat -c%s $(AGENT_PATH)) bytes)"

portal: agent
	go build -trimpath -ldflags "$(LDFLAGS_PORTAL)" -o portal ./cmd/portal
	@echo "built portal (sha=$(GIT_SHA))"

# portal-all builds the Apple Silicon Mac binary — used by CI to produce the
# release artifact.
portal-all: agent
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS_PORTAL)" \
		-o $(PORTAL_DARWIN_ARM64) ./cmd/portal
	@echo "built $(PORTAL_DARWIN_ARM64) (sha=$(GIT_SHA))"

test: agent
	go test ./...

clean:
	rm -f portal $(AGENT_PATH) $(SHA_PATH)

print-sha:
	@echo $(GIT_SHA)
