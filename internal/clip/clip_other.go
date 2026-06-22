//go:build !darwin

package clip

import "errors"

// ErrUnsupported is returned by the stub clipboard on non-darwin platforms.
// The Mac client only runs on darwin; this keeps `go build ./...` green on
// Linux CI.
var ErrUnsupported = errors.New("clip: clipboard access only supported on macOS")

type Unsupported struct{}

func New() Clipboard { return Unsupported{} }

func (Unsupported) HasImage() bool            { return false }
func (Unsupported) ImagePNG() ([]byte, error) { return nil, ErrUnsupported }
