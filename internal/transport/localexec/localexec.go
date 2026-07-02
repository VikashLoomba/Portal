// Package localexec implements transport.Transport by running the
// shell-joined command on THIS machine. It is the faithful dev-mode
// stand-in and the baseline the shared conformance suite runs against:
// because it joins argv and runs `sh -c <joined>` (NOT a direct
// exec.Command(argv[0], argv[1:]...)), its behavior is identical to the ssh
// transports and to every existing pre-quoting consumer, so the conformance
// suite is meaningful across implementations.
//
// localexec deliberately does NOT implement transport.PortForwarder —
// forwarding to yourself is meaningless.
package localexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// Local runs commands on the local machine via `sh -c`.
type Local struct{}

// New returns a ready Local transport.
func New() *Local { return &Local{} }

// join renders argv into the single command string a shell executes, per the
// transport argv contract (single ASCII spaces, target shell re-splits).
func join(argv []string) string { return strings.Join(argv, " ") }

// Ensure is a no-op: there is nothing to build. Always idempotent.
func (l *Local) Ensure(ctx context.Context) (bool, error) { return false, nil }

// Health always reports up; there is no pid ground truth for a subprocess
// model.
func (l *Local) Health(ctx context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 0, Detail: "localexec"}, nil
}

// Exec joins argv and runs `sh -c <joined>` on this machine, feeding stdin and
// capturing stdout/stderr. A non-zero exit (sh propagates the inner command's
// status) returns an error mentioning the exit code and trimmed stderr; the
// stdout/stderr strings are still returned.
func (l *Local) Exec(ctx context.Context, stdin []byte, argv ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", join(argv))
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	stdout, stderr := outBuf.String(), errBuf.String()
	if err == nil {
		return stdout, stderr, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout, stderr, fmt.Errorf("localexec exit %d: %s", ee.ExitCode(), strings.TrimSpace(stderr))
	}
	return stdout, stderr, err
}

// Stream runs the same `sh -c <joined>` command with live pipes; wait is
// cmd.Wait.
func (l *Local) Stream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", join(argv))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, nil, err
	}
	wait := func() error { return cmd.Wait() }
	return stdin, stdout, stderr, wait, nil
}

// Close is a no-op: there is no persistent resource to stop.
func (l *Local) Close(ctx context.Context) (bool, error) { return false, nil }

// Describe identifies this transport.
func (l *Local) Describe() transport.Desc {
	return transport.Desc{Impl: "localexec", Host: "local", Endpoint: ""}
}

var _ transport.Transport = (*Local)(nil)
