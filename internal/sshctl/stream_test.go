package sshctl_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/VikashLoomba/Portal/internal/run"
	"github.com/VikashLoomba/Portal/internal/sshctl"
	"github.com/VikashLoomba/Portal/internal/transport"
)

func TestStreamExitCodeMappingWithPathShim(t *testing.T) {
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "ssh")
	shim := `#!/bin/sh
while [ "$#" -gt 0 ]; do
	case "$1" in
		-S|-o|-p|-l|-i|-F|-J)
			shift 2
			;;
		-*)
			shift
			;;
		*)
			shift
			break
			;;
	esac
done
if [ "$#" -eq 0 ]; then
	exit 0
fi
cmd=$*
exec sh -c "$cmd"
`
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write ssh shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name       string
		command    string
		wantStdout string
		wantCode   int
		wantExit   bool
	}{
		{name: "success", command: "printf ok", wantStdout: "ok"},
		{name: "nonzero", command: "exit 3", wantCode: 3, wantExit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sshctl.New(filepath.Join(t.TempDir(), "cm.sock"), "shimhost", nil, run.OSRunner{})
			stdin, stdout, stderr, wait, err := s.Stream(context.Background(), tt.command)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if err := stdin.Close(); err != nil {
				t.Fatalf("close stdin: %v", err)
			}
			out, err := io.ReadAll(stdout)
			if err != nil {
				t.Fatalf("drain stdout: %v", err)
			}
			if string(out) != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", string(out), tt.wantStdout)
			}
			errOut, err := io.ReadAll(stderr)
			if err != nil {
				t.Fatalf("drain stderr: %v", err)
			}
			if len(errOut) != 0 {
				t.Fatalf("stderr = %q, want empty", string(errOut))
			}

			werr := wait()
			code, ok := transport.ExitCode(werr)
			if tt.wantExit {
				if !ok || code != tt.wantCode {
					t.Fatalf("ExitCode(wait err) = (%d, %v), want (%d, true); err=%v", code, ok, tt.wantCode, werr)
				}
				if msg := werr.Error(); msg == "" {
					t.Fatal("wait error string is empty")
				}
				return
			}
			if werr != nil {
				t.Fatalf("wait: %v", werr)
			}
			if ok {
				t.Fatalf("ExitCode(nil wait err) = (%d, true), want false", code)
			}
		})
	}
}
