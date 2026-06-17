// Package logfile is a minimal log reader for the `logs` command. Tail
// returns the last n lines; Follow streams new lines until ctx cancels,
// reopening on truncate (since launchd may rotate the file).
package logfile

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// Tail returns the last n lines of path. n <= 0 defaults to 40 (matches the
// bash default for `portal logs`).
func Tail(path string, n int) (string, error) {
	if n <= 0 {
		n = 40
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Naive but adequate for human-sized log files: read all, keep last n.
	all, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(all), "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n") + "\n", nil
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n", nil
}

// Follow streams new lines until ctx cancels, reopening on truncate or
// missing file (with backoff) — equivalent to `tail -f`.
func Follow(ctx context.Context, path string, w io.Writer) error {
	for {
		err := followOnce(ctx, path, w)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if err != nil {
			// File missing or rotated — wait briefly and retry.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
	}
}

func followOnce(ctx context.Context, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			if _, werr := w.Write([]byte(line)); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			// Detect truncation: if the file shrank below our offset, reopen.
			off, _ := f.Seek(0, io.SeekCurrent)
			st, statErr := os.Stat(path)
			if statErr == nil && st.Size() < off {
				return nil // signal followOnce caller to reopen
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}
