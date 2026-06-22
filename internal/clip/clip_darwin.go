//go:build darwin

package clip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Darwin extracts clipboard images via osascript. No cgo.
type Darwin struct{}

func New() Clipboard { return Darwin{} }

// hasImageScript checks for any of the common image flavors on the
// pasteboard. `clipboard info` lists every available type; we look for the
// PNG, TIFF, or generic picture class. Most apps (Preview, browsers,
// Screenshot.app) put «class PNGf» and/or TIFF on the board for an image.
const hasImageScript = `clipboard info`

// HasImage reports whether the clipboard holds image data. It inspects the
// type list from `clipboard info` for an image flavor — much cheaper than
// actually pulling the (potentially large) image bytes on every Ctrl+V.
func (Darwin) HasImage() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", hasImageScript).Output()
	if err != nil {
		return false
	}
	info := string(out)
	// `clipboard info` renders flavors like: «class PNGf», 1234, «class TIFF», ...
	for _, flavor := range []string{"PNGf", "TIFF", "«class PICT»", "public.png", "public.tiff"} {
		if bytes.Contains([]byte(info), []byte(flavor)) {
			return true
		}
	}
	return false
}

// ImagePNG pulls the clipboard image as PNG bytes. It writes to a temp file
// via osascript (AppleScript can't return raw binary on stdout cleanly),
// reads it back, and removes it. If the clipboard has no PNG flavor but has
// TIFF, it asks for PNG anyway — the AppleScript coercion «class PNGf»
// converts most image flavors to PNG.
func (Darwin) ImagePNG() ([]byte, error) {
	tmp, err := os.CreateTemp("", "portal-clip-*.png")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// AppleScript: open the temp file, truncate, write the clipboard coerced
	// to PNG, close. On failure (no image) it returns an error string.
	script := fmt.Sprintf(`set f to (open for access (POSIX file %s) with write permission)
try
	set eof f to 0
	write (the clipboard as «class PNGf») to f
	close access f
	return "OK"
on error errMsg
	try
		close access f
	end try
	return "ERR:" & errMsg
end try`, strconv.Quote(tmpPath))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return nil, fmt.Errorf("osascript: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(out), []byte("OK")) {
		return nil, fmt.Errorf("no image in clipboard: %s", bytes.TrimSpace(out))
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("clipboard image was empty")
	}
	return data, nil
}

// ensure filepath import is used (kept for future flavor-specific temp dirs).
var _ = filepath.Join
