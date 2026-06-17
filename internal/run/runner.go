// Package run is the single seam through which every external process is
// spawned. Adapters (sshctl, proc, service, discover) talk only to Runner so a
// FakeRunner can substitute for os/exec in unit tests.
package run

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
)

// Runner spawns external processes. err is non-nil only for spawn or IO
// failures; a non-zero exit is reported via exitCode + stderr because callers
// must inspect those themselves (notably ssh -O forward whose exit code is
// unreliable — success is determined by the absence of "request failed" in
// stderr, see internal/sshctl).
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdin string) (stdout, stderr string, exitCode int, err error)
}

// OSRunner is the production Runner backed by os/exec.
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args []string, stdin string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	stdout := outBuf.String()
	stderrStr := errBuf.String()

	if err == nil {
		return stdout, stderrStr, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout, stderrStr, exitErr.ExitCode(), nil
	}
	return stdout, stderrStr, -1, err
}

// Compile-time assertion.
var _ Runner = OSRunner{}
var _ io.Reader = (*strings.Reader)(nil)
