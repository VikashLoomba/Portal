package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
)

// fakeClip lets us drive the Ctrl+V interception logic deterministically.
type fakeClip struct {
	has bool
	png []byte
	err error
}

func (f *fakeClip) HasImage() bool            { return f.has }
func (f *fakeClip) ImagePNG() ([]byte, error) { return f.png, f.err }
func (f *fakeClip) Describe() string          { return "fake" }

// discardLog is a no-op logger for tests.
var discardLog = log.New(io.Discard, "", 0)

// expectedRemotePath mirrors clipupload's content-addressed naming so a fake
// transport can echo back a path that passes clipupload's validateRemotePath
// (which rejects any path that does not end in the sha256-derived basename).
func expectedRemotePath(png []byte) string {
	sum := sha256.Sum256(png)
	name := "clip-" + hex.EncodeToString(sum[:])[:32] + ".png"
	return "/home/u/.cache/portal/clip/" + name
}

// fakeUploadTransport records ExecBytes and returns a remote path the real
// clipupload.Upload will accept. By default it content-addresses the path from
// the uploaded PNG (so Upload's validation passes); badPath forces an invalid
// path (to drive the upload-error branch) and execErr forces a transport error.
type fakeUploadTransport struct {
	mu       sync.Mutex
	gotStdin []byte
	badPath  bool          // return a path Upload will reject
	execErr  error         // return this error from ExecBytes
	delay    time.Duration // block this long (honoring ctx) before returning
}

func (f *fakeUploadTransport) Host() string                                    { return "h" }
func (f *fakeUploadTransport) Sock() string                                    { return "/tmp/s" }
func (f *fakeUploadTransport) MasterPID(context.Context) (int, error)          { return 1, nil }
func (f *fakeUploadTransport) EnsureMaster(context.Context) (int, bool, error) { return 1, false, nil }
func (f *fakeUploadTransport) Forward(context.Context, int, int) error         { return nil }
func (f *fakeUploadTransport) Cancel(context.Context, int, int) error          { return nil }
func (f *fakeUploadTransport) Exit(context.Context) (bool, error)              { return false, nil }
func (f *fakeUploadTransport) Exec(context.Context, string, ...string) (string, error) {
	return "", nil
}
func (f *fakeUploadTransport) ExecBytes(ctx context.Context, stdin []byte, _ ...string) (string, string, error) {
	f.mu.Lock()
	f.gotStdin = append([]byte(nil), stdin...)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if f.execErr != nil {
		return "", "boom", f.execErr
	}
	if f.badPath {
		return "/wrong/path.png", "", nil
	}
	return expectedRemotePath(stdin), "", nil
}
func (f *fakeUploadTransport) stdin() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.gotStdin...)
}
func (f *fakeUploadTransport) ExecStream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}

// compile-time check that the fake satisfies the real interface.
var _ sshctl.Transport = (*fakeUploadTransport)(nil)

// TestWriteWithPaste_NoImage: Ctrl+V passes through untouched when the
// clipboard has no image.
func TestWriteWithPaste_NoImage(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: false}
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte{'a', ctrlV, 'b'}, &out, cb, tr, discardLog)
	if !bytes.Equal(out.Bytes(), []byte{'a', ctrlV, 'b'}) {
		t.Errorf("no-image passthrough: got %v, want [a 0x16 b]", out.Bytes())
	}
	if tr.stdin() != nil && len(tr.stdin()) > 0 {
		t.Errorf("no upload should have happened")
	}
}

// TestWriteWithPaste_WithImage: Ctrl+V is swallowed and replaced by the
// uploaded remote path; surrounding bytes are preserved.
func TestWriteWithPaste_WithImage(t *testing.T) {
	var out bytes.Buffer
	png := []byte("\x89PNG fake")
	cb := &fakeClip{has: true, png: png}
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte{'x', ctrlV, 'y'}, &out, cb, tr, discardLog)

	want := "x" + expectedRemotePath(png) + "y"
	if out.String() != want {
		t.Errorf("with-image: got %q, want %q", out.String(), want)
	}
	if !bytes.Equal(tr.stdin(), png) {
		t.Errorf("upload stdin: got %q, want the PNG bytes", tr.stdin())
	}
}

// TestWriteWithPaste_NoCtrlV: ordinary input is forwarded verbatim.
func TestWriteWithPaste_NoCtrlV(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: true} // even if clipboard has image, no Ctrl+V = no action
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte("hello world"), &out, cb, tr, discardLog)
	if out.String() != "hello world" {
		t.Errorf("got %q, want %q", out.String(), "hello world")
	}
	if len(tr.stdin()) > 0 {
		t.Errorf("no upload should have happened")
	}
}

// TestWriteWithPaste_ImagePNGError: when extracting the clipboard image FAILS
// (ImagePNG error, before any upload), a bell is emitted and no path injected.
func TestWriteWithPaste_ImagePNGError(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: true, err: errors.New("clip read failed")}
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte{ctrlV}, &out, cb, tr, discardLog)
	if out.String() != "\x07" {
		t.Errorf("expected bell on ImagePNG error, got %q", out.String())
	}
	if len(tr.stdin()) > 0 {
		t.Errorf("no upload should happen when ImagePNG fails")
	}
}

// TestWriteWithPaste_UploadError: when the clipboard image extracts fine but the
// UPLOAD itself fails (transport error), a bell is emitted and no path injected.
// (The old test of this name actually drove the ImagePNG-error path; this one
// exercises a genuine upload failure, which is the documented bell-on-failure.)
func TestWriteWithPaste_UploadError(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: true, png: []byte("\x89PNG fake")}
	tr := &fakeUploadTransport{execErr: errors.New("ssh exec failed")}
	writeWithPaste(context.Background(), []byte{ctrlV}, &out, cb, tr, discardLog)
	if out.String() != "\x07" {
		t.Errorf("expected bell on upload error, got %q", out.String())
	}
	if len(tr.stdin()) == 0 {
		t.Errorf("upload should have been attempted (PNG sent to transport)")
	}
}

// TestWriteWithPaste_KittyEncoding: Ctrl+V sent as the Kitty keyboard
// protocol escape sequence is detected just like the legacy 0x16 byte.
func TestWriteWithPaste_KittyEncoding(t *testing.T) {
	var out bytes.Buffer
	png := []byte("\x89PNGdata")
	cb := &fakeClip{has: true, png: png}
	tr := &fakeUploadTransport{}
	in := append([]byte("a"), append([]byte("\x1b[118;5u"), 'b')...)
	writeWithPaste(context.Background(), in, &out, cb, tr, discardLog)
	want := "a" + expectedRemotePath(png) + "b"
	if out.String() != want {
		t.Errorf("kitty encoding: got %q, want %q", out.String(), want)
	}
}

// TestWriteWithPaste_ModifyOtherKeys: the xterm modifyOtherKeys form is also
// detected.
func TestWriteWithPaste_ModifyOtherKeys(t *testing.T) {
	var out bytes.Buffer
	png := []byte("\x89PNGdata")
	cb := &fakeClip{has: true, png: png}
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte("\x1b[27;5;118~"), &out, cb, tr, discardLog)
	want := expectedRemotePath(png)
	if out.String() != want {
		t.Errorf("modifyOtherKeys: got %q, want %q", out.String(), want)
	}
}

// TestWriteWithPaste_KittyNoImage: the escape sequence passes through
// UNCHANGED when there's no image, so the remote app still sees a real Ctrl+V.
func TestWriteWithPaste_KittyNoImage(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: false}
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte("\x1b[118;5u"), &out, cb, tr, discardLog)
	if out.String() != "\x1b[118;5u" {
		t.Errorf("no-image kitty passthrough: got %q, want the escape seq verbatim", out.String())
	}
}

// TestTrailingCtrlVPrefixLen: a chunk ending mid-escape-sequence holds the
// partial token back so a split-across-reads Ctrl+V is still matched.
func TestTrailingCtrlVPrefixLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 0},            // no prefix
		{"abc\x1b", 1},        // ESC alone — could start a token
		{"x\x1b[118;5", 7},    // full token minus final 'u'
		{"\x1b[118;5u", 0},    // complete token — nothing to hold
		{"hello", 0},          // ordinary text
		{"\x1b[27;5;118", 10}, // modifyOtherKeys minus final '~'
	}
	for _, c := range cases {
		if got := trailingCtrlVPrefixLen([]byte(c.in)); got != c.want {
			t.Errorf("trailingCtrlVPrefixLen(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- proxyStdin orchestration tests -------------------------------------

// slicedReader yields a fixed list of chunks (one per Read) then EOF, so we can
// drive proxyStdin with a deterministic, split-across-reads byte stream.
type slicedReader struct {
	chunks [][]byte
	i      int
	gate   chan struct{} // if non-nil, block before each chunk until signaled
}

func (r *slicedReader) Read(p []byte) (int, error) {
	if r.gate != nil {
		<-r.gate
	}
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// runProxy drives proxyStdin to completion against the given chunks and returns
// everything written to the (fake) ptmx. interactive selects the TTY path.
func runProxy(t *testing.T, interactive bool, cb *fakeClip, tr *fakeUploadTransport, chunks ...[]byte) string {
	t.Helper()
	var out syncBuffer
	in := &slicedReader{chunks: chunks}
	done := make(chan struct{})
	go func() {
		proxyStdin(context.Background(), in, &out, cb, tr, discardLog, interactive)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyStdin did not return")
	}
	return out.String()
}

// syncBuffer is a goroutine-safe bytes.Buffer (proxyStdin writes from its writer
// goroutine while the test may read after it returns).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestProxyStdin_SplitToken: a Ctrl+V escape token delivered in TWO reads is
// stitched back together and triggers exactly one path injection.
func TestProxyStdin_SplitToken(t *testing.T) {
	png := []byte("\x89PNG split")
	cb := &fakeClip{has: true, png: png}
	tr := &fakeUploadTransport{}
	// "\x1b[118;5u" split after the 4th byte: "\x1b[11" + "8;5u".
	got := runProxy(t, true, cb, tr, []byte("a\x1b[11"), []byte("8;5ub"))
	want := "a" + expectedRemotePath(png) + "b"
	if got != want {
		t.Errorf("split token: got %q, want %q", got, want)
	}
	if c := strings.Count(got, expectedRemotePath(png)); c != 1 {
		t.Errorf("expected path injected exactly once, got %d", c)
	}
}

// TestProxyStdin_LoneESCFlushed: a lone trailing ESC (a prefix of every escape
// token) must NOT be held indefinitely — the idle flush forwards it promptly so
// pressing Escape isn't dead (F5). We deliver just an ESC and then EOF and
// assert it reaches the ptmx.
func TestProxyStdin_LoneESCFlushed(t *testing.T) {
	cb := &fakeClip{has: false}
	tr := &fakeUploadTransport{}
	// EOF arrives right after, so flush-on-EOF also covers it; but the timing
	// test below proves the idle path. Here we just assert correctness.
	got := runProxy(t, true, cb, tr, []byte("\x1b"))
	if got != "\x1b" {
		t.Errorf("lone ESC: got %q, want a single ESC byte", got)
	}
}

// TestProxyStdin_LoneESCIdleFlush: with the input goroutine kept open (no EOF),
// a lone trailing ESC is flushed by the idle timer within a bounded time rather
// than held until the next keystroke (F5).
func TestProxyStdin_LoneESCIdleFlush(t *testing.T) {
	cb := &fakeClip{has: false}
	tr := &fakeUploadTransport{}
	var out syncBuffer
	// gate lets us deliver the ESC chunk, then withhold the next read (and EOF)
	// so only the idle timer can flush the held ESC.
	gate := make(chan struct{}, 2)
	in := &slicedReader{chunks: [][]byte{{0x1b}, nil}, gate: gate}
	go proxyStdin(context.Background(), in, &out, cb, tr, discardLog, true)
	gate <- struct{}{} // release the ESC read; then withhold the rest
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if out.String() == "\x1b" {
			return // flushed promptly by the idle timer — good
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("lone ESC was not idle-flushed; got %q", out.String())
}

// TestProxyStdin_CtrlVInBracketedPaste: a 0x16 byte inside a bracketed-paste
// region (ESC[200~ ... ESC[201~) is a literal pasted byte and must pass through
// VERBATIM, not be treated as Ctrl+V (F8).
func TestProxyStdin_CtrlVInBracketedPaste(t *testing.T) {
	cb := &fakeClip{has: true, png: []byte("should-not-upload")}
	tr := &fakeUploadTransport{}
	payload := "\x1b[200~before\x16after\x1b[201~"
	got := runProxy(t, true, cb, tr, []byte(payload))
	if got != payload {
		t.Errorf("bracketed paste: got %q, want verbatim %q", got, payload)
	}
	if len(tr.stdin()) > 0 {
		t.Errorf("no upload should happen for a 0x16 inside a bracketed paste")
	}
}

// TestProxyStdin_SplitBracketedPaste: the ESC[200~ start marker split across
// two reads is still recognized, and a 0x16 inside the region passes through
// verbatim (F8) — proving bracketed-paste tracking survives read boundaries.
func TestProxyStdin_SplitBracketedPaste(t *testing.T) {
	cb := &fakeClip{has: true, png: []byte("should-not-upload")}
	tr := &fakeUploadTransport{}
	// "\x1b[200~" split after "\x1b[20"; payload has a literal 0x16.
	got := runProxy(t, true, cb, tr, []byte("a\x1b[20"), []byte("0~x\x16y\x1b[201~b"))
	want := "a\x1b[200~x\x16y\x1b[201~b"
	if got != want {
		t.Errorf("split bracketed paste: got %q, want %q", got, want)
	}
	if len(tr.stdin()) > 0 {
		t.Errorf("no upload should happen for a 0x16 inside a bracketed paste")
	}
}

// TestProxyStdin_MultipleCtrlV: two Ctrl+V tokens in one chunk each inject the
// path, with the surrounding bytes preserved in order.
func TestProxyStdin_MultipleCtrlV(t *testing.T) {
	png := []byte("\x89PNG multi")
	cb := &fakeClip{has: true, png: png}
	tr := &fakeUploadTransport{}
	chunk := append([]byte{'a', ctrlV, 'b'}, append([]byte("\x1b[118;5u"), 'c')...)
	got := runProxy(t, true, cb, tr, chunk)
	p := expectedRemotePath(png)
	want := "a" + p + "b" + p + "c"
	if got != want {
		t.Errorf("multiple Ctrl+V: got %q, want %q", got, want)
	}
}

// TestProxyStdin_NonInteractiveVerbatim: when stdin is NOT an interactive TTY,
// a bare 0x16 in (possibly binary) input is forwarded verbatim and never treated
// as a paste trigger, so piped binary input is not corrupted (F8).
func TestProxyStdin_NonInteractiveVerbatim(t *testing.T) {
	cb := &fakeClip{has: true, png: []byte("should-not-upload")}
	tr := &fakeUploadTransport{}
	data := []byte{0x00, ctrlV, 0x01, 0x1b, '[', '1', '1', '8', ';', '5', 'u', 0xff}
	got := runProxy(t, false, cb, tr, data)
	if got != string(data) {
		t.Errorf("non-interactive: got %q, want verbatim %q", got, string(data))
	}
	if len(tr.stdin()) > 0 {
		t.Errorf("no upload should happen on non-interactive stdin")
	}
}

// TestProxyStdin_CtrlCCancelsUpload: a 0x03 (Ctrl+C) arriving while an upload is
// in flight cancels the upload context so the session is not wedged (F6c). The
// fake upload blocks until its ctx is cancelled; the test sends Ctrl+V then
// Ctrl+C and asserts the proxy returns promptly (well under the upload delay).
func TestProxyStdin_CtrlCCancelsUpload(t *testing.T) {
	cb := &fakeClip{has: true, png: []byte("\x89PNG slow")}
	// A long delay that only ctx-cancellation can cut short.
	tr := &fakeUploadTransport{delay: 30 * time.Second}
	var out syncBuffer
	gate := make(chan struct{}, 4)
	in := &slicedReader{chunks: [][]byte{{ctrlV}, {ctrlC}}, gate: gate}
	done := make(chan struct{})
	go func() {
		proxyStdin(context.Background(), in, &out, cb, tr, discardLog, true)
		close(done)
	}()
	gate <- struct{}{} // deliver Ctrl+V (kicks the blocking upload on the worker)
	time.Sleep(50 * time.Millisecond)
	gate <- struct{}{} // deliver Ctrl+C — must cancel the in-flight upload

	// The cancelled upload fails, so handlePaste rings the bell. Wait for it to
	// appear WELL under the 30s upload delay — that proves Ctrl+C cancelled the
	// in-flight upload rather than the proxy merely waiting it out. We have NOT
	// released EOF yet, so only the Ctrl+C cancellation can unblock the worker.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "\x07") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "\x07") {
		t.Fatal("Ctrl+C did not cancel the in-flight upload; no failure bell")
	}

	gate <- struct{}{} // release the final read → EOF, let the proxy exit
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyStdin did not return after EOF")
	}
}
