package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/localexec"
	"github.com/VikashLoomba/Portal/pkg/wsbits"
	"github.com/fxamacker/cbor/v2"
)

func TestExecUpgradeReaches101(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"printf", "hello"})
	c.Close()
}

func TestExecPrintfStdoutExitZero(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"printf", "hello"})
	defer c.Close()

	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := joinFrameData(frames, api.ExecStreamStdout); got != "hello" {
		t.Fatalf("stdout = %q, want hello (bridge must pass argv verbatim)", got)
	}
	if got := lastExitCode(frames); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestExecNonZeroExitFrame(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"sh", "-c", "'exit 4'"})
	defer c.Close()

	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := lastExitCode(frames); got != 4 {
		t.Fatalf("exit code = %d, want 4", got)
	}
}

func TestExecStdinHalfClose(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"cat"})
	defer c.Close()

	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte("ping\n")})
	stdout := readExecFrameMatching(t, c, api.ExecStreamStdout, 2*time.Second)
	if string(stdout.Data) != "ping\n" {
		t.Fatalf("stdout = %q, want ping newline", string(stdout.Data))
	}

	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte{}})
	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := lastExitCode(frames); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestExecMalformedApplicationFrameDoesNotTearDownSession(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"cat"})
	defer c.Close()

	payload := oversizedWinchExecFramePayload(t)
	if _, err := api.DecodeExecFrame(payload); err == nil {
		t.Fatal("api.DecodeExecFrame accepted oversized winch rows; regression test no longer covers malformed app frames")
	}
	if err := writeClientFrame(c, wsbits.OpBinary, payload); err != nil {
		t.Fatalf("write malformed client frame: %v", err)
	}
	assertNoTerminalFrameWithin(t, c, 200*time.Millisecond)

	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte("alive\n")})
	stdout := readExecFrameMatching(t, c, api.ExecStreamStdout, 2*time.Second)
	if string(stdout.Data) != "alive\n" {
		t.Fatalf("stdout = %q, want alive newline", string(stdout.Data))
	}
	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte{}})
	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := lastExitCode(frames); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestExecFeatureOffNoUpgrade(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.SetFeature(config.FeatureExec, false); err != nil {
		t.Fatalf("SetFeature(exec,false): %v", err)
	}
	path, _ := startExecServer(t, cfg, audit.New(t.TempDir()), localexec.New())

	status, _, body := rawExecHTTP(t, path, []string{"printf", "hello"})
	if !strings.Contains(status, "403") {
		t.Fatalf("status = %q, want 403", status)
	}
	if strings.Contains(status, "101") {
		t.Fatalf("disabled exec upgraded unexpectedly: %q", status)
	}
	var eb api.ErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	if eb.Error.Code != "feature_disabled" {
		t.Fatalf("error code = %q, want feature_disabled", eb.Error.Code)
	}
}

func TestExecAuditOpenCloseOnce(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())
	c := dialExecWS(t, path, []string{"printf", "hello"})
	defer c.Close()

	_ = readExecFramesUntilExit(t, c, 2*time.Second)
	lines := waitAuditLines(t, a.Path(), 2, 2*time.Second)
	var opens, closes int
	wantUIDField := fmt.Sprintf("\tuid=%d\t", os.Getuid())
	var openSID, closeSID string
	for _, line := range lines {
		if strings.Contains(line, "\texec-open\t") {
			opens++
			fields := auditFields(line)
			openSID = fields["sid"]
			for _, token := range []string{"host=local", "sid=", wantUIDField, "argv=printf hello"} {
				if !strings.Contains(line, token) {
					t.Fatalf("exec-open missing %q: %s", token, line)
				}
			}
			if !strings.Contains(line, "\thost=local\tsid=") {
				t.Fatalf("exec-open sid is not immediately after host: %s", line)
			}
			if strings.Contains(line, "\tpty=1") {
				t.Fatalf("non-pty exec-open unexpectedly carries pty=1: %s", line)
			}
		}
		if strings.Contains(line, "\texec-close\t") {
			closes++
			fields := auditFields(line)
			closeSID = fields["sid"]
			for _, token := range []string{"host=local", "sid=", "code=0", "err=", "dur="} {
				if !strings.Contains(line, token) {
					t.Fatalf("exec-close missing %q: %s", token, line)
				}
			}
			if !strings.Contains(line, "\thost=local\tsid=") {
				t.Fatalf("exec-close sid is not immediately after host: %s", line)
			}
		}
	}
	if opens != 1 || closes != 1 {
		t.Fatalf("audit exec-open=%d exec-close=%d, want 1 each\n%s", opens, closes, strings.Join(lines, "\n"))
	}
	if openSID == "" || closeSID == "" || openSID != closeSID {
		t.Fatalf("audit sid open=%q close=%q, want same non-empty\n%s", openSID, closeSID, strings.Join(lines, "\n"))
	}
}

func TestExecAuditPtyOpenFlagAndSID(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())
	c := dialExecWSWithQuery(t, path, []string{"printf", "hello"}, url.Values{"pty": {"1"}})
	defer c.Close()

	_ = readExecFramesUntilExit(t, c, 2*time.Second)
	lines := waitAuditLines(t, a.Path(), 2, 2*time.Second)
	var openSID, closeSID string
	for _, line := range lines {
		fields := auditFields(line)
		switch {
		case strings.Contains(line, "\texec-open\t"):
			openSID = fields["sid"]
			for _, token := range []string{"host=local", "sid=", "argv=printf hello", "pty=1"} {
				if !strings.Contains(line, token) {
					t.Fatalf("pty exec-open missing %q: %s", token, line)
				}
			}
		case strings.Contains(line, "\texec-close\t"):
			closeSID = fields["sid"]
		}
	}
	if openSID == "" || closeSID == "" || openSID != closeSID {
		t.Fatalf("audit sid open=%q close=%q, want same non-empty\n%s", openSID, closeSID, strings.Join(lines, "\n"))
	}
}

func TestExecAuditConcurrentSessionsPairBySID(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())

	one := dialExecWS(t, path, []string{"sh", "-c", "'sleep 0.1; printf one'"})
	defer one.Close()
	two := dialExecWS(t, path, []string{"sh", "-c", "'sleep 0.1; printf two'"})
	defer two.Close()

	_ = readExecFramesUntilExit(t, one, 2*time.Second)
	_ = readExecFramesUntilExit(t, two, 2*time.Second)

	lines := waitAuditLines(t, a.Path(), 4, 2*time.Second)
	opens := make(map[string]string)
	closes := make(map[string]string)
	for _, line := range lines {
		fields := auditFields(line)
		switch {
		case strings.Contains(line, "\texec-open\t"):
			opens[fields["sid"]] = line
		case strings.Contains(line, "\texec-close\t"):
			closes[fields["sid"]] = line
		}
	}
	if len(opens) != 2 || len(closes) != 2 {
		t.Fatalf("open/close counts = %d/%d, want 2/2\n%s", len(opens), len(closes), strings.Join(lines, "\n"))
	}
	for sid, openLine := range opens {
		if sid == "" {
			t.Fatalf("open missing sid: %s", openLine)
		}
		if _, ok := closes[sid]; !ok {
			t.Fatalf("open sid %q has no matching close\n%s", sid, strings.Join(lines, "\n"))
		}
	}
}

func TestExecEmptyArgvReturnsInvalidRequestWithoutUpgrade(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())

	status, _, body := rawExecHTTP(t, path, nil)
	if !strings.Contains(status, "400") {
		t.Fatalf("status = %q, want 400", status)
	}
	if strings.Contains(status, "101") {
		t.Fatalf("empty argv upgraded unexpectedly: %q", status)
	}
	var eb api.ErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	if eb.Error.Code != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request", eb.Error.Code)
	}
}

func TestExecPtyUnsupportedNoUpgrade(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), unsupportedPtyExecStreamer{})

	status, _, body := rawExecHTTPWithQuery(t, path, nil, url.Values{"pty": {"1"}})
	if !strings.Contains(status, "409") {
		t.Fatalf("status = %q, want 409", status)
	}
	if strings.Contains(status, "101") {
		t.Fatalf("unsupported pty upgraded unexpectedly: %q", status)
	}
	var eb api.ErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	if eb.Error.Code != "pty_unsupported" {
		t.Fatalf("error code = %q, want pty_unsupported", eb.Error.Code)
	}
}

func TestExecPtyCapabilityHeaderConfirm(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())

	ptyConn := dialExecWSWithQuery(t, path, []string{"printf", "hello"}, url.Values{"pty": {"1"}})
	if got := ptyConn.headers.Get("X-Portal-Exec-Pty"); got != "1" {
		ptyConn.Close()
		t.Fatalf("X-Portal-Exec-Pty = %q, want 1", got)
	}
	ptyConn.Close()

	plainConn := dialExecWS(t, path, []string{"printf", "hello"})
	defer plainConn.Close()
	if got := plainConn.headers.Get("X-Portal-Exec-Pty"); got != "" {
		t.Fatalf("non-pty X-Portal-Exec-Pty = %q, want absent", got)
	}
}

func TestExecPtySttySize(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWSWithQuery(t, path, []string{"stty", "size"}, url.Values{
		"pty":  {"1"},
		"rows": {"40"},
		"cols": {"100"},
	})
	defer c.Close()

	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	stdout := cleanPtyOutput(joinFrameData(frames, api.ExecStreamStdout))
	if !strings.Contains(stdout, "40 100") {
		t.Fatalf("stdout = %q, want stty size 40 100", stdout)
	}
	if got := countFrames(frames, api.ExecStreamStderr); got != 0 {
		t.Fatalf("stderr frames = %d, want 0 for pty", got)
	}
}

func TestExecPtyWinchResizesSession(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWSWithQuery(t, path, []string{"sh", "-c", "'stty size; read _; stty size'"}, url.Values{
		"pty":  {"1"},
		"rows": {"40"},
		"cols": {"100"},
	})
	defer c.Close()

	initial := readExecFramesUntilStdoutContains(t, c, "40 100", 2*time.Second)
	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamWinch, Rows: 50, Cols: 120})
	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte("go\n")})
	rest := readExecFramesUntilExit(t, c, 2*time.Second)

	frames := append(initial, rest...)
	stdout := cleanPtyOutput(joinFrameData(frames, api.ExecStreamStdout))
	for _, want := range []string{"40 100", "50 120"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
	if got := countFrames(frames, api.ExecStreamStderr); got != 0 {
		t.Fatalf("stderr frames = %d, want 0 for pty", got)
	}
}

func TestExecPtyMalformedWinchFrameDoesNotTearDownSession(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWSWithQuery(t, path, []string{"sh", "-c", "'stty size; read _; stty size'"}, url.Values{
		"pty":  {"1"},
		"rows": {"40"},
		"cols": {"100"},
	})
	defer c.Close()

	initial := readExecFramesUntilStdoutContains(t, c, "40 100", 2*time.Second)
	payload := oversizedWinchExecFramePayload(t)
	if _, err := api.DecodeExecFrame(payload); err == nil {
		t.Fatal("api.DecodeExecFrame accepted oversized winch rows; regression test no longer covers malformed app frames")
	}
	if err := writeClientFrame(c, wsbits.OpBinary, payload); err != nil {
		t.Fatalf("write malformed client frame: %v", err)
	}
	assertNoTerminalFrameWithin(t, c, 200*time.Millisecond)

	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamWinch, Rows: 50, Cols: 120})
	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte("go\n")})
	rest := readExecFramesUntilExit(t, c, 2*time.Second)

	frames := append(initial, rest...)
	stdout := cleanPtyOutput(joinFrameData(frames, api.ExecStreamStdout))
	if !strings.Contains(stdout, "50 120") {
		t.Fatalf("stdout = %q, want valid winch size 50 120 after malformed frame", stdout)
	}
	if got := lastExitCode(frames); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestExecPtyZeroLengthStdinNoOp(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWSWithQuery(t, path, []string{"sh", "-c", "'read line; printf \"%s\\n\" \"$line\"'"}, url.Values{"pty": {"1"}})
	defer c.Close()

	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte{}})
	assertNoTerminalFrameWithin(t, c, 200*time.Millisecond)
	writeExecClientFrame(t, c, api.ExecFrame{Stream: api.ExecStreamStdin, Data: []byte("alive\n")})

	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	stdout := cleanPtyOutput(joinFrameData(frames, api.ExecStreamStdout))
	if !strings.Contains(stdout, "alive") {
		t.Fatalf("stdout = %q, want subsequent stdin to reach pty process", stdout)
	}
}

func TestExecClientDisconnectCancelsStream(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())
	baseline := runtime.NumGoroutine()

	c := dialExecWS(t, path, []string{"sh", "-c", "'sleep 5; echo hi'"})
	_ = c.Close()

	_ = waitAuditLines(t, a.Path(), 2, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = runtime.NumGoroutine()
		if got <= baseline+4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines after disconnect = %d, baseline = %d", got, baseline)
}

func TestExecBridgeNoGoroutineLeakAcrossOrderings(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())

	warm := dialExecWS(t, path, []string{"printf", "warm"})
	_ = readExecFramesUntilExit(t, warm, 2*time.Second)
	_ = warm.Close()
	baseline := settledGoroutines()

	for i := 0; i < 5; i++ {
		normal := dialExecWS(t, path, []string{"printf", "hello"})
		frames := readExecFramesUntilExit(t, normal, 2*time.Second)
		if got := joinFrameData(frames, api.ExecStreamStdout); got != "hello" {
			normal.Close()
			t.Fatalf("normal stdout = %q, want hello", got)
		}
		if got := lastExitCode(frames); got != 0 {
			normal.Close()
			t.Fatalf("normal exit = %d, want 0", got)
		}
		_ = normal.Close()

		// The process sleeps longer than the settle window below. Passing this
		// test proves the connection close cancels Stream instead of leaving
		// bridge goroutines blocked until the remote command exits naturally.
		disconnected := dialExecWS(t, path, []string{"sh", "-c", "'sleep 5; echo hi'"})
		_ = disconnected.Close()

		nonZero := dialExecWS(t, path, []string{"sh", "-c", "'exit 4'"})
		frames = readExecFramesUntilExit(t, nonZero, 2*time.Second)
		if got := lastExitCode(frames); got != 4 {
			nonZero.Close()
			t.Fatalf("non-zero exit = %d, want 4", got)
		}
		_ = nonZero.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = runtime.NumGoroutine()
		if got <= baseline+8 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines after exec ordering stress = %d, baseline = %d, want <= baseline+8", got, baseline)
}

func TestExecBridgeDoesNotDoubleShellArgv(t *testing.T) {
	src, err := os.ReadFile("exec.go")
	if err != nil {
		t.Fatalf("read exec.go: %v", err)
	}
	if bytes.Contains(src, []byte(`"sh", "-c"`)) {
		t.Fatal(`exec.go contains bridge-level "sh", "-c" wrapping; argv must pass through to Stream`)
	}
	if !bytes.Contains(src, []byte("Stream(bctx, argv...)")) {
		t.Fatal("exec.go does not contain Stream(bctx, argv...) passthrough")
	}
}

func TestNoThirdPartyWebSocketImportsAndGoModPinned(t *testing.T) {
	root := moduleRoot(t)

	banned := map[string]bool{
		"github.com/gorilla/websocket": true,
		"nhooyr.io/websocket":          true,
		"golang.org/x/net/websocket":   true,
	}
	for _, dir := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".go" {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				if banned[importPath] {
					t.Errorf("%s imports forbidden websocket package %q", path, importPath)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// Stage 5 uses in-tree WebSocket framing: the require set stays pinned to
	// main, including the Stage-4 x/crypto dependency and existing indirect
	// x/net module, but no websocket subpackage dependency is introduced.
	got := requiredModules(t, filepath.Join(root, "go.mod"))
	want := map[string]bool{
		"github.com/fxamacker/cbor/v2":         true,
		"github.com/mdlayher/netlink":          true,
		"github.com/spf13/cobra":               true,
		"golang.org/x/crypto":                  true,
		"golang.org/x/sys":                     true,
		"github.com/google/go-cmp":             true,
		"github.com/inconshreveable/mousetrap": true,
		"github.com/mdlayher/socket":           true,
		"github.com/spf13/pflag":               true,
		"github.com/x448/float16":              true,
		"golang.org/x/net":                     true,
		"golang.org/x/sync":                    true,
	}
	if missing, extra := moduleSetDiff(want, got); len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("go.mod require set changed; missing=%v extra=%v", missing, extra)
	}
}

func TestExecMalformedInboundFrameTearsDownSession(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"cat"})
	defer c.Close()

	if _, err := c.Write(oversizedMaskedFrameHeader(wsbits.MaxPayload + 1)); err != nil {
		t.Fatalf("write oversized frame header: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		op, _, err := readServerFrame(c, time.Until(deadline))
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			return
		}
		if op == wsbits.OpClose {
			return
		}
	}
	t.Fatal("server did not close or end the exec session after malformed inbound frame")
}

func TestExecReaderCloseReleasesBlockedOutputWriter(t *testing.T) {
	a := audit.New(t.TempDir())
	streamer := &blockedOutputExecStreamer{waitCalled: make(chan struct{})}
	s := New(Deps{
		Version:    api.VersionInfo{Version: "9.9"},
		Config:     config.New(t.TempDir()),
		ExecStream: streamer,
		Audit:      a,
	})

	server, client := net.Pipe()
	defer client.Close()
	w := newHijackResponseWriter(server)
	r := wsUpgradeRequest("dGhlIHNhbXBsZSBub25jZQ==")
	r.Method = http.MethodPost
	q := r.URL.Query()
	q.Add("arg", "spammer")
	r.URL.RawQuery = q.Encode()

	done := make(chan struct{})
	go func() {
		s.handleExec(w, r)
		close(done)
	}()

	br := bufio.NewReader(client)
	tp := textproto.NewReader(br)
	status, err := tp.ReadLine()
	if err != nil {
		t.Fatalf("read upgrade status: %v", err)
	}
	if status != "HTTP/1.1 101 Switching Protocols" {
		t.Fatalf("upgrade status = %q, want 101", status)
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		t.Fatalf("read upgrade headers: %v", err)
	}

	// handleExec may synchronously close the net.Pipe after reading OpClose;
	// no further client writes follow, so leave this deadline in place.
	if err := client.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	if err := writeClientFrame(client, wsbits.OpClose, []byte{0x03, 0xe8}); err != nil {
		t.Fatalf("write close frame: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleExec did not return after client close with blocked output writer")
	}
	select {
	case <-streamer.waitCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("stream wait was not called")
	}
	lines := waitAuditLines(t, a.Path(), 2, 2*time.Second)
	if !strings.Contains(lines[len(lines)-1], "\texec-close\t") {
		t.Fatalf("last audit line = %q, want exec-close", lines[len(lines)-1])
	}
}

type blockedOutputExecStreamer struct {
	waitCalled chan struct{}
}

func (s *blockedOutputExecStreamer) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	wait := func() error {
		close(s.waitCalled)
		return nil
	}
	return nopWriteCloser{}, io.NopCloser(infiniteReader{}), io.NopCloser(bytes.NewReader(nil)), wait, nil
}

func (s *blockedOutputExecStreamer) Describe() transport.Desc {
	return transport.Desc{Impl: "test", Host: "blocked-output", Endpoint: "net.Pipe"}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }

func (nopWriteCloser) Close() error { return nil }

type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type unsupportedPtyExecStreamer struct{}

func (unsupportedPtyExecStreamer) Stream(ctx context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, fmt.Errorf("unexpected Stream call")
}

func (unsupportedPtyExecStreamer) Describe() transport.Desc {
	return transport.Desc{Impl: "test", Host: "pipe-only", Endpoint: "net.Pipe"}
}

type wsTestConn struct {
	net.Conn
	br      *bufio.Reader
	status  string
	headers textproto.MIMEHeader
}

func startExecServer(t *testing.T, cfg *config.Store, a *audit.Log, streamer ExecStreamer) (string, *Server) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "api.sock")
	s := New(Deps{
		Version:    api.VersionInfo{Version: "9.9"},
		Config:     cfg,
		ExecStream: streamer,
		Audit:      a,
	})
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exec server did not stop")
		}
	})
	waitVersion(t, path)
	return path, s
}

func dialExecWS(t *testing.T, path string, argv []string) *wsTestConn {
	t.Helper()
	return dialExecWSWithQuery(t, path, argv, nil)
}

func dialExecWSWithQuery(t *testing.T, path string, argv []string, query url.Values) *wsTestConn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	q := url.Values{}
	for key, values := range query {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	for _, arg := range argv {
		q.Add("arg", arg)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	target := execTarget(q)
	if _, err := fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: unix\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", target, key); err != nil {
		c.Close()
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(c)
	tp := textproto.NewReader(br)
	status, err := tp.ReadLine()
	if err != nil {
		c.Close()
		t.Fatalf("read status: %v", err)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		c.Close()
		t.Fatalf("read headers: %v", err)
	}
	if status != "HTTP/1.1 101 Switching Protocols" {
		c.Close()
		t.Fatalf("status = %q, want 101", status)
	}
	if got, want := hdr.Get("Sec-WebSocket-Accept"), wsbits.AcceptKey(key); got != want {
		c.Close()
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	return &wsTestConn{Conn: c, br: br, status: status, headers: hdr}
}

func rawExecHTTP(t *testing.T, path string, argv []string) (string, textproto.MIMEHeader, []byte) {
	t.Helper()
	return rawExecHTTPWithQuery(t, path, argv, nil)
}

func rawExecHTTPWithQuery(t *testing.T, path string, argv []string, query url.Values) (string, textproto.MIMEHeader, []byte) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer c.Close()
	q := url.Values{}
	for key, values := range query {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	for _, arg := range argv {
		q.Add("arg", arg)
	}
	target := execTarget(q)
	if _, err := fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: unix\r\nUpgrade: websocket\r\nConnection: close, Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", target); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(c)
	req, err := http.NewRequest(http.MethodPost, "http://unix"+target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.Status, textproto.MIMEHeader(resp.Header), body
}

func execTarget(q url.Values) string {
	if len(q) == 0 {
		return "/v1/exec"
	}
	return "/v1/exec?" + q.Encode()
}

func writeExecClientFrame(t *testing.T, c *wsTestConn, f api.ExecFrame) {
	t.Helper()
	payload, err := api.EncodeExecFrame(f)
	if err != nil {
		t.Fatalf("api.EncodeExecFrame: %v", err)
	}
	if err := writeClientFrame(c, wsbits.OpBinary, payload); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
}

func oversizedWinchExecFramePayload(t *testing.T) []byte {
	t.Helper()
	payload, err := cbor.Marshal(struct {
		Stream string `cbor:"s"`
		Rows   uint32 `cbor:"rs"`
		Cols   uint16 `cbor:"cs"`
	}{
		Stream: api.ExecStreamWinch,
		Rows:   70000,
		Cols:   100,
	})
	if err != nil {
		t.Fatalf("marshal oversized winch frame: %v", err)
	}
	return payload
}

func writeClientFrame(c net.Conn, op wsbits.Opcode, payload []byte) error {
	return wsbits.WriteFrame(c, op, payload, true)
}

func oversizedMaskedFrameHeader(n uint64) []byte {
	frame := []byte{0x80 | byte(wsbits.OpBinary), 0x80 | 127, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(frame[2:10], n)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

func readExecFrameMatching(t *testing.T, c *wsTestConn, stream string, timeout time.Duration) api.ExecFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for %s frame", stream)
		}
		f := readExecFrame(t, c, remaining)
		if f.Stream == stream {
			return f
		}
		if f.Stream == api.ExecStreamError || f.Stream == api.ExecStreamExit {
			t.Fatalf("got terminal frame before %s: %+v", stream, f)
		}
	}
}

func readExecFramesUntilExit(t *testing.T, c *wsTestConn, timeout time.Duration) []api.ExecFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var frames []api.ExecFrame
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for exit frame; got %+v", frames)
		}
		f := readExecFrame(t, c, remaining)
		frames = append(frames, f)
		switch f.Stream {
		case api.ExecStreamExit:
			return frames
		case api.ExecStreamError:
			t.Fatalf("error frame: %s", string(f.Data))
		}
	}
}

func readExecFramesUntilStdoutContains(t *testing.T, c *wsTestConn, want string, timeout time.Duration) []api.ExecFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var frames []api.ExecFrame
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for stdout containing %q; got %+v", want, frames)
		}
		f := readExecFrame(t, c, remaining)
		frames = append(frames, f)
		switch f.Stream {
		case api.ExecStreamStdout:
			if strings.Contains(cleanPtyOutput(joinFrameData(frames, api.ExecStreamStdout)), want) {
				return frames
			}
		case api.ExecStreamExit, api.ExecStreamError:
			t.Fatalf("terminal frame before stdout contained %q: %+v", want, f)
		}
	}
}

func assertNoTerminalFrameWithin(t *testing.T, c *wsTestConn, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		op, payload, err := readServerFrame(c, remaining)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return
			}
			t.Fatalf("read server frame: %v", err)
		}
		if op != wsbits.OpBinary {
			continue
		}
		f, err := api.DecodeExecFrame(payload)
		if err != nil {
			t.Fatalf("api.DecodeExecFrame: %v", err)
		}
		if f.Stream == api.ExecStreamExit || f.Stream == api.ExecStreamError {
			t.Fatalf("terminal frame arrived during no-op window: %+v", f)
		}
	}
}

func readExecFrame(t *testing.T, c *wsTestConn, timeout time.Duration) api.ExecFrame {
	t.Helper()
	for {
		op, payload, err := readServerFrame(c, timeout)
		if err != nil {
			t.Fatalf("read server frame: %v", err)
		}
		if op == wsbits.OpClose {
			t.Fatal("server close before exec terminal frame")
		}
		if op != wsbits.OpBinary {
			continue
		}
		f, err := api.DecodeExecFrame(payload)
		if err != nil {
			t.Fatalf("api.DecodeExecFrame: %v", err)
		}
		return f
	}
}

func readServerFrame(c *wsTestConn, timeout time.Duration) (wsbits.Opcode, []byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})

	return wsbits.ReadFrame(c.br, false)
}

func joinFrameData(frames []api.ExecFrame, stream string) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Stream == stream {
			b.Write(f.Data)
		}
	}
	return b.String()
}

func cleanPtyOutput(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}

func countFrames(frames []api.ExecFrame, stream string) int {
	var n int
	for _, f := range frames {
		if f.Stream == stream {
			n++
		}
	}
	return n
}

func lastExitCode(frames []api.ExecFrame) int {
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Stream == api.ExecStreamExit {
			return frames[i].Code
		}
	}
	return -1
}

func waitAuditLines(t *testing.T, path string, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) >= want && lines[0] != "" {
				return lines
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("audit log did not reach %d lines:\n%s", want, string(b))
	return nil
}

func auditFields(line string) map[string]string {
	fields := make(map[string]string)
	for _, col := range strings.Split(line, "\t") {
		k, v, ok := strings.Cut(col, "=")
		if ok {
			fields[k] = v
		}
	}
	return fields
}

func settledGoroutines() int {
	last := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		now := runtime.NumGoroutine()
		if now == last {
			return now
		}
		last = now
	}
	return last
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test working directory")
		}
		dir = parent
	}
}

func requiredModules(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	mods := make(map[string]bool)
	inRequire := false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "//"):
			continue
		case strings.HasPrefix(trimmed, "require ("):
			inRequire = true
			continue
		case inRequire && trimmed == ")":
			inRequire = false
			continue
		case inRequire:
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				mods[fields[0]] = true
			}
		case strings.HasPrefix(trimmed, "require "):
			fields := strings.Fields(trimmed)
			if len(fields) >= 3 {
				mods[fields[1]] = true
			}
		}
	}
	return mods
}

func moduleSetDiff(want, got map[string]bool) (missing, extra []string) {
	for mod := range want {
		if !got[mod] {
			missing = append(missing, mod)
		}
	}
	for mod := range got {
		if !want[mod] {
			extra = append(extra, mod)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}
