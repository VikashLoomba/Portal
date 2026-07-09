package sshctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/ptyx"
)

// StreamPty runs ssh under a local controlling terminal and lets OpenSSH
// propagate window changes and termios state. req.Term is deliberately unused:
// sshctl sets no cmd.Env, so ssh inherits TERM from its environment. Empty argv
// appends nothing after the host, which starts ssh's interactive login shell.
func (s *SSH) StreamPty(ctx context.Context, req transport.PtyRequest, argv ...string) (transport.PtySession, error) {
	args := []string{"-S", s.SockPath}
	args = append(args, s.Opts...)
	args = append(args, "-tt", s.HostID)
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	master, err := ptyx.Start(cmd, req.Rows, req.Cols)
	if err != nil {
		return nil, fmt.Errorf("sshctl: start pty: %w", err)
	}

	return &sshctlPtySession{
		master:   master,
		cmd:      cmd,
		waitDone: make(chan struct{}),
	}, nil
}

type sshctlPtySession struct {
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

func (s *sshctlPtySession) Read(p []byte) (int, error) {
	n, err := s.master.Read(p)
	if errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (s *sshctlPtySession) Write(p []byte) (int, error) {
	return s.master.Write(p)
}

func (s *sshctlPtySession) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.ended:
		return errors.New("sshctl: resize pty after session ended")
	case s.closed:
		return errors.New("sshctl: resize pty after session closed")
	}
	if err := ptyx.Setsize(s.master, rows, cols); err != nil {
		return fmt.Errorf("sshctl: resize pty: %w", err)
	}
	return nil
}

func (s *sshctlPtySession) Wait() error {
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

func (s *sshctlPtySession) Close() error {
	s.mu.Lock()
	s.closeOnce.Do(func() {
		s.closed = true
		s.closeErr = s.master.Close()
		if s.cmd.Process != nil {
			// ptyx.Start makes ssh a session leader. Killing the ssh process is
			// sufficient because it owns the mux channel; there is no local shell
			// tree to reap.
			_ = s.cmd.Process.Kill()
		}
	})
	s.mu.Unlock()
	return s.closeErr
}

var _ transport.PtyStreamer = (*SSH)(nil)
