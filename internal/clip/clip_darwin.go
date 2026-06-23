//go:build darwin

package clip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxImageBytes caps the temp-file image we'll read into memory, so a
// pathological clipboard can't make us allocate an unbounded blob.
const maxImageBytes = 64 << 20 // 64 MiB

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

// imageFlavors are the `clipboard info` flavor names that indicate the
// clipboard holds raster image data we can coerce to PNG. `clipboard info`
// always reports class-form names («class XXXX»), never the public.* UTI
// form, so we anchor each class code to the «class XXXX» form and match it
// exactly against a split flavor list rather than loose substring search —
// short codes like GIF/JPEG/TIFF are otherwise a false-positive hazard.
var imageFlavors = []string{
	"«class PNGf»", // screenshots, most apps
	"«class TIFF»", // Preview, generic
	"«class PICT»", // legacy
	"«class JPEG»", // JPEG picture
	"«class GIFf»", // GIF picture
	"«class GIF »", // GIF picture (alternate padding)
	"«class BMP »", // bitmaps
	"«class HEIC»", // modern HEIF stills
	"«class AVIF»", // AV1 stills
	"«class jp2 »", // JPEG 2000
	"«class 8BPS»", // Photoshop
}

// matchImageFlavor reports whether the `clipboard info` flavor list contains
// a known raster image flavor, returning the matched flavor name. It splits
// the comma-joined list on ", " and compares each entry exactly so that a
// short code (GIF/JPEG/TIFF) appearing as a substring of an unrelated flavor
// name does not falsely match. It is pure for testability.
func matchImageFlavor(info string) (flavor string, ok bool) {
	if strings.TrimSpace(info) == "" {
		return "", false
	}
	for _, entry := range strings.Split(info, ", ") {
		entry = strings.TrimSpace(entry)
		for _, f := range imageFlavors {
			if entry == f {
				return f, true
			}
		}
	}
	return "", false
}

// HasImage reports whether the clipboard holds image data. It inspects the
// type list from `clipboard info` for a known image flavor — much cheaper
// than pulling the (potentially large) image bytes on every Ctrl+V.
func (Darwin) HasImage() bool {
	info, err := Info()
	if err != nil {
		return false
	}
	_, ok := matchImageFlavor(info)
	return ok
}

// Describe reports the raw clipboard flavor list and whether HasImage
// matched, for the `portal clip-check` diagnostic.
func (d Darwin) Describe() string {
	info, err := Info()
	if err != nil {
		return fmt.Sprintf("clipboard info failed: %v", err)
	}
	verdict := "no image flavor detected"
	if matched, ok := matchImageFlavor(info); ok {
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

	// Pass tmpPath as an argv item rather than interpolating it into the
	// script: Go's strconv.Quote and AppleScript's string-literal escaping
	// diverge for control/odd bytes, so an exotic $TMPDIR could break the
	// POSIX file coercion. `on run argv` sidesteps escaping entirely.
	script := `on run argv
	set f to (open for access (POSIX file (item 1 of argv)) with write permission)
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
	end try
end run`

	// Large/multi-monitor screenshots can take well over 5s to coerce; align
	// the ceiling with the 30s upload budget so we don't spuriously time out.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script, tmpPath).Output()
	if err != nil {
		// Distinguish a deadline/timeout from a coercion error so clip-check
		// can report "osascript timed out" rather than a generic failure.
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("osascript timed out coercing clipboard image after 30s")
		}
		return nil, fmt.Errorf("osascript: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(out), []byte("OK")) {
		return nil, fmt.Errorf("clipboard coercion failed: %s", bytes.TrimSpace(out))
	}

	// Guard against reading an unbounded blob into RAM. macOS will happily
	// hand us a multi-hundred-MB coercion for pathological clipboards.
	if fi, err := os.Stat(tmpPath); err != nil {
		return nil, err
	} else if fi.Size() > maxImageBytes {
		return nil, fmt.Errorf("clipboard image too large: %d bytes (max %d)", fi.Size(), maxImageBytes)
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
