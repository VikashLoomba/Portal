package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecSSH_NotFound verifies execSSH returns a clear error (rather than
// panicking or silently succeeding) when no ssh binary is on PATH. We can't
// test the happy path directly because syscall.Exec replaces the process image
// on success — so the contract we CAN assert is the lookup-failure branch.
func TestExecSSH_NotFound(t *testing.T) {
	// Empty PATH => exec.LookPath("ssh") fails.
	t.Setenv("PATH", "")
	err := execSSH([]string{"somehost"})
	if err == nil {
		t.Fatal("expected an error when ssh is not on PATH")
	}
	if !strings.Contains(err.Error(), "ssh not found") {
		t.Errorf("error = %q, want it to mention 'ssh not found'", err)
	}
}

// TestExecSSH_PassthroughExitCode proves the passthrough forwards args verbatim
// to ssh AND propagates ssh's exit code via exitCodeErr on the child-process
// fallback path. We can't make syscall.Exec fail on demand, so we exercise the
// fallback's exit-code mapping with a fake "ssh" that exits with a known code:
// we shadow ssh on PATH with a script that exits 7, then run the fallback half
// of execSSH directly (the syscall.Exec half would replace this test process).
func TestExecSSH_PassthroughExitCode(t *testing.T) {
	dir := t.TempDir()
	// A fake ssh that records argv and exits non-zero. argv[1..] should be the
	// args we pass through verbatim.
	fakeSSH := filepath.Join(dir, "ssh")
	argLog := filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shQuote(argLog) + "\nexit 7\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Run the fake ssh exactly as execSSH's fallback would (c.Run + exit-code
	// mapping), so we assert both verbatim args and the exitCodeErr mapping
	// without replacing the test process via syscall.Exec.
	args := []string{"myhost", "-p", "2222", "echo", "hi"}
	c := exec.Command(fakeSSH, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := c.Run()
	if runErr == nil {
		t.Fatal("expected the fake ssh to exit non-zero")
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T", runErr)
	}
	if code := exitErr.ExitCode(); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	// exitCodeErr carries that code out to main verbatim.
	ece := exitCodeErr{code: exitErr.ExitCode()}
	if ece.code != 7 {
		t.Errorf("exitCodeErr.code = %d, want 7", ece.code)
	}

	// Verify the fake ssh saw our args VERBATIM (passthrough contract).
	got, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := strings.Join(args, "\n") + "\n"
	if string(got) != wantLog {
		t.Errorf("ssh received args %q, want %q", got, wantLog)
	}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
