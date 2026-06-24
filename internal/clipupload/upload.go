// Package clipupload uploads a clipboard image to the remote dev box over
// an existing SSH ControlMaster and returns the remote path. Used by
// `portal ssh` when Ctrl+V is pressed with an image on the clipboard.
package clipupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
)

// RemoteDir is where uploaded clipboard images land on the dev box.
const RemoteDir = "~/.cache/portal/clip"

// MaxUploadBytes caps the interactive-paste payload before it crosses the
// ControlMaster (DESIGN §3). A multi-MB upload competes with the agent pipe
// for bandwidth and can stall heartbeats; larger clipboards fail fast and the
// shim falls through. Screenshots are well under this; 64 MiB pastes are not
// worth stalling heartbeats for.
const MaxUploadBytes = 8 << 20 // 8 MiB

// Upload writes pngData to RemoteDir/clip-<hash>.png on the remote via the
// transport's multiplexed ControlMaster and returns the remote path with
// $HOME expanded (so it's usable as an absolute path by agent CLIs).
//
// The filename is content-addressed (first 32 hex chars of the sha256, 128
// bits) so pasting the same screenshot twice reuses one file rather than
// littering the dir. Upload is atomic: write to .tmp, then mv.
//
// The remote path comes back over stdout, which is adversary-influenceable
// (rc files, PROMPT_COMMAND, command wrappers can all prepend output) and the
// caller types it straight into the foreground PTY. Upload therefore runs the
// remote shell non-interactively (--noprofile --norc) AND validates the
// returned path against the basename it already knows, so callers can trust
// the value with no risk of newline/control-byte injection.
func Upload(ctx context.Context, t sshctl.Transport, pngData []byte) (string, error) {
	if len(pngData) == 0 {
		return "", fmt.Errorf("clipupload: empty image")
	}
	sha := ShortSHA(pngData)
	abs, _, err := upload(ctx, t, pngData, "clip-"+sha+".png")
	return abs, err
}

// UploadText writes textData to RemoteDir/text-<sha>.txt the same atomic,
// 0600, validated way as Upload and returns (absolute path, short sha). It is
// the side channel for `clip text`: the ClipResponse carries only the sha and
// the agent reconstructs ~/.cache/portal/clip/text-<sha>.txt from it. Text
// bytes never cross the CBOR frame (DESIGN §4.1/§7), so the 1 MiB cap is safe.
func UploadText(ctx context.Context, t sshctl.Transport, textData []byte) (path, sha string, err error) {
	if len(textData) == 0 {
		return "", "", fmt.Errorf("clipupload: empty text")
	}
	sha = ShortSHA(textData)
	return upload(ctx, t, textData, "text-"+sha+".txt")
}

// UploadImage is Upload but also returns the short sha so the caller can put
// it in a ClipResponse without recomputing it. The remote path is identical
// to Upload's.
func UploadImage(ctx context.Context, t sshctl.Transport, pngData []byte) (path, sha string, err error) {
	if len(pngData) == 0 {
		return "", "", fmt.Errorf("clipupload: empty image")
	}
	sha = ShortSHA(pngData)
	path, _, err = upload(ctx, t, pngData, "clip-"+sha+".png")
	return path, sha, err
}

// ShortSHA returns the content address used in clip filenames: the first 32
// hex chars (128 bits) of the sha256. portald clip reconstructs the local
// filename from exactly this string (matched against ^[0-9a-f]{32}$), so the
// Mac and the agent MUST agree on this derivation.
func ShortSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:32]
}

// upload is the shared atomic write used by the image and text variants. name
// is the content-addressed basename (clip-<sha>.png / text-<sha>.txt). It
// enforces MaxUploadBytes before touching the wire, writes the file 0600
// EXPLICITLY via chmod (not umask — DESIGN §7.1), and validates the returned
// path against the basename it already knows so an injected stdout line can
// never reach the caller. Returns (absolute path, short sha).
func upload(ctx context.Context, t sshctl.Transport, data []byte, name string) (string, string, error) {
	if len(data) > MaxUploadBytes {
		return "", "", fmt.Errorf("clipupload: payload too large: %d bytes (max %d)", len(data), MaxUploadBytes)
	}
	// install -d for an atomic 0700 dir; write to a unique tmp, chmod 0600
	// EXPLICITLY (defense-in-depth — do not trust the remote umask), then mv.
	// Echo the $HOME-expanded absolute path back so the caller can verify it.
	// Emit "$HOME/.cache/portal/clip/<name>" directly — a plain expansion that
	// does NOT re-glob or re-split the way `eval echo` would.
	remotePath := RemoteDir + "/" + name
	script := fmt.Sprintf(
		`set -e; install -d -m 0700 %s && tmp=$(mktemp %s/.clip.tmp.XXXXXX) && `+
			`trap 'rm -f "$tmp"' EXIT && cat > "$tmp" && chmod 0600 "$tmp" && mv "$tmp" %s && trap - EXIT && `+
			`printf '%%s' "$HOME/.cache/portal/clip/%s"`,
		RemoteDir, RemoteDir, remotePath, name,
	)
	// --noprofile --norc keeps rc noise off stdout so the path is the only
	// thing we read back.
	stdout, stderr, err := t.ExecBytes(ctx, data, "bash", "--noprofile", "--norc", "-c", shellQuote(script))
	if err != nil {
		if s := strings.TrimSpace(stderr); s != "" {
			return "", "", fmt.Errorf("clipupload: %w: %s", err, s)
		}
		return "", "", fmt.Errorf("clipupload: %w", err)
	}
	abs, err := validateRemotePath(stdout, name)
	if err != nil {
		if s := strings.TrimSpace(stderr); s != "" {
			return "", "", fmt.Errorf("%w: %s", err, s)
		}
		return "", "", err
	}
	// shortSHA is the substring between the "clip-"/"text-" prefix and the
	// extension; recompute from data to avoid parsing the name back out.
	return abs, ShortSHA(data), nil
}

// validateRemotePath turns adversary-influenceable remote stdout into a path
// the caller can safely type into the foreground PTY. expectedName is the
// content-addressed basename Upload already computed; the path is accepted
// only if it is an absolute, single-line, control-byte-free path ending in
// "/" + expectedName. On any mismatch it returns an error and NO path, so an
// injected leading line or embedded newline can never reach the caller.
func validateRemotePath(out, expectedName string) (string, error) {
	abs := strings.TrimSpace(out)
	if abs == "" {
		return "", fmt.Errorf("clipupload: remote returned empty path")
	}
	if !strings.HasPrefix(abs, "/") {
		return "", fmt.Errorf("clipupload: remote path not absolute: %q", abs)
	}
	for i := 0; i < len(abs); i++ {
		if abs[i] < 0x20 {
			// Catches embedded newlines and any other control byte, which is
			// what an injected extra line of output would look like.
			return "", fmt.Errorf("clipupload: remote path has a control byte at %d: %q", i, abs)
		}
	}
	if !strings.HasSuffix(abs, "/"+expectedName) {
		return "", fmt.Errorf("clipupload: remote path %q does not end in /%s", abs, expectedName)
	}
	return abs, nil
}

// shellQuote wraps a script in single quotes for safe remote execution via
// ssh (which joins argv with spaces and runs the result through sh -c).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
