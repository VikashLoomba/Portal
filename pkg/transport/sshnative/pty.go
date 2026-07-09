package sshnative

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

const (
	defaultPtyTerm = "xterm"
	defaultPtyRows = 24
	defaultPtyCols = 80
)

// StreamPty opens an SSH session backed by a remote pseudo-terminal.
func (c *Client) StreamPty(ctx context.Context, req transport.PtyRequest, argv ...string) (transport.PtySession, error) {
	client, err := c.liveClient(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshnative: new pty session: %w", err)
	}

	term, rows, cols := normalizePtyRequest(req)
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty(term, int(rows), int(cols), modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshnative: request pty: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshnative: pty stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshnative: pty stdout pipe: %w", err)
	}
	if len(argv) == 0 {
		err = sess.Shell()
	} else {
		err = sess.Start(strings.Join(argv, " "))
	}
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshnative: start pty: %w", err)
	}

	ps := &nativePtySession{
		client:   c,
		sess:     sess,
		stdin:    stdin,
		stdout:   stdout,
		ctx:      ctx,
		stopCtx:  watchSessionCtx(ctx, sess),
		waitDone: make(chan struct{}),
	}
	c.registerPtySession(ps)
	return ps, nil
}

func normalizePtyRequest(req transport.PtyRequest) (term string, rows, cols uint16) {
	term = req.Term
	if term == "" {
		term = defaultPtyTerm
	}
	rows = req.Rows
	if rows == 0 {
		rows = defaultPtyRows
	}
	cols = req.Cols
	if cols == 0 {
		cols = defaultPtyCols
	}
	return term, rows, cols
}

type nativePtySession struct {
	client *Client
	sess   *ssh.Session
	stdin  io.Writer
	stdout io.Reader
	ctx    context.Context

	stopCtx  func()
	stopOnce sync.Once

	mu     sync.Mutex
	closed bool
	ended  bool

	closeOnce sync.Once
	closeErr  error

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

func (s *nativePtySession) Read(p []byte) (int, error) {
	return s.stdout.Read(p)
}

func (s *nativePtySession) Write(p []byte) (int, error) {
	return s.stdin.Write(p)
}

func (s *nativePtySession) Resize(rows, cols uint16) error {
	s.mu.Lock()
	closed, ended := s.closed, s.ended
	s.mu.Unlock()
	switch {
	case ended:
		return errors.New("sshnative: resize pty after session ended")
	case closed:
		return errors.New("sshnative: resize pty after session closed")
	}
	if err := s.sess.WindowChange(int(rows), int(cols)); err != nil {
		return fmt.Errorf("sshnative: resize pty: %w", err)
	}
	return nil
}

func (s *nativePtySession) Wait() error {
	s.waitOnce.Do(func() {
		defer close(s.waitDone)
		werr := s.sess.Wait()
		s.stop()
		s.closeOnce.Do(func() {
			_ = s.sess.Close()
		})
		s.markEnded()
		s.client.deregisterPtySession(s)
		s.waitErr = s.mapWaitError(werr)
	})
	<-s.waitDone
	return s.waitErr
}

func (s *nativePtySession) Close() error {
	s.closeOnce.Do(func() {
		s.stop()
		s.markClosed()
		s.closeErr = s.sess.Close()
		s.client.deregisterPtySession(s)
	})
	return s.closeErr
}

func (s *nativePtySession) stop() {
	s.stopOnce.Do(s.stopCtx)
}

func (s *nativePtySession) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *nativePtySession) markEnded() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
}

func (s *nativePtySession) mapWaitError(err error) error {
	if s.ctx != nil && s.ctx.Err() != nil {
		return fmt.Errorf("sshnative: pty: %w", s.ctx.Err())
	}
	if err == nil {
		return nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return &transport.ExitError{Code: ee.ExitStatus(), Signal: ee.Signal()}
	}
	var me *ssh.ExitMissingError
	if errors.As(err, &me) {
		return &transport.ExitError{Code: -1, Signal: "missing"}
	}
	return fmt.Errorf("sshnative: pty wait: %w", err)
}

func (c *Client) registerPtySession(s *nativePtySession) {
	c.ptyMu.Lock()
	defer c.ptyMu.Unlock()
	if c.ptySessions == nil {
		c.ptySessions = make(map[*nativePtySession]struct{})
	}
	c.ptySessions[s] = struct{}{}
}

func (c *Client) deregisterPtySession(s *nativePtySession) {
	c.ptyMu.Lock()
	defer c.ptyMu.Unlock()
	delete(c.ptySessions, s)
}

func (c *Client) closeRegisteredPtySessions() {
	c.ptyMu.Lock()
	sessions := make([]*nativePtySession, 0, len(c.ptySessions))
	for s := range c.ptySessions {
		sessions = append(sessions, s)
	}
	c.ptyMu.Unlock()

	for _, s := range sessions {
		_ = s.Close()
	}
}

func (c *Client) ptySessionCount() int {
	c.ptyMu.Lock()
	defer c.ptyMu.Unlock()
	return len(c.ptySessions)
}
