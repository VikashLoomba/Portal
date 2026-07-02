package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/VikashLoomba/Portal/internal/clipshim"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// agentDigest caches the sha256 of EmbeddedAgent() so the probe can
// compare CONTENT, not just length. Without this a same-size on-disk file
// (partial-write leftover, attacker swap, non-deterministic rebuild that
// happened to land on the same length) would silently bypass re-upload.
var (
	agentDigestOnce sync.Once
	agentDigest     string
)

func embeddedSHA256() string {
	agentDigestOnce.Do(func() {
		sum := sha256.Sum256(EmbeddedAgent())
		agentDigest = hex.EncodeToString(sum[:])
	})
	return agentDigest
}

// shellQuoted wraps a shell script in single quotes, escaping any embedded
// single quote with the standard close-escape-reopen sh idiom (the
// ReplaceAll below). Required because ssh joins every argv argument after
// the host with spaces and runs the result on the remote via sh -c. Without
// quoting, a multi-token script gets re-tokenized and only the first word
// survives.
func shellQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// EmbeddedSHA exposes the package-level EmbeddedSHA function as a method
// so callers that only have a *Manager (not the package) can access it.
func (m *Manager) EmbeddedSHA() string { return EmbeddedSHA() }

// remoteDir is where the agent binaries live on the dev box. We use
// ~/.cache/portal/ rather than /tmp/ so they survive reboots — saves the
// ~3MB upload after every reboot. The dir is created mode 0700.
const remoteDir = "~/.cache/portal"

// Manager handles the embedded-agent → remote-cache lifecycle:
//  1. Probe for the right SHA already at ~/.cache/portal/agent-<sha>.
//  2. If missing or wrong size, atomically upload via `cat > .tmp.$$ ; mv`.
//  3. Best-effort prune older agent-* files.
type Manager struct {
	T   transport.Transport
	Log *slog.Logger
}

// New constructs a Manager. If log is nil, slog.Default() is used.
func New(t transport.Transport, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{T: t, Log: log}
}

// EnsureUploaded probes the dev box for the right agent binary and uploads
// it if missing or content-mismatched. Returns the absolute remote path of
// the agent (with $HOME expanded by the remote shell).
//
// Probe contract: "right binary at this path" means BOTH (a) byte-count
// matches and (b) sha256sum matches. Length-only verification (which we
// did initially) was insufficient because a same-size on-disk file —
// truncated upload leftover, attacker swap, non-deterministic rebuild
// landing at the same length — would silently bypass re-upload.
//
// Upload contract: ATOMIC. The script writes to a uniquely-named .tmp
// file, asserts the byte count matches expected, then renames into place.
// A trap on EXIT removes the .tmp on any abnormal exit (network drop,
// half-close, ctx cancel, kill). The previous canonical binary is left
// intact until the final mv lands, so a partial transfer never produces
// a corrupt agent at the canonical path.
func (m *Manager) EnsureUploaded(ctx context.Context) (string, error) {
	sha := EmbeddedSHA()
	if sha == "" {
		return "", fmt.Errorf("bootstrap: embedded agent has no SHA — `make agent` must run before build")
	}
	agent := EmbeddedAgent()
	if len(agent) == 0 {
		return "", fmt.Errorf("bootstrap: embedded agent is empty — `make agent` must run before build")
	}

	remotePath := remoteDir + "/agent-" + sha
	expectedSize := strconv.Itoa(len(agent))
	expectedDigest := embeddedSHA256()

	// 1. Probe by content hash. We still gate on length first because
	// sha256sum on a misshapen file is wasted IO. The probe prints a
	// SINGLE line "<size> <digest>" or "MISSING" so parsing is trivial.
	// Portable sha256 probe: tries sha256sum (Linux), then sha256 -q (FreeBSD/macOS),
	// then openssl dgst -sha256 as a last resort.
	probe := fmt.Sprintf(
		`test -x %s && printf '%%s %%s' "$(stat -c %%s %s 2>/dev/null || stat -f %%z %s)" "$(sha256sum %s 2>/dev/null | awk '{print $1}' || sha256 -q %s 2>/dev/null || openssl dgst -sha256 -hex %s 2>/dev/null | awk '{print $NF}')" || echo MISSING`,
		remotePath, remotePath, remotePath, remotePath, remotePath, remotePath,
	)
	out, _, err := m.T.Exec(ctx, nil, "bash", "-c", shellQuoted(probe))
	if err == nil {
		got := strings.TrimSpace(out)
		want := expectedSize + " " + expectedDigest
		if got == want {
			m.Log.Debug("agent already present", "remote", remotePath, "sha", sha[:min(8, len(sha))])
			return remotePath, nil
		}
	}

	// 2. Upload atomically with size-verification before rename.
	m.Log.Info("uploading agent", "remote", remotePath, "sha", sha[:min(8, len(sha))], "bytes", len(agent))
	script := fmt.Sprintf(
		// install -d creates the directory atomically at 0700 in one syscall,
		// avoiding the mkdir-then-chmod window where the dir is briefly 0755.
		`set -e; install -d -m 0700 %s && tmp=$(mktemp %s/.agent.tmp.XXXXXX) && trap 'rm -f "$tmp"' EXIT && cat > "$tmp" && [ "$(wc -c < "$tmp")" = "%s" ] && chmod 0755 "$tmp" && mv "$tmp" %s && trap - EXIT`,
		remoteDir, remoteDir, expectedSize, remotePath,
	)
	if _, _, err := m.T.Exec(ctx, agent, "bash", "-c", shellQuoted(script)); err != nil {
		return "", fmt.Errorf("bootstrap: upload failed: %w", err)
	}

	// 3. Update the stable `portald` symlink so the xdg-open wrapper can
	// always find the current agent without knowing the SHA.
	symlink := fmt.Sprintf(`ln -sf %s %s/portald`, remotePath, remoteDir)
	_, _, _ = m.T.Exec(ctx, nil, "bash", "-c", shellQuoted(symlink))

	// 4. Best-effort prune older agent-* (older than 1 day) and any leftover
	// .agent.tmp.* fragments from earlier interrupted uploads.
	// Two separate find commands so -delete applies correctly to each predicate.
	// The original single find had operator-precedence issues: -delete bound
	// only to the last primary, silently never pruning old agent-* binaries.
	prune := fmt.Sprintf(
		`find %s -maxdepth 1 -name 'agent-*' ! -name 'agent-%s' -mtime +0 -delete 2>/dev/null; find %s -maxdepth 1 -name '.agent.tmp.*' -delete 2>/dev/null; true`,
		remoteDir, sha, remoteDir,
	)
	_, _, _ = m.T.Exec(ctx, nil, "bash", "-c", shellQuoted(prune))

	return remotePath, nil
}

// PruneAll removes every agent-* file and the clipboard-image cache dir from
// the remote cache dir, AND removes the clipboard read shims (xclip/wl-paste),
// the xdg-open wrapper, the portald symlink, the env snippet, and the
// PATH-prepend marker block — restoring any backed-up binaries (DESIGN §9.4).
// Called from `portal uninstall`. clipshim.Remove is idempotent, so calling it
// here in addition to removePortalWrappers (the CLI path) is harmless; this
// makes PruneAll self-contained for any caller that only has a *Manager.
func (m *Manager) PruneAll(ctx context.Context) error {
	clipshim.Remove(ctx, m.T)
	cmd := fmt.Sprintf(`rm -rf %s/agent-* %s/clip 2>/dev/null || true`, remoteDir, remoteDir)
	_, _, err := m.T.Exec(ctx, nil, "bash", "-c", shellQuoted(cmd))
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
