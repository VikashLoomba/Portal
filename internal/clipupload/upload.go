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

// Upload writes pngData to RemoteDir/clip-<hash>.png on the remote via the
// transport's multiplexed ControlMaster and returns the remote path with
// $HOME expanded (so it's usable as an absolute path by agent CLIs).
//
// The filename is content-addressed (first 12 hex chars of the sha256) so
// pasting the same screenshot twice reuses one file rather than littering
// the dir. Upload is atomic: write to .tmp, then mv.
func Upload(ctx context.Context, t sshctl.Transport, pngData []byte) (string, error) {
	if len(pngData) == 0 {
		return "", fmt.Errorf("clipupload: empty image")
	}
	sum := sha256.Sum256(pngData)
	name := "clip-" + hex.EncodeToString(sum[:])[:12] + ".png"
	remotePath := RemoteDir + "/" + name

	// install -d for an atomic 0700 dir; write to a unique tmp then mv.
	// Echo the $HOME-expanded absolute path back so the caller can inject it.
	script := fmt.Sprintf(
		`set -e; install -d -m 0700 %s && tmp=$(mktemp %s/.clip.tmp.XXXXXX) && `+
			`trap 'rm -f "$tmp"' EXIT && cat > "$tmp" && mv "$tmp" %s && trap - EXIT && `+
			`printf '%%s' "$(eval echo %s)"`,
		RemoteDir, RemoteDir, remotePath, remotePath,
	)
	stdout, _, err := t.ExecBytes(ctx, pngData, "bash", "-c", shellQuote(script))
	if err != nil {
		return "", fmt.Errorf("clipupload: %w", err)
	}
	abs := strings.TrimSpace(stdout)
	if abs == "" {
		return "", fmt.Errorf("clipupload: remote returned empty path")
	}
	return abs, nil
}

// shellQuote wraps a script in single quotes for safe remote execution via
// ssh (which joins argv with spaces and runs the result through sh -c).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
