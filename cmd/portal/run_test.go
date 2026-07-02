package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/VikashLoomba/Portal/internal/agentclient"
	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/clip"
	"github.com/VikashLoomba/Portal/internal/clipupload"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/forward"
	"github.com/VikashLoomba/Portal/internal/protocol"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// errFake is an injectable read failure so a test can drive the
// "image enabled but coercion fails -> fall through to text" branch.
var errFake = fmt.Errorf("fake clipboard read error")

// fakeClipboard is a scriptable clip.Clipboard for serveClipRequest tests. It
// lets a table row dictate exactly what the Mac clipboard reports (image
// present / text present / concealed) and what bytes the image/text reads
// yield, plus an injectable read error so we can exercise the
// "enabled-but-upload-fails falls through to text" path.
type fakeClipboard struct {
	hasImage  bool
	hasText   bool
	concealed bool
	imagePNG  []byte
	text      []byte
	imageErr  error
	textErr   error
}

func (f *fakeClipboard) HasImage() bool    { return f.hasImage }
func (f *fakeClipboard) HasText() bool     { return f.hasText }
func (f *fakeClipboard) IsConcealed() bool { return f.concealed }
func (f *fakeClipboard) Describe() string  { return "fake" }
func (f *fakeClipboard) ImagePNG(context.Context) ([]byte, error) {
	if f.imageErr != nil {
		return nil, f.imageErr
	}
	return f.imagePNG, nil
}
func (f *fakeClipboard) Text(context.Context) ([]byte, error) {
	if f.textErr != nil {
		return nil, f.textErr
	}
	return f.text, nil
}

var _ clip.Clipboard = (*fakeClipboard)(nil)

// uploadFakeTransport is the minimal transport.Transport that clipupload.Upload*
// needs: Exec (with byte stdin) must echo back a path that validateRemotePath
// accepts ("/<abs>/<name>"). It derives the expected basename from the script
// (which ends with the content-addressed name) so any sha round-trips cleanly.
type uploadFakeTransport struct{}

func (uploadFakeTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (uploadFakeTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1, Detail: "pid=1"}, nil
}
func (uploadFakeTransport) Close(context.Context) (bool, error) { return true, nil }
func (uploadFakeTransport) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: "fakehost", Endpoint: "/tmp/sock-fake"}
}
func (uploadFakeTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}
func (uploadFakeTransport) Exec(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	if len(stdin) == 0 {
		// Non-upload probe path (nil stdin) — no canned reply needed.
		return "", "", nil
	}
	// The upload script ends with: printf '%s' "$HOME/.cache/portal/clip/<name>"
	// We can't run shell, so synthesize a path ending in the basename the
	// script embeds. Extract the trailing /clip/<name>" token from the joined
	// argv and return an absolute path with that basename.
	joined := strings.Join(argv, " ")
	const marker = "/.cache/portal/clip/"
	i := strings.LastIndex(joined, marker)
	if i < 0 {
		return "", "", fmt.Errorf("uploadFakeTransport: no clip path in script")
	}
	rest := joined[i+len(marker):]
	// rest starts with "<name>"... — strip at the first quote/space.
	name := rest
	if j := strings.IndexAny(name, "\"' "); j >= 0 {
		name = name[:j]
	}
	return "/home/u/.cache/portal/clip/" + name, "", nil
}

var _ transport.Transport = (*uploadFakeTransport)(nil)

// newTestApp builds an App with a fake transport + audit sink rooted at a temp
// dir so feature-toggle files can be written per test.
func newTestApp(t *testing.T) *app.App {
	t.Helper()
	dir := t.TempDir()
	return &app.App{
		Cfg:       config.New(dir),
		Log:       forward.StdoutLogger(),
		Audit:     audit.New(dir),
		Transport: uploadFakeTransport{},
	}
}

// TestEnsureForwardedForURL_ForwardsUnderNativeHealth proves the run
// auto-forward path (EC10b): under a healthy transport reporting Pid==0
// (native-shaped Health) it still forwards a missing localhost port, and the
// Forward call routes through App.PF (a transport.PortForwarder) — NOT
// App.Transport, which no longer has forwarding methods. The recording
// forwarder starts with no current forwards, so the URL's port must be added.
func TestEnsureForwardedForURL_ForwardsUnderNativeHealth(t *testing.T) {
	dir := t.TempDir()
	pf := &recordingForwarder{current: nil}
	a := &app.App{
		Cfg:       config.New(dir),
		Log:       forward.StdoutLogger(),
		Audit:     audit.New(dir),
		Transport: nativeHealthTransport{up: true, pid: 0},
		PF:        pf,
	}

	ensureForwardedForURL(context.Background(), "http://localhost:39041/callback", a)

	if len(pf.forwarded) != 1 || pf.forwarded[0] != [2]int{39041, 39041} {
		t.Fatalf("Forward calls = %v, want [[39041 39041]] (routed through App.PF under Pid==0 Health)", pf.forwarded)
	}
}

// TestEnsureForwardedForURL_SkipsWhenMasterDown proves the gate short-circuits
// when Health.Up is false: no Forward is attempted.
func TestEnsureForwardedForURL_SkipsWhenMasterDown(t *testing.T) {
	dir := t.TempDir()
	pf := &recordingForwarder{}
	a := &app.App{
		Cfg:       config.New(dir),
		Log:       forward.StdoutLogger(),
		Audit:     audit.New(dir),
		Transport: nativeHealthTransport{up: false},
		PF:        pf,
	}

	ensureForwardedForURL(context.Background(), "http://localhost:39041/callback", a)

	if len(pf.forwarded) != 0 {
		t.Fatalf("Forward calls = %v, want none when master down", pf.forwarded)
	}
}

// serveOnce runs serveClipRequest against a fresh probe cache so each call is
// independent (no cross-test cache bleed).
func serveOnce(a *app.App, cb clip.Clipboard, kind, format string) *protocol.ClipResponse {
	probe := map[string]clipEntry{}
	var mu sync.Mutex
	return serveClipRequest(context.Background(), a, cb, probe, &mu,
		&agentclient.ClipEvent{Nonce: 1, Epoch: 2, Kind: kind, Format: format})
}

func TestServeClipRequest_Targets(t *testing.T) {
	pngMagic := append([]byte("\x89PNG\r\n\x1a\n"), []byte("data")...)
	tests := []struct {
		name         string
		cb           *fakeClipboard
		imageEnabled bool
		textEnabled  bool
		wantOK       bool
		wantHas      bool
		wantKind     string
	}{
		{
			name:         "image present, image enabled -> serves image",
			cb:           &fakeClipboard{hasImage: true, imagePNG: pngMagic},
			imageEnabled: true, textEnabled: true,
			wantOK: true, wantHas: true, wantKind: "image",
		},
		{
			// The regression: image+text both present, image DISABLED, text
			// enabled. Must fall through and serve text, not hide it.
			name:         "image+text, image disabled text enabled -> serves text",
			cb:           &fakeClipboard{hasImage: true, imagePNG: pngMagic, hasText: true, text: []byte("hello")},
			imageEnabled: false, textEnabled: true,
			wantOK: true, wantHas: true, wantKind: "text",
		},
		{
			name:         "image present, image disabled, no text -> none",
			cb:           &fakeClipboard{hasImage: true, imagePNG: pngMagic},
			imageEnabled: false, textEnabled: true,
			wantOK: true, wantHas: false,
		},
		{
			// Image enabled but the coercion fails -> fall through to text.
			name:         "image upload fails -> falls through to text",
			cb:           &fakeClipboard{hasImage: true, imageErr: errFake, hasText: true, text: []byte("hi")},
			imageEnabled: true, textEnabled: true,
			wantOK: true, wantHas: true, wantKind: "text",
		},
		{
			name:         "text present, text disabled -> none",
			cb:           &fakeClipboard{hasText: true, text: []byte("secret")},
			imageEnabled: true, textEnabled: false,
			wantOK: true, wantHas: false,
		},
		{
			name:         "text present but concealed -> none",
			cb:           &fakeClipboard{hasText: true, text: []byte("pw"), concealed: true},
			imageEnabled: true, textEnabled: true,
			wantOK: true, wantHas: false,
		},
		{
			name:         "text too large -> none (source-side cap)",
			cb:           &fakeClipboard{hasText: true, text: make([]byte, clipupload.MaxUploadBytes+1)},
			imageEnabled: true, textEnabled: true,
			wantOK: true, wantHas: false,
		},
		{
			name:         "empty clipboard -> none",
			cb:           &fakeClipboard{},
			imageEnabled: true, textEnabled: true,
			wantOK: true, wantHas: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			mustSetFeature(t, a, config.FeatureClipImage, tc.imageEnabled)
			mustSetFeature(t, a, config.FeatureClipText, tc.textEnabled)
			resp := serveOnce(a, tc.cb, "targets", "")
			if resp.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", resp.OK, tc.wantOK)
			}
			if resp.Has != tc.wantHas {
				t.Errorf("Has = %v, want %v", resp.Has, tc.wantHas)
			}
			if resp.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", resp.Kind, tc.wantKind)
			}
			if tc.wantHas && resp.SHA == "" {
				t.Errorf("expected non-empty SHA when Has=true")
			}
		})
	}
}

func TestServeClipRequest_Image(t *testing.T) {
	pngMagic := append([]byte("\x89PNG\r\n\x1a\n"), []byte("data")...)
	tests := []struct {
		name    string
		cb      *fakeClipboard
		format  string
		enabled bool
		wantOK  bool
	}{
		{"png image enabled", &fakeClipboard{hasImage: true, imagePNG: pngMagic}, "png", true, true},
		{"non-png format rejected", &fakeClipboard{hasImage: true, imagePNG: pngMagic}, "bmp", true, false},
		{"image disabled", &fakeClipboard{hasImage: true, imagePNG: pngMagic}, "png", false, false},
		{"no image present", &fakeClipboard{}, "png", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			mustSetFeature(t, a, config.FeatureClipImage, tc.enabled)
			mustSetFeature(t, a, config.FeatureClipText, true)
			resp := serveOnce(a, tc.cb, "image", tc.format)
			if resp.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", resp.OK, tc.wantOK)
			}
		})
	}
}

func TestServeClipRequest_Text(t *testing.T) {
	tests := []struct {
		name    string
		cb      *fakeClipboard
		enabled bool
		wantOK  bool
	}{
		{"text enabled", &fakeClipboard{hasText: true, text: []byte("hello")}, true, true},
		{"text disabled", &fakeClipboard{hasText: true, text: []byte("hello")}, false, false},
		{"text concealed", &fakeClipboard{hasText: true, text: []byte("pw"), concealed: true}, true, false},
		{"no text present", &fakeClipboard{}, true, false},
		{"text too large", &fakeClipboard{hasText: true, text: make([]byte, clipupload.MaxUploadBytes+1)}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			mustSetFeature(t, a, config.FeatureClipImage, true)
			mustSetFeature(t, a, config.FeatureClipText, tc.enabled)
			resp := serveOnce(a, tc.cb, "text", "")
			if resp.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", resp.OK, tc.wantOK)
			}
		})
	}
}

// TestServeClipRequest_TextGateOnCachedSHA verifies the SECURITY-CRITICAL
// invariant from run.go: the text gate is checked BEFORE the probe cache
// lookup, so a SHA cached by an earlier `targets` probe is NOT leaked once the
// user disables the feature (or the clipboard becomes concealed). We populate
// the cache via a targets probe with text enabled, then disable text and assert
// the follow-up `text` fetch refuses despite the warm cache.
func TestServeClipRequest_TextGateOnCachedSHA(t *testing.T) {
	a := newTestApp(t)
	mustSetFeature(t, a, config.FeatureClipImage, true)
	mustSetFeature(t, a, config.FeatureClipText, true)
	cb := &fakeClipboard{hasText: true, text: []byte("cached-secret")}

	probe := map[string]clipEntry{}
	var mu sync.Mutex
	// Warm the cache via a targets probe.
	tResp := serveClipRequest(context.Background(), a, cb, probe, &mu,
		&agentclient.ClipEvent{Nonce: 1, Kind: "targets"})
	if !tResp.OK || !tResp.Has || tResp.Kind != "text" {
		t.Fatalf("targets probe did not cache text: %+v", tResp)
	}
	// Now disable text and re-fetch with the SAME cache: must refuse.
	mustSetFeature(t, a, config.FeatureClipText, false)
	fResp := serveClipRequest(context.Background(), a, cb, probe, &mu,
		&agentclient.ClipEvent{Nonce: 2, Kind: "text"})
	if fResp.OK {
		t.Errorf("text fetch returned OK after disable; cached SHA leaked")
	}
}

func mustSetFeature(t *testing.T, a *app.App, feature string, on bool) {
	t.Helper()
	if err := a.Cfg.SetFeature(feature, on); err != nil {
		t.Fatalf("SetFeature(%s,%v): %v", feature, on, err)
	}
}
