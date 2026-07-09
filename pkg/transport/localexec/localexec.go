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
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/ptyx"
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
// status) returns transport.ExitError with best-effort trimmed stderr; the
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
		return stdout, stderr, &transport.ExitError{Code: ee.ExitCode(), Stderr: strings.TrimSpace(stderr)}
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
	wait := func() error {
		werr := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return &transport.ExitError{Code: ee.ExitCode()}
		}
		return werr
	}
	return stdin, stdout, stderr, wait, nil
}

// StreamPty runs the shell-joined command under a local pseudo-terminal.
func (l *Local) StreamPty(ctx context.Context, req transport.PtyRequest, argv ...string) (transport.PtySession, error) {
	var cmd *exec.Cmd
	if len(argv) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-i")
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", join(argv))
	}
	master, err := ptyx.Start(cmd, req.Rows, req.Cols)
	if err != nil {
		return nil, err
	}
	return &localexecPtySession{
		master:   master,
		cmd:      cmd,
		waitDone: make(chan struct{}),
	}, nil
}

type localexecPtySession struct {
	master *os.File
	cmd    *exec.Cmd

	mu     sync.Mutex
	closed bool
	ended  bool

	closeOnce sync.Once
	closeErr  error

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

func (s *localexecPtySession) Read(p []byte) (int, error) {
	n, err := s.master.Read(p)
	if errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (s *localexecPtySession) Write(p []byte) (int, error) {
	return s.master.Write(p)
}

func (s *localexecPtySession) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.ended:
		return errors.New("localexec: resize pty after session ended")
	case s.closed:
		return errors.New("localexec: resize pty after session closed")
	}
	if err := ptyx.Setsize(s.master, rows, cols); err != nil {
		return fmt.Errorf("localexec: resize pty: %w", err)
	}
	return nil
}

func (s *localexecPtySession) Wait() error {
	s.waitOnce.Do(func() {
		defer close(s.waitDone)
		werr := s.cmd.Wait()
		s.mu.Lock()
		s.closeOnce.Do(func() {
			_ = s.master.Close()
		})
		s.ended = true
		s.mu.Unlock()
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			s.waitErr = &transport.ExitError{Code: ee.ExitCode()}
			return
		}
		s.waitErr = werr
	})
	<-s.waitDone
	return s.waitErr
}

func (s *localexecPtySession) Close() error {
	s.mu.Lock()
	s.closeOnce.Do(func() {
		s.closed = true
		s.closeErr = s.master.Close()
		if s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	s.mu.Unlock()
	return s.closeErr
}

// Close is a no-op: there is no persistent resource to stop.
func (l *Local) Close(ctx context.Context) (bool, error) { return false, nil }

// Describe identifies this transport.
func (l *Local) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplLocalExec, Host: "local", Endpoint: ""}
}

var _ transport.Transport = (*Local)(nil)
var _ transport.PtyStreamer = (*Local)(nil)
