// Package clip reads image data from the local (Mac) clipboard. It is used
// by `portal ssh` to intercept Ctrl+V: when the clipboard holds an image,
// the proxy uploads it to the remote and injects the resulting path instead
// of passing the keystroke through.
//
// The darwin implementation shells out to osascript so there is no cgo
// dependency — important because the release binaries are cross-compiled
// for darwin on Linux CI runners (CGO_ENABLED=0).
package clip

import "context"

// Clipboard reads image (and, opt-in, text) data from the local clipboard.
type Clipboard interface {
	// HasImage reports whether the clipboard currently holds an image.
	// Must be cheap — it's polled on the Ctrl+V hot path.
	HasImage() bool
	// ImagePNG returns the clipboard image encoded as PNG bytes, or an
	// error if there is no image or extraction failed. It takes a context so
	// the caller's paste-path deadline (DESIGN §4.5: Mac coerce ≤8s) bounds
	// the osascript coercion — the implementation must honour ctx and not
	// install a longer timeout of its own.
	ImagePNG(ctx context.Context) ([]byte, error)
	// HasText reports whether the clipboard currently holds text. Like
	// HasImage it must be cheap — it's on the `clip targets` probe path.
	HasText() bool
	// IsConcealed reports whether the clipboard is marked secret/transient —
	// macOS apps (password managers, secure note tools) set the
	// org.nspasteboard.ConcealedType / org.nspasteboard.TransientType hints to
	// signal "do not persist / do not exfiltrate this". The text serve is a
	// standing pull endpoint, so the daemon SKIPS serving text when this is
	// true (SPEC C concealed-clipboard skip) to avoid auto-exfiltrating a
	// password the user copied. It must be cheap — it's on the targets/text
	// probe path. A probe failure returns false (fail-open is acceptable here:
	// the capability gate is the primary control; this is defense-in-depth).
	IsConcealed() bool
	// Text returns the clipboard contents as UTF-8 text bytes, or an error
	// if there is no text or extraction failed. Text is the password-bearing
	// surface, so the daemon gates it behind an opt-in flag (DESIGN §7.1)
	// before ever calling this. Like ImagePNG it honours the caller's ctx
	// deadline so the paste-path budget is respected.
	Text(ctx context.Context) ([]byte, error)
	// Describe returns a human-readable summary of the current clipboard
	// flavors, for the `portal clip-check` diagnostic.
	Describe() string
}
