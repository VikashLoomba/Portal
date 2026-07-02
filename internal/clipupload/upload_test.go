package clipupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// fakeTransport returns canned stdout/stderr/err from Exec so we can drive
// Upload's validation paths.
type fakeTransport struct {
	gotStdin []byte
	gotArgv  []string
	stdout   string
	stderr   string
	err      error
}

func (f *fakeTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (f *fakeTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}
func (f *fakeTransport) Exec(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	f.gotStdin = append([]byte(nil), stdin...)
	f.gotArgv = argv
	return f.stdout, f.stderr, f.err
}
func (f *fakeTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, func() error { return nil }, nil
}
func (f *fakeTransport) Close(context.Context) (bool, error) { return false, nil }
func (f *fakeTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/fake-sock"}
}

var _ transport.Transport = (*fakeTransport)(nil)

// expectedName mirrors the content-addressed basename Upload computes.
func expectedName(t *testing.T, png []byte) string {
	t.Helper()
	sum := sha256.Sum256(png)
	return "clip-" + hex.EncodeToString(sum[:])[:32] + ".png"
}

// TestValidateRemotePath_Accepts: a well-formed absolute path ending in the
// expected basename is returned verbatim.
func TestValidateRemotePath_Accepts(t *testing.T) {
	const name = "clip-0123456789abcdef0123456789abcdef.png"
	in := "/home/u/.cache/portal/clip/" + name
	got, err := validateRemotePath(in+"\n", name) // trailing newline is outer whitespace
	if err != nil {
		t.Fatalf("expected accept, got error: %v", err)
	}
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

// TestValidateRemotePath_Rejects: every injection shape must be rejected with
// NO path returned.
func TestValidateRemotePath_Rejects(t *testing.T) {
	const name = "clip-0123456789abcdef0123456789abcdef.png"
	good := "/home/u/.cache/portal/clip/" + name
	cases := []struct {
		desc string
		in   string
	}{
		// An rc file / PROMPT_COMMAND prepending a line, then the path. The
		// embedded newline (the auto-submit vector) must be rejected even
		// though TrimSpace leaves it intact.
		{"embedded newline / injected leading line", "echo pwned\n" + good},
		// Injected text after the path on its own line.
		{"injected trailing line", good + "\nrm -rf ~"},
		// Leading garbage on the same line as the path.
		{"leading garbage", "garbage" + good},
		// A bare control byte (e.g. CR) inside the path.
		{"control byte (CR)", "/home/u/clip\r" + name},
		// Not absolute.
		{"not absolute", "home/u/.cache/portal/clip/" + name},
		// Wrong basename (different hash).
		{"wrong basename", "/home/u/.cache/portal/clip/clip-ffffffffffffffffffffffffffffffff.png"},
		// Right name but as a prefix without the "/" separator, so it does not
		// end in "/" + name.
		{"name without slash separator", "/home/u/x" + name},
		// Empty after trim.
		{"empty", "   \n  "},
	}
	for _, c := range cases {
		got, err := validateRemotePath(c.in, name)
		if err == nil {
			t.Errorf("%s: expected error, got path %q", c.desc, got)
		}
		if got != "" {
			t.Errorf("%s: expected no path on rejection, got %q", c.desc, got)
		}
	}
}

// TestUpload_RejectsInjectedStdout: Upload as a whole must not return an
// injected path even when the remote stdout is hostile.
func TestUpload_RejectsInjectedStdout(t *testing.T) {
	png := []byte("\x89PNG fake")
	name := expectedName(t, png)
	good := "/home/u/.cache/portal/clip/" + name

	tr := &fakeTransport{stdout: "evil prelude\n" + good}
	got, err := Upload(context.Background(), tr, png)
	if err == nil {
		t.Fatalf("expected Upload to reject injected stdout, got %q", got)
	}
	if got != "" {
		t.Errorf("expected no path on rejection, got %q", got)
	}
}

// TestUpload_AcceptsValidStdout: Upload returns the path when the remote
// echoes back exactly the expected absolute path.
func TestUpload_AcceptsValidStdout(t *testing.T) {
	png := []byte("\x89PNG fake")
	name := expectedName(t, png)
	want := "/home/u/.cache/portal/clip/" + name

	tr := &fakeTransport{stdout: want}
	got, err := Upload(context.Background(), tr, png)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The PNG bytes are streamed as stdin.
	if string(tr.gotStdin) != string(png) {
		t.Errorf("stdin: got %q, want %q", tr.gotStdin, png)
	}
	// Remote shell is invoked non-interactively (F12).
	wantArgv := []string{"bash", "--noprofile", "--norc", "-c"}
	if len(tr.gotArgv) != 5 {
		t.Fatalf("argv: got %v, want bash --noprofile --norc -c <script>", tr.gotArgv)
	}
	for i, a := range wantArgv {
		if tr.gotArgv[i] != a {
			t.Errorf("argv[%d]: got %q, want %q", i, tr.gotArgv[i], a)
		}
	}
	// The script must not use `eval` (F11).
	if strings.Contains(tr.gotArgv[4], "eval") {
		t.Errorf("script still uses eval: %q", tr.gotArgv[4])
	}
}

// TestUpload_EmptyPNG: empty input is rejected before any remote call.
func TestUpload_EmptyPNG(t *testing.T) {
	tr := &fakeTransport{stdout: "/should/not/matter"}
	got, err := Upload(context.Background(), tr, nil)
	if err == nil {
		t.Fatalf("expected error on empty pngData, got %q", got)
	}
	if tr.gotStdin != nil {
		t.Errorf("no remote call should happen for empty input")
	}
}

// TestUpload_FoldsStderr: on remote failure, stderr is folded into the error
// (F12) rather than being silently discarded.
func TestUpload_FoldsStderr(t *testing.T) {
	tr := &fakeTransport{err: context.DeadlineExceeded, stderr: "bash: install: not found"}
	_, err := Upload(context.Background(), tr, []byte("data"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bash: install: not found") {
		t.Errorf("stderr not folded into error: %v", err)
	}
}

// TestContentAddressedName: the basename is stable for identical bytes and
// differs for different bytes, and is widened to 32 hex chars (128 bits).
func TestContentAddressedName(t *testing.T) {
	a1 := expectedName(t, []byte("same"))
	a2 := expectedName(t, []byte("same"))
	b := expectedName(t, []byte("different"))
	if a1 != a2 {
		t.Errorf("same bytes must yield same name: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("different bytes must yield different name: both %q", a1)
	}
	// "clip-" + 32 hex + ".png" = 5 + 32 + 4 = 41
	if len(a1) != len("clip-")+32+len(".png") {
		t.Errorf("name width unexpected: %q (len %d)", a1, len(a1))
	}
	hexPart := strings.TrimSuffix(strings.TrimPrefix(a1, "clip-"), ".png")
	if len(hexPart) != 32 {
		t.Errorf("expected 32 hex chars (128 bits), got %d: %q", len(hexPart), hexPart)
	}
}

// TestUpload_ExplicitChmod0600: the upload script must chmod the temp file
// 0600 EXPLICITLY (defense-in-depth, not umask — DESIGN §7.1) before mv.
func TestUpload_ExplicitChmod0600(t *testing.T) {
	png := []byte("\x89PNG fake")
	name := expectedName(t, png)
	tr := &fakeTransport{stdout: "/home/u/.cache/portal/clip/" + name}
	if _, err := Upload(context.Background(), tr, png); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(tr.gotArgv[4], "chmod 0600") {
		t.Errorf("script must chmod 0600 explicitly: %q", tr.gotArgv[4])
	}
}

// TestUpload_SizeCap: a payload over MaxUploadBytes is rejected before any
// remote call (DESIGN §3 — protect heartbeats from a multi-MB upload).
func TestUpload_SizeCap(t *testing.T) {
	tr := &fakeTransport{stdout: "/should/not/matter"}
	big := make([]byte, MaxUploadBytes+1)
	if _, err := Upload(context.Background(), tr, big); err == nil {
		t.Fatalf("expected size-cap rejection")
	}
	if tr.gotStdin != nil {
		t.Errorf("no remote call should happen for oversize input")
	}
}

// TestShortSHA: stable 32-hex content address; differs for different bytes.
func TestShortSHA(t *testing.T) {
	a := ShortSHA([]byte("same"))
	b := ShortSHA([]byte("same"))
	c := ShortSHA([]byte("different"))
	if a != b {
		t.Errorf("same bytes must yield same sha")
	}
	if a == c {
		t.Errorf("different bytes must yield different sha")
	}
	if len(a) != 32 {
		t.Errorf("expected 32 hex chars, got %d: %q", len(a), a)
	}
}

// TestUploadImage_ReturnsSHA: UploadImage returns the same short sha that names
// the file, so the Mac can put it straight into a ClipResponse.
func TestUploadImage_ReturnsSHA(t *testing.T) {
	png := []byte("\x89PNG fake")
	name := expectedName(t, png)
	tr := &fakeTransport{stdout: "/home/u/.cache/portal/clip/" + name}
	path, sha, err := UploadImage(context.Background(), tr, png)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != ShortSHA(png) {
		t.Errorf("sha = %q, want %q", sha, ShortSHA(png))
	}
	if !strings.HasSuffix(path, "/"+name) {
		t.Errorf("path %q does not end in /%s", path, name)
	}
}

// TestUploadText: writes text-<sha>.txt, returns the sha, and the script's
// basename matches the text- prefix.
func TestUploadText(t *testing.T) {
	body := []byte("hello text")
	sha := ShortSHA(body)
	name := "text-" + sha + ".txt"
	tr := &fakeTransport{stdout: "/home/u/.cache/portal/clip/" + name}
	path, gotSHA, err := UploadText(context.Background(), tr, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotSHA != sha {
		t.Errorf("sha = %q, want %q", gotSHA, sha)
	}
	if !strings.HasSuffix(path, "/"+name) {
		t.Errorf("path %q does not end in /%s", path, name)
	}
	if string(tr.gotStdin) != string(body) {
		t.Errorf("stdin: got %q, want %q", tr.gotStdin, body)
	}
	// Empty text is rejected before any remote call.
	tr2 := &fakeTransport{}
	if _, _, err := UploadText(context.Background(), tr2, nil); err == nil {
		t.Fatalf("expected empty-text rejection")
	}
}

// TestShellQuote: single quotes are escaped so the script survives ssh's
// sh -c re-parsing.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc", "'abc'"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"'", `''\'''`},
		{"a'b'c", `'a'\''b'\''c'`},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
