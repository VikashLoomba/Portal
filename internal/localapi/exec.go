package localapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/wsbits"
)

const execWriteTimeout = time.Second

// handleExec enforces the exec feature gate and argv rules before any
// WebSocket upgrade; audits exactly one open and close with the peer uid; passes
// argv verbatim to the transport per the T2 shell-join contract; binds the
// stream context to the connection lifetime so client disconnect cancels the
// remote stream; and joins copy goroutines, the WebSocket reader, and wait
// before returning.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	stack, release := s.stackView(r.Context())
	defer release()
	if stack.HostKnown && stack.Host == "" {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "no active host is configured")
		return
	}
	if s.deps.Config == nil || !s.deps.Config.FeatureEnabled(config.FeatureExec) {
		writeError(w, http.StatusForbidden, "feature_disabled", "exec capability is disabled")
		return
	}

	q := r.URL.Query()
	pty := q.Get("pty") == "1"
	term := q.Get("term")
	rows := parseUint16(q.Get("rows"))
	cols := parseUint16(q.Get("cols"))
	argv := q["arg"]
	if len(argv) == 0 && !pty {
		writeError(w, http.StatusBadRequest, "invalid_request", "exec requires at least one arg query parameter")
		return
	}
	pStreamer, ptyOK := stack.ExecStream.(transport.PtyStreamer)
	if pty && !ptyOK {
		writeError(w, http.StatusConflict, "pty_unsupported", "the active transport does not support PTY")
		return
	}

	sid, err := execSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "exec session id generation failed")
		return
	}

	var extraHeaders []string
	if pty {
		extraHeaders = append(extraHeaders, "X-Portal-Exec-Pty: 1")
	}
	conn, rw, err := wsUpgrade(w, r, extraHeaders...)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upgrade", err.Error())
		return
	}
	defer conn.Close()
	if stack.Lifetime != nil {
		stopClose := context.AfterFunc(stack.Lifetime, func() { _ = conn.Close() })
		defer stopClose()
	}

	host := stack.Host
	if host == "" && stack.ExecStream != nil {
		host = stack.ExecStream.Describe().Host
	}
	uid, _ := r.Context().Value(peerUIDKey{}).(int)
	s.deps.Audit.ExecOpen(host, sid, strings.Join(argv, " "), uid, pty)
	start := time.Now()

	writeMu := &sync.Mutex{}
	bctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if pty {
		s.bridgeExecPty(conn, rw, writeMu, bctx, cancel, pStreamer, transport.PtyRequest{Term: term, Rows: rows, Cols: cols}, argv, host, sid, start)
		return
	}

	if stack.ExecStream == nil {
		errStr := "exec streamer is not configured"
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, sid, 0, errStr, time.Since(start))
		return
	}

	stdin, stdout, stderr, wait, err := stack.ExecStream.Stream(bctx, argv...)
	if err != nil {
		errStr := err.Error()
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, sid, 0, errStr, time.Since(start))
		return
	}
	if stdin == nil || stdout == nil || stderr == nil || wait == nil {
		cancel()
		if stdin != nil {
			_ = stdin.Close()
		}
		errStr := "exec streamer returned incomplete stream"
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, sid, 0, errStr, time.Since(start))
		return
	}

	var wg sync.WaitGroup
	copyOneDone := make(chan struct{}, 2)
	readDone := make(chan struct{})

	wg.Add(3)
	go copyExecOutput(conn, writeMu, &wg, copyOneDone, cancel, stdout, api.ExecStreamStdout)
	go readExecWS(conn, rw, writeMu, &wg, readDone, cancel, stdin)
	go copyExecOutput(conn, writeMu, &wg, copyOneDone, cancel, stderr, api.ExecStreamStderr)

	copiesDone := 0
	readClosed := false
	for copiesDone < 2 && !readClosed {
		select {
		case <-copyOneDone:
			copiesDone++
		case <-readDone:
			readClosed = true
		}
	}

	// If stdout/stderr drained first, wait must see the real process status;
	// canceling a local CommandContext before Wait can mask a clean exit.
	if readClosed {
		cancel()
		_ = conn.Close()
	}
	_ = stdin.Close()
	for copiesDone < 2 {
		<-copyOneDone
		copiesDone++
	}

	werr := wait()
	cancel()
	code := 0
	errStr := ""
	if c, ok := transport.ExitCode(werr); ok {
		code = c
	} else if werr != nil {
		errStr = werr.Error()
	}

	if errStr == "" {
		_ = writeExecFrame(conn, writeMu, api.ExecFrame{Stream: api.ExecStreamExit, Code: code})
	} else {
		_ = writeExecFrame(conn, writeMu, api.ExecFrame{Stream: api.ExecStreamError, Data: []byte(errStr)})
	}
	_ = writeExecClose(conn, writeMu)
	_ = conn.Close()
	if !readClosed {
		<-readDone
	}
	wg.Wait()

	s.deps.Audit.ExecClose(host, sid, code, errStr, time.Since(start))
}

func (s *Server) bridgeExecPty(conn netConnWriter, rw *bufio.ReadWriter, writeMu *sync.Mutex, bctx context.Context, cancel context.CancelFunc, pStreamer transport.PtyStreamer, req transport.PtyRequest, argv []string, host, sid string, start time.Time) {
	sess, err := pStreamer.StreamPty(bctx, req, argv...)
	if err != nil {
		errStr := err.Error()
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, sid, 0, errStr, time.Since(start))
		return
	}
	if sess == nil {
		cancel()
		errStr := "exec streamer returned nil pty session"
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, sid, 0, errStr, time.Since(start))
		return
	}

	var wg sync.WaitGroup
	outputDone := make(chan struct{})
	readDone := make(chan struct{})

	wg.Add(2)
	go copyExecPtyOutput(conn, writeMu, &wg, outputDone, cancel, sess)
	go readExecPtyWS(conn, rw, writeMu, &wg, readDone, cancel, sess)

	outputClosed := false
	readClosed := false
	select {
	case <-outputDone:
		outputClosed = true
	case <-readDone:
		readClosed = true
	}

	if readClosed {
		cancel()
		_ = conn.Close()
	}

	werr := sess.Wait()
	cancel()
	_ = sess.Close()
	code := 0
	errStr := ""
	if c, ok := transport.ExitCode(werr); ok {
		code = c
	} else if werr != nil {
		errStr = werr.Error()
	}

	if errStr == "" {
		_ = writeExecFrame(conn, writeMu, api.ExecFrame{Stream: api.ExecStreamExit, Code: code})
	} else {
		_ = writeExecFrame(conn, writeMu, api.ExecFrame{Stream: api.ExecStreamError, Data: []byte(errStr)})
	}
	_ = writeExecClose(conn, writeMu)
	_ = conn.Close()
	if !readClosed {
		<-readDone
	}
	if !outputClosed {
		<-outputDone
	}
	wg.Wait()

	s.deps.Audit.ExecClose(host, sid, code, errStr, time.Since(start))
}

type netConnWriter interface {
	io.Writer
	Close() error
	SetWriteDeadline(time.Time) error
}

func copyExecPtyOutput(conn netConnWriter, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, sess transport.PtySession) {
	defer wg.Done()
	defer close(done)

	buf := make([]byte, 32*1024)
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			if werr := writeExecFrame(conn, writeMu, api.ExecFrame{Stream: api.ExecStreamStdout, Data: buf[:n]}); werr != nil {
				cancel()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func readExecPtyWS(conn netConnWriter, rw *bufio.ReadWriter, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, sess transport.PtySession) {
	defer wg.Done()
	defer close(done)

	for {
		op, payload, err := wsbits.ReadFrame(rw, true)
		if err != nil {
			cancel()
			return
		}
		switch op {
		case wsbits.OpBinary:
			f, err := api.DecodeExecFrame(payload)
			if err != nil {
				// Malformed client frames are ignored; only transport failures tear down the session.
				continue
			}
			switch f.Stream {
			case api.ExecStreamStdin:
				if len(f.Data) == 0 {
					// PTY masters cannot half-close; EOF is process exit or disconnect.
					continue
				}
				if err := wsbits.WriteFull(sess, f.Data); err != nil {
					cancel()
					return
				}
			case api.ExecStreamWinch:
				if err := sess.Resize(f.Rows, f.Cols); err != nil {
					cancel()
					return
				}
			}
		case wsbits.OpPing:
			if err := writeExecPong(conn, writeMu, payload); err != nil {
				cancel()
				return
			}
		case wsbits.OpClose:
			cancel()
			return
		}
	}
}

func copyExecOutput(conn netConnWriter, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, src io.Reader, stream string) {
	defer wg.Done()
	defer func() { done <- struct{}{} }()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if werr := writeExecFrame(conn, writeMu, api.ExecFrame{Stream: stream, Data: buf[:n]}); werr != nil {
				cancel()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func readExecWS(conn netConnWriter, rw *bufio.ReadWriter, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, stdin io.WriteCloser) {
	defer wg.Done()
	defer close(done)

	for {
		op, payload, err := wsbits.ReadFrame(rw, true)
		if err != nil {
			cancel()
			return
		}
		switch op {
		case wsbits.OpBinary:
			f, err := api.DecodeExecFrame(payload)
			if err != nil {
				// Malformed client frames are ignored; only transport failures tear down the session.
				continue
			}
			if f.Stream != api.ExecStreamStdin {
				continue
			}
			if len(f.Data) == 0 {
				_ = stdin.Close()
				continue
			}
			if err := wsbits.WriteFull(stdin, f.Data); err != nil {
				cancel()
				return
			}
		case wsbits.OpPing:
			if err := writeExecPong(conn, writeMu, payload); err != nil {
				cancel()
				return
			}
		case wsbits.OpClose:
			cancel()
			return
		}
	}
}

func writeExecError(w netConnWriter, writeMu *sync.Mutex, errStr string) {
	_ = writeExecFrame(w, writeMu, api.ExecFrame{Stream: api.ExecStreamError, Data: []byte(errStr)})
	_ = writeExecClose(w, writeMu)
}

func writeExecFrame(w netConnWriter, writeMu *sync.Mutex, f api.ExecFrame) error {
	payload, err := api.EncodeExecFrame(f)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := w.SetWriteDeadline(time.Now().Add(execWriteTimeout)); err != nil {
		return err
	}
	return wsbits.WriteFrame(w, wsbits.OpBinary, payload, false)
}

func writeExecPong(w netConnWriter, writeMu *sync.Mutex, payload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := w.SetWriteDeadline(time.Now().Add(execWriteTimeout)); err != nil {
		return err
	}
	return wsbits.WriteFrame(w, wsbits.OpPong, payload, false)
}

func writeExecClose(w netConnWriter, writeMu *sync.Mutex) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := w.SetWriteDeadline(time.Now().Add(execWriteTimeout)); err != nil {
		return err
	}
	return wsbits.WriteClose(w, false, 1000, "")
}

func execSessionID() (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func parseUint16(s string) uint16 {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}
