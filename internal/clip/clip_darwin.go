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

// maxTextBytes caps the clipboard text we'll read into memory. Text is the
// password-bearing surface and is opt-in (DESIGN §7.1); the cap is a sanity
// bound against a pathologically large paste, not a security control.
const maxTextBytes = 16 << 20 // 16 MiB

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

// textFlavors are the `clipboard info` flavor names that indicate the
// clipboard holds text we can coerce with `the clipboard as text`. As with
// imageFlavors we match the class-form names exactly against a split list.
var textFlavors = []string{
	"«class utf8»", // plain UTF-8 text
	"«class ut16»", // UTF-16 text
	"«class TEXT»", // legacy plain text
	"string",       // AppleScript's coerced name for a text flavor
}

// matchTextFlavor reports whether the `clipboard info` flavor list contains a
// known text flavor. Same exact-split matching discipline as matchImageFlavor.
func matchTextFlavor(info string) (flavor string, ok bool) {
	if strings.TrimSpace(info) == "" {
		return "", false
	}
	for _, entry := range strings.Split(info, ", ") {
		entry = strings.TrimSpace(entry)
		for _, f := range textFlavors {
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

// concealedTypes are the NSPasteboard type identifiers that signal the
// clipboard owner does NOT want the contents persisted or exfiltrated.
// Password managers (1Password, Bitwarden, KeePassXC, etc.) set
// org.nspasteboard.ConcealedType when they copy a credential; some tools set
// org.nspasteboard.TransientType for ephemeral copies. macOS surfaces these as
// reverse-DNS UTIs on NSPasteboard.types — NOT in the AppleScript `clipboard
// info` flavor list — so detecting them needs a richer pasteboard-types probe.
var concealedTypes = []string{
	"org.nspasteboard.ConcealedType",
	"org.nspasteboard.TransientType",
}

// concealedProbeScript reads the full NSPasteboard type list via the
// AppleScript-ObjC bridge and prints one type per line. This is the only way to
// see the reverse-DNS UTIs (`clipboard info` only reports «class XXXX» four-char
// codes, never org.nspasteboard.*). It is cgo-free (osascript hosts the ObjC
// bridge) and runs in a fraction of `clipboard info`'s time. On any bridge
// failure it prints nothing → caller treats the clipboard as not concealed.
const concealedProbeScript = `use framework "AppKit"
use scripting additions
set pb to current application's NSPasteboard's generalPasteboard()
set theTypes to pb's types() as list
set out to ""
repeat with t in theTypes
	set out to out & (t as text) & linefeed
end repeat
return out`

// IsConcealed reports whether the clipboard is marked secret/transient. It
// probes the NSPasteboard type list (via the ObjC bridge) for a known concealed
// UTI. A probe failure returns true (fail-CLOSED): the text serve is a standing
// pull endpoint, so a flaky/unavailable/timed-out probe must err on the side of
// NOT auto-exfiltrating a password-manager credential rather than serving it.
// The design's "minimum to not auto-exfiltrate secrets" intent (SPEC C / DESIGN
// §7.1) is only honored if an unknown clipboard state is treated as concealed.
func (Darwin) IsConcealed() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", concealedProbeScript).Output()
	if err != nil {
		// Bridge/probe failure: fail closed — treat the clipboard as concealed
		// so we never serve text we could not confirm is safe.
		return true
	}
	return matchConcealedType(string(out))
}

// matchConcealedType reports whether the newline-separated NSPasteboard type
// list contains a known concealed/transient UTI. Pure for testability; matches
// each line exactly (trimmed) so a substring of an unrelated type name does not
// falsely trip the skip.
func matchConcealedType(types string) bool {
	for _, line := range strings.Split(types, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, c := range concealedTypes {
			if line == c {
				return true
			}
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
func (Darwin) ImagePNG(ctx context.Context) ([]byte, error) {
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

	// Cap the osascript coercion at 5s on top of whatever deadline the caller
	// already carries. The paste path (cmd/portal/run.go) passes an 8s context
	// and the budget (DESIGN §4.5) reserves ~5s for coercion + ~3s for upload;
	// a longer image coercion must fail fast so the agent's clipTimeout (9s)
	// and socket deadline (11s) — both under the 12s HeartbeatTimeout — hold.
	// Honouring the parent ctx (rather than context.Background as before) is
	// what makes that budget real: a caller with a tighter deadline wins.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script, tmpPath).Output()
	if err != nil {
		// Distinguish a deadline/timeout from a coercion error so clip-check
		// can report "osascript timed out" rather than a generic failure.
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("osascript timed out coercing clipboard image after 5s")
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

// HasText reports whether the clipboard holds text data. Like HasImage it
// inspects the cheap `clipboard info` flavor list rather than pulling bytes.
func (Darwin) HasText() bool {
	info, err := Info()
	if err != nil {
		return false
	}
	_, ok := matchTextFlavor(info)
	return ok
}

// Text pulls the clipboard contents as UTF-8 text bytes. Like ImagePNG it
// writes to a temp file via osascript rather than reading stdout, so embedded
// NULs / control bytes survive round-tripping cleanly (AppleScript cannot
// return arbitrary binary on stdout). The `as «class utf8»` coercion is the
// pbpaste equivalent — it forces UTF-8 regardless of the source flavor.
func (Darwin) Text(ctx context.Context) ([]byte, error) {
	tmp, err := os.CreateTemp("", "portal-clip-*.txt")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// Same `on run argv` discipline as ImagePNG: pass tmpPath as an argv item
	// to sidestep AppleScript string-literal escaping for exotic $TMPDIR paths.
	script := `on run argv
	set f to (open for access (POSIX file (item 1 of argv)) with write permission)
	try
		set eof f to 0
		write (the clipboard as «class utf8») to f
		close access f
		return "OK"
	on error errMsg
		try
			close access f
		end try
		return "ERR:" & errMsg
	end try
end run`

	// Cap at 5s on top of the caller's deadline, same as ImagePNG — honour the
	// parent ctx so the paste-path budget (DESIGN §4.5) bounds the coercion
	// rather than a hardcoded standalone timeout.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script, tmpPath).Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("osascript timed out coercing clipboard text after 5s")
		}
		return nil, fmt.Errorf("osascript: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(out), []byte("OK")) {
		return nil, fmt.Errorf("clipboard text coercion failed: %s", bytes.TrimSpace(out))
	}

	if fi, err := os.Stat(tmpPath); err != nil {
		return nil, err
	} else if fi.Size() > maxTextBytes {
		return nil, fmt.Errorf("clipboard text too large: %d bytes (max %d)", fi.Size(), maxTextBytes)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("clipboard text was empty")
	}
	return data, nil
}
