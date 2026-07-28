package clip

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pasteboardRecorder struct {
	name     string
	args     []string
	stdin    []byte
	statMode fs.FileMode
	statData []byte
	err      error
	calls    int
}

func (r *pasteboardRecorder) run(_ context.Context, name string, args []string, stdin []byte) error {
	r.calls++
	r.name = name
	r.args = append([]string(nil), args...)
	r.stdin = append([]byte(nil), stdin...)
	if len(args) > 0 {
		path := args[len(args)-1]
		if info, err := os.Stat(path); err == nil {
			r.statMode = info.Mode()
		}
		if data, err := os.ReadFile(path); err == nil {
			r.statData = data
		}
	}
	return r.err
}

func TestSetText_PbcopyArgvAndStdin(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "no trailing newline", data: []byte("hello")},
		{name: "embedded NUL", data: []byte("left\x00right")},
		{name: "UTF-8", data: []byte("héllo 世界")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &pasteboardRecorder{}
			w := writer{run: rec.run}
			if err := w.SetText(context.Background(), tt.data); err != nil {
				t.Fatal(err)
			}
			if rec.name != "/usr/bin/pbcopy" {
				t.Errorf("command = %q, want /usr/bin/pbcopy", rec.name)
			}
			if len(rec.args) != 0 {
				t.Errorf("args = %#v, want none", rec.args)
			}
			if !bytes.Equal(rec.stdin, tt.data) {
				t.Errorf("stdin = %q, want %q", rec.stdin, tt.data)
			}
		})
	}

	runErr := errors.New("runner failed")
	rec := &pasteboardRecorder{err: runErr}
	if err := (writer{run: rec.run}).SetText(context.Background(), []byte("x")); !errors.Is(err, runErr) {
		t.Fatalf("SetText error = %v, want wrapped runner error", err)
	}
}

func TestClear_EmptyPbcopyStdin(t *testing.T) {
	rec := &pasteboardRecorder{}
	if err := (writer{run: rec.run}).Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.name != "/usr/bin/pbcopy" {
		t.Errorf("command = %q, want /usr/bin/pbcopy", rec.name)
	}
	if len(rec.args) != 0 {
		t.Errorf("args = %#v, want none", rec.args)
	}
	if len(rec.stdin) != 0 {
		t.Errorf("stdin = %q, want empty", rec.stdin)
	}
}

func TestSetImagePNG_ScriptAndTempFile(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("test payload")...)
	for _, tt := range []struct {
		name   string
		runErr error
	}{
		{name: "success"},
		{name: "runner error", runErr: errors.New("osascript failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			rec := &pasteboardRecorder{err: tt.runErr}
			err := (writer{run: rec.run}).SetImagePNG(context.Background(), png)
			if tt.runErr == nil && err != nil {
				t.Fatal(err)
			}
			if tt.runErr != nil && !errors.Is(err, tt.runErr) {
				t.Fatalf("SetImagePNG error = %v, want wrapped runner error", err)
			}
			if rec.name != "/usr/bin/osascript" {
				t.Errorf("command = %q, want /usr/bin/osascript", rec.name)
			}
			if len(rec.args) != 3 {
				t.Fatalf("args = %#v, want -e, script, temp path", rec.args)
			}
			if rec.args[0] != "-e" {
				t.Errorf("args[0] = %q, want -e", rec.args[0])
			}
			if rec.args[1] != setImagePNGScript ||
				!strings.HasPrefix(rec.args[1], "on run argv") ||
				!strings.Contains(rec.args[1], "set the clipboard to") ||
				!strings.Contains(rec.args[1], "«class PNGf»") {
				t.Errorf("unexpected osascript:\n%s", rec.args[1])
			}
			if filepath.Ext(rec.args[2]) != ".png" {
				t.Errorf("temp path = %q, want .png suffix", rec.args[2])
			}
			if got := rec.statMode.Perm(); got != 0o600 {
				t.Errorf("temp mode = %04o, want 0600", got)
			}
			if !bytes.Equal(rec.statData, png) {
				t.Errorf("temp contents differ")
			}
			if len(rec.stdin) != 0 {
				t.Errorf("osascript stdin = %q, want empty", rec.stdin)
			}
			if _, err := os.Stat(rec.args[2]); !os.IsNotExist(err) {
				t.Errorf("temp file still exists after SetImagePNG: %v", err)
			}
		})
	}
}

func TestSetImagePNG_RejectsNonPNG(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "JPEG", data: []byte("\xff\xd8\xff\xe0")},
		{name: "empty", data: []byte{}},
		{name: "truncated", data: []byte("\x89PN")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("TMPDIR", tmpDir)
			rec := &pasteboardRecorder{}
			if err := (writer{run: rec.run}).SetImagePNG(context.Background(), tt.data); err == nil {
				t.Fatal("SetImagePNG accepted non-PNG data")
			}
			if rec.calls != 0 {
				t.Errorf("runner called %d times, want 0", rec.calls)
			}
			entries, err := os.ReadDir(tmpDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("rejected PNG left temp files: %v", entries)
			}
		})
	}
}

func TestHasPNGMagic(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "exact signature", data: []byte("\x89PNG\r\n\x1a\n"), want: true},
		{name: "signature and payload", data: []byte("\x89PNG\r\n\x1a\npayload"), want: true},
		{name: "truncated signature", data: []byte("\x89PNG\r\n\x1a"), want: false},
		{name: "JPEG", data: []byte("\xff\xd8\xff\xe0"), want: false},
		{name: "empty", data: []byte{}, want: false},
		{name: "nil", data: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasPNGMagic(tt.data); got != tt.want {
				t.Errorf("hasPNGMagic(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestWriterHonorsContext(t *testing.T) {
	t.Run("canceled context reaches runner", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var runnerErr error
		w := writer{run: func(got context.Context, _ string, _ []string, _ []byte) error {
			runnerErr = got.Err()
			return runnerErr
		}}
		err := w.SetText(ctx, []byte("x"))
		if !errors.Is(runnerErr, context.Canceled) {
			t.Fatalf("runner context error = %v, want canceled", runnerErr)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SetText error = %v, want wrapped canceled error", err)
		}
	})

	t.Run("writer installs five second cap", func(t *testing.T) {
		before := time.Now()
		w := writer{run: func(got context.Context, _ string, _ []string, _ []byte) error {
			deadline, ok := got.Deadline()
			if !ok {
				t.Fatal("runner context has no deadline")
			}
			if deadline.Before(before.Add(4*time.Second)) || deadline.After(before.Add(setWriteTimeout+time.Second)) {
				t.Errorf("runner deadline = %v, want approximately %v from now", deadline, setWriteTimeout)
			}
			return nil
		}}
		if err := w.SetText(context.Background(), []byte("x")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("shorter caller deadline wins", func(t *testing.T) {
		callerDeadline := time.Now().Add(time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
		defer cancel()
		w := writer{run: func(got context.Context, _ string, _ []string, _ []byte) error {
			deadline, ok := got.Deadline()
			if !ok {
				t.Fatal("runner context has no deadline")
			}
			if !deadline.Equal(callerDeadline) {
				t.Errorf("runner deadline = %v, want caller deadline %v", deadline, callerDeadline)
			}
			return nil
		}}
		if err := w.SetText(ctx, []byte("x")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("image writer preserves caller deadline", func(t *testing.T) {
		callerDeadline := time.Now().Add(time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
		defer cancel()
		png := []byte("\x89PNG\r\n\x1a\npayload")
		w := writer{run: func(got context.Context, _ string, _ []string, _ []byte) error {
			deadline, ok := got.Deadline()
			if !ok {
				t.Fatal("runner context has no deadline")
			}
			if !deadline.Equal(callerDeadline) {
				t.Errorf("runner deadline = %v, want caller deadline %v", deadline, callerDeadline)
			}
			return nil
		}}
		if err := w.SetImagePNG(ctx, png); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("expired deadline is reported as timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		w := writer{run: func(got context.Context, _ string, _ []string, _ []byte) error {
			return got.Err()
		}}
		if err := w.SetText(ctx, []byte("x")); err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("SetText error = %v, want timeout-specific error", err)
		}
	})
}
