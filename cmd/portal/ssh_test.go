package main

import (
	"bytes"
	"context"
	"io"
	"testing"

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

// fakeUploadTransport records ExecBytes and returns a canned remote path.
type fakeUploadTransport struct {
	gotStdin   []byte
	remotePath string
}

func (f *fakeUploadTransport) Host() string                                     { return "h" }
func (f *fakeUploadTransport) Sock() string                                     { return "/tmp/s" }
func (f *fakeUploadTransport) MasterPID(context.Context) (int, error)           { return 1, nil }
func (f *fakeUploadTransport) EnsureMaster(context.Context) (int, bool, error)  { return 1, false, nil }
func (f *fakeUploadTransport) Forward(context.Context, int, int) error          { return nil }
func (f *fakeUploadTransport) Cancel(context.Context, int, int) error           { return nil }
func (f *fakeUploadTransport) Exit(context.Context) (bool, error)               { return false, nil }
func (f *fakeUploadTransport) Exec(context.Context, string, ...string) (string, error) {
	return "", nil
}
func (f *fakeUploadTransport) ExecBytes(_ context.Context, stdin []byte, _ ...string) (string, string, error) {
	f.gotStdin = append([]byte(nil), stdin...)
	return f.remotePath, "", nil
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
	writeWithPaste(context.Background(), []byte{'a', ctrlV, 'b'}, &out, cb, tr)
	if !bytes.Equal(out.Bytes(), []byte{'a', ctrlV, 'b'}) {
		t.Errorf("no-image passthrough: got %v, want [a 0x16 b]", out.Bytes())
	}
	if tr.gotStdin != nil {
		t.Errorf("no upload should have happened")
	}
}

// TestWriteWithPaste_WithImage: Ctrl+V is swallowed and replaced by the
// uploaded remote path; surrounding bytes are preserved.
func TestWriteWithPaste_WithImage(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: true, png: []byte("\x89PNG fake")}
	tr := &fakeUploadTransport{remotePath: "/home/u/.cache/portal/clip/clip-abc123.png"}
	writeWithPaste(context.Background(), []byte{'x', ctrlV, 'y'}, &out, cb, tr)

	want := "x/home/u/.cache/portal/clip/clip-abc123.pngy"
	if out.String() != want {
		t.Errorf("with-image: got %q, want %q", out.String(), want)
	}
	if !bytes.Equal(tr.gotStdin, []byte("\x89PNG fake")) {
		t.Errorf("upload stdin: got %q, want the PNG bytes", tr.gotStdin)
	}
}

// TestWriteWithPaste_NoCtrlV: ordinary input is forwarded verbatim.
func TestWriteWithPaste_NoCtrlV(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: true} // even if clipboard has image, no Ctrl+V = no action
	tr := &fakeUploadTransport{remotePath: "/should/not/appear"}
	writeWithPaste(context.Background(), []byte("hello world"), &out, cb, tr)
	if out.String() != "hello world" {
		t.Errorf("got %q, want %q", out.String(), "hello world")
	}
	if tr.gotStdin != nil {
		t.Errorf("no upload should have happened")
	}
}

// TestWriteWithPaste_UploadError: on upload failure a bell is emitted and
// no path is injected.
func TestWriteWithPaste_UploadError(t *testing.T) {
	var out bytes.Buffer
	cb := &fakeClip{has: true, err: context.DeadlineExceeded}
	tr := &fakeUploadTransport{}
	writeWithPaste(context.Background(), []byte{ctrlV}, &out, cb, tr)
	if out.String() != "\x07" {
		t.Errorf("expected bell on error, got %q", out.String())
	}
}
