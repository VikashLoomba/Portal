package audit

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLog_AppendsLines(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	// Pin the clock so the timestamp is deterministic.
	fixed := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return fixed }

	l.ClipServed("box", "image", "sha=deadbeef")
	l.ClipDenied("box", "text", "concealed")
	l.Notify("box", "Claude finished", true, 0)
	l.NotifyDenied("box", "disabled")
	l.OpenURL("box", "https://example.com/path")
	l.ExecOpen("box", "a1b2c3d4", "printf hello", 501, false)
	l.ExecClose("box", "a1b2c3d4", 4, "remote\nfailure", 2*time.Second)

	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	wantSubstrings := []string{
		"2026-06-24T12:00:00Z\tclip-served\thost=box\tkind=image\tsha=deadbeef",
		"clip-denied\thost=box\tkind=text\treason=concealed",
		"notify\thost=box\tverified=true\turgency=0\ttitle=Claude finished",
		"notify-denied\thost=box\treason=disabled",
		"open-url\thost=box\turl=https://example.com/path",
		"exec-open\thost=box\tsid=a1b2c3d4\tuid=501\targv=printf hello",
		"exec-close\thost=box\tsid=a1b2c3d4\tcode=4\terr=remote failure\tdur=2s",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("audit log missing line %q\nfull log:\n%s", w, got)
		}
	}
	if n := strings.Count(got, "\n"); n != 7 {
		t.Errorf("expected 7 lines, got %d:\n%s", n, got)
	}
}

func TestLog_ExecSessionFields(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)

	l.ExecOpen("box", "0123abcd", "sh", 501, true)
	l.ExecOpen("box", "4567ef00", "printf hi", 502, false)
	l.ExecClose("box", "0123abcd", 7, "boom", time.Second)

	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	wantSubstrings := []string{
		"exec-open\thost=box\tsid=0123abcd\tuid=501\targv=sh\tpty=1",
		"exec-open\thost=box\tsid=4567ef00\tuid=502\targv=printf hi",
		"exec-close\thost=box\tsid=0123abcd\tcode=7\terr=boom\tdur=1s",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("audit log missing line %q\nfull log:\n%s", w, got)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "sid=4567ef00") && strings.Contains(line, "pty=1") {
			t.Fatalf("pty=false exec-open carried pty=1: %s", line)
		}
		if strings.Contains(line, "exec-open") && !strings.Contains(line, "\thost=box\tsid=") {
			t.Fatalf("sid is not immediately after host: %s", line)
		}
	}
}

func TestLog_StripsControlBytes(t *testing.T) {
	// A crafted URL/title with embedded newlines/tabs must not forge extra
	// audit lines or columns.
	dir := t.TempDir()
	l := New(dir)
	l.OpenURL("box", "https://evil/\n2026-01-01T00:00:00Z\tclip-served\tforged")

	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("control bytes leaked extra lines: %d newlines:\n%s", n, got)
	}
	if strings.Contains(got, "clip-served\tforged") {
		t.Errorf("forged columns leaked into log: %s", got)
	}
}

func TestLog_NilSafe(t *testing.T) {
	var l *Log
	// Must not panic.
	l.ClipServed("h", "image", "x")
	l.Notify("h", "t", false, 1)
	l.OpenURL("h", "u")
	l.ExecOpen("h", "sid", "argv", 1, false)
	l.ExecClose("h", "sid", 0, "", time.Millisecond)
	if l.Path() != "" {
		t.Errorf("nil Log.Path() should be empty")
	}
}
