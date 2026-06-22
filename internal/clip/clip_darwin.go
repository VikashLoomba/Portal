//go:build darwin

package clip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Darwin extracts clipboard images via osascript. No cgo.
type Darwin struct{}

func New() Clipboard { return Darwin{} }

// Info returns the raw `clipboard info` flavor list — used by the
// `portal clip-check` diagnostic so we can see exactly what macOS reports.
func Info() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", "clipboard info").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// imageFlavors are the `clipboard info` substrings that indicate the
// clipboard holds raster image data we can coerce to PNG. macOS renders
// flavors in several forms depending on the source app, so we match
// generously.
var imageFlavors = []string{
	"PNGf",        // «class PNGf» — screenshots, most apps
	"TIFF",        // «class TIFF»/TIFF picture — Preview, generic
	"PICT",        // legacy «class PICT»
	"public.png",  // UTI form
	"public.tiff",
	"public.jpeg",
	"JPEG",        // JPEG picture
	"GIF",         // GIF picture
	"public.heic",
	"«class BMP",  // bitmaps
}

// HasImage reports whether the clipboard holds image data. It inspects the
// type list from `clipboard info` for a known image flavor — much cheaper
// than pulling the (potentially large) image bytes on every Ctrl+V.
func (Darwin) HasImage() bool {
	info, err := Info()
	if err != nil {
		return false
	}
	for _, flavor := range imageFlavors {
		if strings.Contains(info, flavor) {
			return true
		}
	}
	return false
}

// Describe reports the raw clipboard flavor list and whether HasImage
// matched, for the `portal clip-check` diagnostic.
func (d Darwin) Describe() string {
	info, err := Info()
	if err != nil {
		return fmt.Sprintf("clipboard info failed: %v", err)
	}
	matched := ""
	for _, flavor := range imageFlavors {
		if strings.Contains(info, flavor) {
			matched = flavor
			break
		}
	}
	verdict := "no image flavor detected"
	if matched != "" {
		verdict = "image detected (matched flavor: " + matched + ")"
	}
	return verdict + "\nraw flavors: " + info
}

// ImagePNG pulls the clipboard image as PNG bytes. It writes to a temp file
// via osascript (AppleScript can't return raw binary on stdout cleanly),
// reads it back, and removes it. The «class PNGf» coercion converts most
// image flavors (TIFF, JPEG, etc.) to PNG.
func (Darwin) ImagePNG() ([]byte, error) {
	tmp, err := os.CreateTemp("", "portal-clip-*.png")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

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
		return nil, fmt.Errorf("clipboard coercion failed: %s", bytes.TrimSpace(out))
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
