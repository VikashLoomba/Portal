// Package clip reads image data from the local (Mac) clipboard. It is used
// by `portal ssh` to intercept Ctrl+V: when the clipboard holds an image,
// the proxy uploads it to the remote and injects the resulting path instead
// of passing the keystroke through.
//
// The darwin implementation shells out to osascript so there is no cgo
// dependency — important because the release binaries are cross-compiled
// for darwin on Linux CI runners (CGO_ENABLED=0).
package clip

// Clipboard reads image data from the local clipboard.
type Clipboard interface {
	// HasImage reports whether the clipboard currently holds an image.
	// Must be cheap — it's polled on the Ctrl+V hot path.
	HasImage() bool
	// ImagePNG returns the clipboard image encoded as PNG bytes, or an
	// error if there is no image or extraction failed.
	ImagePNG() ([]byte, error)
	// Describe returns a human-readable summary of the current clipboard
	// flavors, for the `portal clip-check` diagnostic.
	Describe() string
}
