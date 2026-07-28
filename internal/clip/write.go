package clip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"
)

// Writer sets the local Mac clipboard on behalf of a remote write request.
// Every method honors the caller's context: the Mac-side write slot is at most
// 8s total (DESIGN-clipboard-write-interception §4.4), and setting the
// pasteboard is only part of it. Payload size limits are enforced by the
// caller; keeping the clipupload cap out of this leaf avoids a second limit
// that can drift.
type Writer interface {
	SetText(ctx context.Context, data []byte) error
	SetImagePNG(ctx context.Context, png []byte) error
	Clear(ctx context.Context) error
}

// pasteboardRunner runs one command to completion with stdin, honoring ctx.
// It is injected so command and script construction are testable off-darwin.
type pasteboardRunner func(ctx context.Context, name string, args []string, stdin []byte) error

type writer struct {
	run pasteboardRunner
}

const setWriteTimeout = 5 * time.Second

const setImagePNGScript = `on run argv
	set the clipboard to (read (POSIX file (item 1 of argv)) as «class PNGf»)
end run`

func (w writer) SetText(ctx context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, setWriteTimeout)
	defer cancel()

	if err := w.run(ctx, "/usr/bin/pbcopy", nil, data); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pbcopy timed out setting clipboard text after %s", setWriteTimeout)
		}
		return fmt.Errorf("pbcopy: %w", err)
	}
	return nil
}

func (w writer) SetImagePNG(ctx context.Context, png []byte) error {
	if !hasPNGMagic(png) {
		return fmt.Errorf("clipboard image is not PNG data")
	}

	tmp, err := os.CreateTemp("", "portal-clipw-*.png")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmp.Write(png); err != nil {
		return fmt.Errorf("write clipboard PNG temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close clipboard PNG temp file: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, setWriteTimeout)
	defer cancel()
	if err := w.run(ctx, "/usr/bin/osascript", []string{"-e", setImagePNGScript, tmpPath}, nil); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("osascript timed out setting clipboard image after %s", setWriteTimeout)
		}
		return fmt.Errorf("osascript: %w", err)
	}
	return nil
}

func (w writer) Clear(ctx context.Context) error {
	return w.SetText(ctx, nil)
}

func hasPNGMagic(data []byte) bool {
	return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
}
