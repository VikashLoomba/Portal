// Package bootstrap embeds supported Linux agent binaries into the Mac
// `portal` binary at compile time and uploads the selected artifact atomically
// to the dev box at first connect. The build's git SHA is baked into all
// binaries; that SHA is the version token — a mismatch means a stale upload
// and triggers re-upload.
package bootstrap

import (
	_ "embed"
	"strings"
)

// agentBinaryAMD64 is the linux-amd64 agent, produced by `make agent`.
// A real build (`make build`) always runs `make agent` first so go:embed
// has both architecture artifacts available.
//
//go:embed agent/portald-linux-amd64
var agentBinaryAMD64 []byte

// agentBinaryARM64 is the linux-arm64 agent, produced by `make agent`.
//
//go:embed agent/portald-linux-arm64
var agentBinaryARM64 []byte

// agentSHARaw is the build SHA written by `make agent` alongside the
// binary. Read at init time and checked against the linker-injected
// gitSHA below — must match.
//
//go:embed agent/sha.txt
var agentSHARaw string

// gitSHA is set via -ldflags "-X github.com/VikashLoomba/Portal/internal/bootstrap.gitSHA=..."
// by the Makefile. Should always equal strings.TrimSpace(agentSHARaw).
var gitSHA = "dev"

// EmbeddedSHA returns the git SHA the embedded agent was built from.
func EmbeddedSHA() string { return strings.TrimSpace(agentSHARaw) }

// EmbeddedAgent returns the embedded amd64 agent binary bytes for legacy callers.
func EmbeddedAgent() []byte { return agentBinaryAMD64 }

// LinkedSHA returns the SHA injected via -ldflags. Useful for diagnostics
// when the two SHAs don't match (which would indicate a build problem).
func LinkedSHA() string { return gitSHA }

func init() {
	// Catch build drift early (e.g. `go build` without `make agent`): the
	// SHA written to sha.txt at build time must match the one injected via
	// -ldflags. A mismatch means the embedded agent and the Mac binary came
	// from different commits, which would cause a SHA-mismatch on HelloAck.
	// Allow "dev" as the ldflags default during local `go run` / `go test`.
	if gitSHA != "dev" && gitSHA != "" {
		embedded := EmbeddedSHA()
		if embedded != "" && embedded != gitSHA {
			panic("portal: embedded agent SHA (" + embedded + ") does not match -ldflags SHA (" + gitSHA + "); run `make build`")
		}
	}
}
