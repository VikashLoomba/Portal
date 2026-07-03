package localapi

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// ExecFrame is the X7 typed envelope carried in one binary WebSocket message.
// stdin/stdout/stderr/error carry Data; exit carries Code.
type ExecFrame struct {
	Stream string `cbor:"s"`
	Data   []byte `cbor:"d,omitempty"`
	Code   int    `cbor:"c,omitempty"`
}

// ExecStream* names are the stable X7 stream vocabulary. Clients may send only
// stdin; servers may send only stdout, stderr, exit, and error.
const (
	ExecStreamStdin  = "stdin"
	ExecStreamStdout = "stdout"
	ExecStreamStderr = "stderr"
	ExecStreamExit   = "exit"
	ExecStreamError  = "error"
)

// EncodeExecFrame returns the CBOR payload for exactly one ExecFrame envelope.
func EncodeExecFrame(f ExecFrame) ([]byte, error) {
	return cbor.Marshal(f)
}

// DecodeExecFrame decodes exactly one CBOR ExecFrame and rejects malformed
// input before the bridge acts on it.
func DecodeExecFrame(b []byte) (ExecFrame, error) {
	var f ExecFrame
	if err := cbor.Unmarshal(b, &f); err != nil {
		return ExecFrame{}, err
	}
	return f, nil
}

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
	s.deps.Audit.ExecOpen(host, strings.Join(argv, " "), uid)
	start := time.Now()

	writeMu := &sync.Mutex{}
	bctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if s.deps.ExecStream == nil {
		errStr := "exec streamer is not configured"
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, 0, errStr, time.Since(start))
		return
	}

	stdin, stdout, stderr, wait, err := s.deps.ExecStream.Stream(bctx, argv...)
	if err != nil {
		errStr := err.Error()
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, 0, errStr, time.Since(start))
		return
	}
	if stdin == nil || stdout == nil || stderr == nil || wait == nil {
		cancel()
		if stdin != nil {
			_ = stdin.Close()
		}
		errStr := "exec streamer returned incomplete stream"
		writeExecError(conn, writeMu, errStr)
		s.deps.Audit.ExecClose(host, 0, errStr, time.Since(start))
		return
	}

	var wg sync.WaitGroup
	copyOneDone := make(chan struct{}, 2)
	readDone := make(chan struct{})

	wg.Add(3)
	go copyExecOutput(conn, writeMu, &wg, copyOneDone, cancel, stdout, ExecStreamStdout)
	go copyExecOutput(conn, writeMu, &wg, copyOneDone, cancel, stderr, ExecStreamStderr)
	go readExecWS(conn, rw, writeMu, &wg, readDone, cancel, stdin)

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
		_ = writeExecFrame(conn, writeMu, ExecFrame{Stream: ExecStreamExit, Code: code})
	} else {
		_ = writeExecFrame(conn, writeMu, ExecFrame{Stream: ExecStreamError, Data: []byte(errStr)})
	}
	_ = writeExecClose(conn, writeMu)
	_ = conn.Close()
	if !readClosed {
		<-readDone
	}
	wg.Wait()

	s.deps.Audit.ExecClose(host, code, errStr, time.Since(start))
}

func copyExecOutput(conn io.Writer, writeMu *sync.Mutex, wg *sync.WaitGroup, done chan<- struct{}, cancel context.CancelFunc, src io.Reader, stream string) {
	defer wg.Done()
	defer func() { done <- struct{}{} }()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if werr := writeExecFrame(conn, writeMu, ExecFrame{Stream: stream, Data: buf[:n]}); werr != nil {
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
		op, payload, err := wsReadMessage(rw)
		if err != nil {
			cancel()
			return
		}
		switch op {
		case opBinary:
			f, err := DecodeExecFrame(payload)
			if err != nil {
				cancel()
				return
			}
			if f.Stream != ExecStreamStdin {
				continue
			}
			if len(f.Data) == 0 {
				_ = stdin.Close()
				continue
			}
			if err := writeFull(stdin, f.Data); err != nil {
				cancel()
				return
			}
		case opPing:
			if err := writeExecPong(conn, writeMu, payload); err != nil {
				cancel()
				return
			}
		case opClose:
			cancel()
			return
		}
	}
}

func writeExecError(w io.Writer, writeMu *sync.Mutex, errStr string) {
	_ = writeExecFrame(w, writeMu, ExecFrame{Stream: ExecStreamError, Data: []byte(errStr)})
	_ = writeExecClose(w, writeMu)
}

func writeExecFrame(w io.Writer, writeMu *sync.Mutex, f ExecFrame) error {
	payload, err := EncodeExecFrame(f)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	return wsWriteBinary(w, payload)
}

func writeExecPong(w io.Writer, writeMu *sync.Mutex, payload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return wsWritePong(w, payload)
}

func writeExecClose(w io.Writer, writeMu *sync.Mutex) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return wsWriteClose(w, 1000, "")
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
