package localapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/execws"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// handleExec enforces the exec feature gate and non-empty argv before any
// WebSocket upgrade; audits exactly one open and close with the peer uid; passes
// argv verbatim to ExecStream.Stream per the T2 shell-join contract; binds the
// stream context to the connection lifetime so client disconnect cancels the
// remote Stream; and joins stdout/stderr copy goroutines, the WebSocket reader,
// and wait before returning.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil || !s.deps.Config.FeatureEnabled(config.FeatureExec) {
		writeError(w, http.StatusForbidden, "feature_disabled", "exec capability is disabled")
		return
	}

	argv := r.URL.Query()["arg"]
	if len(argv) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "exec requires at least one arg query parameter")
		return
	}

	sid, err := execSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "exec session id generation failed")
		return
	}

	conn, rw, err := wsUpgrade(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upgrade", err.Error())
		return
	}
	defer conn.Close()

	host := ""
	if s.deps.ExecStream != nil {
		host = s.deps.ExecStream.Describe().Host
	}
	uid, _ := r.Context().Value(peerUIDKey{}).(int)
	s.deps.Audit.ExecOpen(host, sid, strings.Join(argv, " "), uid, false)
	start := time.Now()

	writeMu := &sync.Mutex{}
	bctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if s.deps.ExecStream == nil {
		errStr := "exec streamer is not configured"
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, sid, 0, errStr, time.Since(start))
		return
	}

	stdin, stdout, stderr, wait, err := s.deps.ExecStream.Stream(bctx, argv...)
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
	go copyExecOutput(conn, writeMu, &wg, copyOneDone, cancel, stdout, execws.ExecStreamStdout)
	go readExecWS(conn, rw, writeMu, &wg, readDone, cancel, stdin)
	go copyExecOutput(conn, writeMu, &wg, copyOneDone, cancel, stderr, execws.ExecStreamStderr)

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
		_ = writeExecFrame(conn, writeMu, execws.ExecFrame{Stream: execws.ExecStreamExit, Code: code})
	} else {
		_ = writeExecFrame(conn, writeMu, execws.ExecFrame{Stream: execws.ExecStreamError, Data: []byte(errStr)})
	}
	_ = writeExecClose(conn, writeMu)
	_ = conn.Close()
	if !readClosed {
		<-readDone
	}
	wg.Wait()

	s.deps.Audit.ExecClose(host, sid, code, errStr, time.Since(start))
}

func copyExecOutput(conn io.Writer, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, src io.Reader, stream string) {
	defer wg.Done()
	defer func() { done <- struct{}{} }()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if werr := writeExecFrame(conn, writeMu, execws.ExecFrame{Stream: stream, Data: buf[:n]}); werr != nil {
				cancel()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func readExecWS(conn io.Writer, rw *bufio.ReadWriter, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, stdin io.WriteCloser) {
	defer wg.Done()
	defer close(done)

	for {
		op, payload, err := execws.ReadFrame(rw, true)
		if err != nil {
			cancel()
			return
		}
		switch op {
		case execws.OpBinary:
			f, err := execws.DecodeExecFrame(payload)
			if err != nil {
				cancel()
				return
			}
			if f.Stream != execws.ExecStreamStdin {
				continue
			}
			if len(f.Data) == 0 {
				_ = stdin.Close()
				continue
			}
			if err := execws.WriteFull(stdin, f.Data); err != nil {
				cancel()
				return
			}
		case execws.OpPing:
			if err := writeExecPong(conn, writeMu, payload); err != nil {
				cancel()
				return
			}
		case execws.OpClose:
			cancel()
			return
		}
	}
}

func writeExecError(w io.Writer, writeMu *sync.Mutex, errStr string) {
	_ = writeExecFrame(w, writeMu, execws.ExecFrame{Stream: execws.ExecStreamError, Data: []byte(errStr)})
	_ = writeExecClose(w, writeMu)
}

func writeExecFrame(w io.Writer, writeMu *sync.Mutex, f execws.ExecFrame) error {
	payload, err := execws.EncodeExecFrame(f)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	return execws.WriteFrame(w, execws.OpBinary, payload, false)
}

func writeExecPong(w io.Writer, writeMu *sync.Mutex, payload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return execws.WriteFrame(w, execws.OpPong, payload, false)
}

func writeExecClose(w io.Writer, writeMu *sync.Mutex) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return execws.WriteClose(w, false, 1000, "")
}

func execSessionID() (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
