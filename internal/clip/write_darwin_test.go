//go:build darwin

package clip

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunPasteboardCmd(t *testing.T) {
	t.Run("stdin plumbing", func(t *testing.T) {
		if err := runPasteboardCmd(context.Background(), "/bin/cat", nil, []byte("hi")); err != nil {
			t.Fatal(err)
		}
		if err := runPasteboardCmd(context.Background(), "/bin/sh", []string{"-c", `[ "$(/bin/cat)" = hi ]`}, []byte("hi")); err != nil {
			t.Fatalf("command did not receive stdin: %v", err)
		}
	})

	t.Run("stderr surfaced", func(t *testing.T) {
		err := runPasteboardCmd(context.Background(), "/bin/sh", []string{"-c", "echo pasteboard-failed >&2; exit 7"}, nil)
		if err == nil || !strings.Contains(err.Error(), "pasteboard-failed") {
			t.Fatalf("error = %v, want command stderr", err)
		}
	})

	t.Run("canceled context fails fast", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if err := runPasteboardCmd(ctx, "/bin/sleep", []string{"1"}, nil); err == nil {
			t.Fatal("runPasteboardCmd succeeded with canceled context")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("canceled command took %v", elapsed)
		}
	})
}

func TestNewWriter(t *testing.T) {
	got, ok := NewWriter().(writer)
	if !ok {
		t.Fatalf("NewWriter type = %T, want writer", NewWriter())
	}
	if got.run == nil {
		t.Fatal("NewWriter returned a writer with no command runner")
	}
	if reflect.ValueOf(got.run).Pointer() != reflect.ValueOf(runPasteboardCmd).Pointer() {
		t.Fatal("NewWriter did not wire the production pasteboard command runner")
	}
}
