package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/clip"
	"github.com/VikashLoomba/Portal/internal/clipupload"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

type clipWriteOrder struct {
	mu    sync.Mutex
	steps []string
}

func (o *clipWriteOrder) add(step string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.steps = append(o.steps, step)
	o.mu.Unlock()
}

func (o *clipWriteOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.steps...)
}

type fakeClipWriteCall struct {
	kind string
	data []byte
}

type fakeClipWriter struct {
	mu       sync.Mutex
	calls    []fakeClipWriteCall
	textErr  error
	imageErr error
	clearErr error
	started  chan struct{}
	release  <-chan struct{}
	start    sync.Once
	order    *clipWriteOrder
}

func (f *fakeClipWriter) SetText(_ context.Context, data []byte) error {
	return f.apply("text", data, f.textErr)
}

func (f *fakeClipWriter) SetImagePNG(_ context.Context, data []byte) error {
	return f.apply("png", data, f.imageErr)
}

func (f *fakeClipWriter) Clear(context.Context) error {
	return f.apply("clear", nil, f.clearErr)
}

func (f *fakeClipWriter) apply(kind string, data []byte, err error) error {
	if f.started != nil {
		f.start.Do(func() { close(f.started) })
	}
	if f.release != nil {
		<-f.release
	}
	f.order.add("set")
	f.mu.Lock()
	f.calls = append(f.calls, fakeClipWriteCall{kind: kind, data: append([]byte(nil), data...)})
	f.mu.Unlock()
	return err
}

func (f *fakeClipWriter) snapshot() []fakeClipWriteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeClipWriteCall, len(f.calls))
	for i, call := range f.calls {
		out[i] = fakeClipWriteCall{kind: call.kind, data: append([]byte(nil), call.data...)}
	}
	return out
}

var _ clip.Writer = (*fakeClipWriter)(nil)

type fakeClipWriteTransport struct {
	mu      sync.Mutex
	stdout  string
	stderr  string
	err     error
	argv    [][]string
	stdin   [][]byte
	order   *clipWriteOrder
	started chan struct{}
	start   sync.Once
}

func (f *fakeClipWriteTransport) Ensure(context.Context) (bool, error) { return false, nil }

func (f *fakeClipWriteTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true}, nil
}

func (f *fakeClipWriteTransport) Exec(_ context.Context, stdin []byte, argv ...string) (string, string, error) {
	f.order.add("pull")
	if f.started != nil {
		f.start.Do(func() { close(f.started) })
	}
	f.mu.Lock()
	f.argv = append(f.argv, append([]string(nil), argv...))
	f.stdin = append(f.stdin, append([]byte(nil), stdin...))
	stdout, stderr, err := f.stdout, f.stderr, f.err
	f.mu.Unlock()
	return stdout, stderr, err
}

func (f *fakeClipWriteTransport) Stream(context.Context, ...string) (
	io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error,
) {
	return nil, nil, nil, nil, nil
}

func (f *fakeClipWriteTransport) Close(context.Context) (bool, error) { return false, nil }

func (f *fakeClipWriteTransport) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplSystemSSH, Host: "box"}
}

func (f *fakeClipWriteTransport) calls() ([][]string, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv := make([][]string, len(f.argv))
	for i := range f.argv {
		argv[i] = append([]string(nil), f.argv[i]...)
	}
	stdin := make([][]byte, len(f.stdin))
	for i := range f.stdin {
		stdin[i] = append([]byte(nil), f.stdin[i]...)
	}
	return argv, stdin
}

var _ transport.Transport = (*fakeClipWriteTransport)(nil)

type clipWriteLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *clipWriteLogs) add(line string) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

func (l *clipWriteLogs) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

type clipWriteBannerCall struct {
	title    string
	subtitle string
}

type clipWriteBannerHarness struct {
	mu          sync.Mutex
	calls       []clipWriteBannerCall
	durations   []time.Duration
	callbacks   []func()
	stopCalls   int
	stopResult  bool
	order       *clipWriteOrder
	raised      chan struct{}
	dispatches  int
	timerStarts int
}

func newClipWriteBannerHarness(host string, order *clipWriteOrder) (*clipWriteBanner, *clipWriteBannerHarness) {
	h := &clipWriteBannerHarness{
		stopResult: true,
		order:      order,
		raised:     make(chan struct{}, 32),
	}
	b := &clipWriteBanner{
		host:   host,
		window: clipWriteBannerWindow,
		raise:  h.raise,
		dispatch: func(fn func()) {
			h.mu.Lock()
			h.dispatches++
			h.mu.Unlock()
			fn()
		},
		after: h.after,
	}
	return b, h
}

func (h *clipWriteBannerHarness) raise(title, subtitle string) {
	h.order.add("banner")
	h.mu.Lock()
	h.calls = append(h.calls, clipWriteBannerCall{title: title, subtitle: subtitle})
	h.mu.Unlock()
	select {
	case h.raised <- struct{}{}:
	default:
	}
}

func (h *clipWriteBannerHarness) after(d time.Duration, fn func()) func() bool {
	h.mu.Lock()
	h.durations = append(h.durations, d)
	h.callbacks = append(h.callbacks, fn)
	h.timerStarts++
	h.mu.Unlock()
	return func() bool {
		h.mu.Lock()
		h.stopCalls++
		result := h.stopResult
		h.mu.Unlock()
		return result
	}
}

func (h *clipWriteBannerHarness) snapshot() []clipWriteBannerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]clipWriteBannerCall(nil), h.calls...)
}

func (h *clipWriteBannerHarness) callback(t *testing.T, index int) func() {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if index >= len(h.callbacks) {
		t.Fatalf("banner callback %d missing; have %d", index, len(h.callbacks))
	}
	return h.callbacks[index]
}

func (h *clipWriteBannerHarness) waitRaised(t *testing.T) {
	t.Helper()
	select {
	case <-h.raised:
	case <-time.After(time.Second):
		t.Fatal("clipboard-write banner was not raised")
	}
}

func newClipWriteTestDeps(t *testing.T, data []byte) (
	clipWriteDeps, *fakeClipWriteTransport, *fakeClipWriter, *clipWriteLogs,
	*clipWriteBannerHarness,
) {
	t.Helper()
	tr := &fakeClipWriteTransport{stdout: string(data)}
	writer := &fakeClipWriter{}
	logs := &clipWriteLogs{}
	banner, bannerHarness := newClipWriteBannerHarness("box", nil)
	deps := clipWriteDeps{
		Writer:    writer,
		Transport: tr,
		FeatureEnabled: func(feature string) bool {
			return feature == config.FeatureClipWrite
		},
		Audit:  audit.New(t.TempDir()),
		Host:   "box",
		Banner: banner,
		Log:    logs.add,
	}
	return deps, tr, writer, logs, bannerHarness
}

func clipWriteTextEvent(data []byte) *agentclient.ClipWriteEvent {
	return &agentclient.ClipWriteEvent{
		Nonce: 41, Epoch: 7, Kind: "text",
		SHA: clipupload.ShortSHA(data), Size: int64(len(data)),
	}
}

func assertClipWriteResponse(t *testing.T, got *protocol.ClipWriteResponse,
	nonce, epoch uint64, ok bool, reason string) {

	t.Helper()
	if got == nil {
		t.Fatal("clipboard-write response is nil")
	}
	if got.Nonce != nonce || got.Epoch != epoch || got.OK != ok || got.Err != reason {
		t.Fatalf("clipboard-write response = %+v, want nonce=%d epoch=%d ok=%v err=%q",
			got, nonce, epoch, ok, reason)
	}
}

func assertNoClipWriteAudit(t *testing.T, log *audit.Log) {
	t.Helper()
	data, err := os.ReadFile(log.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("unexpected clipboard-write audit: %q", data)
	}
}

func waitClipWriteHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("clipboard-write handler did not stop")
	}
}

func TestServeClipWriteRequest_Disabled(t *testing.T) {
	data := []byte("disabled")
	deps, tr, writer, _, _ := newClipWriteTestDeps(t, data)
	deps.FeatureEnabled = func(string) bool { return false }
	req := clipWriteTextEvent(data)

	resp := serveClipWriteRequest(context.Background(), deps, req)

	assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, false, "disabled")
	assertCredAudit(t, deps.Audit, []string{
		"clip-write-denied", "host=box", "kind=text", "reason=disabled",
	})
	if argv, _ := tr.calls(); len(argv) != 0 {
		t.Fatal("disabled clipboard write reached transport")
	}
	if len(writer.snapshot()) != 0 {
		t.Fatal("disabled clipboard write reached writer")
	}
}

func TestServeClipWriteRequest_GateReReadPerOperation(t *testing.T) {
	data := []byte("gate-reread")
	deps, tr, writer, _, _ := newClipWriteTestDeps(t, data)
	enabled := false
	checks := 0
	deps.FeatureEnabled = func(feature string) bool {
		if feature != config.FeatureClipWrite {
			t.Fatalf("feature gate read %q, want %q", feature, config.FeatureClipWrite)
		}
		checks++
		return enabled
	}
	req := clipWriteTextEvent(data)

	first := serveClipWriteRequest(context.Background(), deps, req)
	enabled = true
	second := serveClipWriteRequest(context.Background(), deps, req)

	assertClipWriteResponse(t, first, req.Nonce, req.Epoch, false, "disabled")
	assertClipWriteResponse(t, second, req.Nonce, req.Epoch, true, "")
	if checks != 2 {
		t.Fatalf("feature gate checks = %d, want 2", checks)
	}
	if argv, _ := tr.calls(); len(argv) != 1 {
		t.Fatalf("transport calls after gate re-enable = %d, want 1", len(argv))
	}
	if len(writer.snapshot()) != 1 {
		t.Fatalf("writer calls after gate re-enable = %d, want 1", len(writer.snapshot()))
	}
}

func TestServeClipWriteRequest_ShapeRejects(t *testing.T) {
	validSHA := clipupload.ShortSHA([]byte("shape"))
	tests := []struct {
		name   string
		req    agentclient.ClipWriteEvent
		reason string
	}{
		{name: "text zero", req: agentclient.ClipWriteEvent{Kind: "text", SHA: validSHA}, reason: "oversize"},
		{name: "text too large", req: agentclient.ClipWriteEvent{Kind: "text", SHA: validSHA, Size: clipWriteMaxBytes + 1}, reason: "oversize"},
		{name: "image zero", req: agentclient.ClipWriteEvent{Kind: "image", Format: "png", SHA: validSHA}, reason: "oversize"},
		{name: "image too large", req: agentclient.ClipWriteEvent{Kind: "image", Format: "png", SHA: validSHA, Size: clipWriteMaxBytes + 1}, reason: "oversize"},
		{name: "sha short", req: agentclient.ClipWriteEvent{Kind: "text", SHA: "abcd", Size: 4}, reason: "badsha"},
		{name: "sha uppercase", req: agentclient.ClipWriteEvent{Kind: "text", SHA: strings.ToUpper(validSHA), Size: 4}, reason: "badsha"},
		{name: "sha non hex", req: agentclient.ClipWriteEvent{Kind: "text", SHA: strings.Repeat("g", 32), Size: 4}, reason: "badsha"},
		{name: "sha empty", req: agentclient.ClipWriteEvent{Kind: "text", Size: 4}, reason: "badsha"},
		{name: "unknown kind", req: agentclient.ClipWriteEvent{Kind: "bogus", SHA: validSHA, Size: 4}, reason: "badsha"},
		{name: "image jpeg", req: agentclient.ClipWriteEvent{Kind: "image", Format: "jpeg", SHA: validSHA, Size: 4}, reason: "badsha"},
		{name: "image empty format", req: agentclient.ClipWriteEvent{Kind: "image", SHA: validSHA, Size: 4}, reason: "badsha"},
		{name: "text png format", req: agentclient.ClipWriteEvent{Kind: "text", Format: "png", SHA: validSHA, Size: 4}, reason: "badsha"},
		{name: "text txt format", req: agentclient.ClipWriteEvent{Kind: "text", Format: "txt", SHA: validSHA, Size: 4}, reason: "badsha"},
		{name: "clear sha", req: agentclient.ClipWriteEvent{Kind: "clear", SHA: validSHA}, reason: "badsha"},
		{name: "clear size", req: agentclient.ClipWriteEvent{Kind: "clear", Size: 1}, reason: "badsha"},
		{name: "clear oversized", req: agentclient.ClipWriteEvent{Kind: "clear", Size: clipWriteMaxBytes + 1}, reason: "badsha"},
		{name: "clear format", req: agentclient.ClipWriteEvent{Kind: "clear", Format: "png"}, reason: "badsha"},
		{
			name: "format precedence",
			req: agentclient.ClipWriteEvent{
				Kind: "text", Format: "png", SHA: "bad", Size: 0,
			},
			reason: "badsha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, tr, writer, _, _ := newClipWriteTestDeps(t, nil)
			tt.req.Nonce, tt.req.Epoch = 51, 9

			resp := serveClipWriteRequest(context.Background(), deps, &tt.req)

			assertClipWriteResponse(t, resp, 51, 9, false, tt.reason)
			events := credAuditFields(t, deps.Audit)
			if len(events) != 1 || len(events[0]) != 4 || events[0][3] != "reason="+resp.Err {
				t.Fatalf("shape audit = %v, response err = %q", events, resp.Err)
			}
			if argv, _ := tr.calls(); len(argv) != 0 {
				t.Fatal("malformed clipboard write reached transport")
			}
			if len(writer.snapshot()) != 0 {
				t.Fatal("malformed clipboard write reached writer")
			}
		})
	}
}

func TestClipWriteDenyReasonVocabulary(t *testing.T) {
	allowed := map[string]bool{
		"disabled": true, "oversize": true, "badsha": true,
		"shamismatch": true, "inflight": true,
	}
	data := []byte("vocabulary")
	tests := []struct {
		name    string
		req     *agentclient.ClipWriteEvent
		prepare func(*clipWriteDeps, *fakeClipWriteTransport)
	}{
		{
			name: "disabled", req: clipWriteTextEvent(data),
			prepare: func(deps *clipWriteDeps, _ *fakeClipWriteTransport) {
				deps.FeatureEnabled = func(string) bool { return false }
			},
		},
		{
			name: "oversize",
			req: &agentclient.ClipWriteEvent{
				Kind: "text", SHA: clipupload.ShortSHA(data), Size: clipWriteMaxBytes + 1,
			},
		},
		{
			name: "badsha",
			req:  &agentclient.ClipWriteEvent{Kind: "text", SHA: "bad", Size: 3},
		},
		{
			name: "shamismatch", req: clipWriteTextEvent(data),
			prepare: func(_ *clipWriteDeps, tr *fakeClipWriteTransport) {
				tr.stdout = "same-length"
			},
		},
	}

	seen := make(map[string]bool)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, tr, _, _, _ := newClipWriteTestDeps(t, data)
			if tt.prepare != nil {
				tt.prepare(&deps, tr)
			}
			resp := serveClipWriteRequest(context.Background(), deps, tt.req)
			if resp.Err == "" || !allowed[resp.Err] {
				t.Fatalf("response introduced denial reason %q", resp.Err)
			}
			events := credAuditFields(t, deps.Audit)
			if len(events) != 1 || events[0][3] != "reason="+resp.Err {
				t.Fatalf("audit reason diverged from response: %v vs %q", events, resp.Err)
			}
			seen[resp.Err] = true
		})
	}

	deps, _, _, _, _ := newClipWriteTestDeps(t, nil)
	resp := denyClipWriteResponse(deps, &protocol.ClipWriteResponse{}, "text", "inflight")
	if !allowed[resp.Err] {
		t.Fatalf("inflight introduced denial reason %q", resp.Err)
	}
	seen[resp.Err] = true
	if len(seen) != len(allowed) {
		t.Fatalf("denial vocabulary exercised = %v, want %v", seen, allowed)
	}
}

func TestRunClipWriteHandler_ClearHappyPath(t *testing.T) {
	deps, tr, writer, _, banner := newClipWriteTestDeps(t, nil)
	events := make(chan agentclient.EngineEvent, 1)
	responses := make(chan *protocol.ClipWriteResponse, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClipWriteHandlerWithDeps(ctx, events, deps, func(resp *protocol.ClipWriteResponse) error {
			copyResp := *resp
			responses <- &copyResp
			return nil
		}, nil)
	}()

	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: &agentclient.ClipWriteEvent{
		Nonce: 61, Epoch: 10, Kind: "clear",
	}}
	resp := <-responses
	assertClipWriteResponse(t, resp, 61, 10, true, "")
	banner.waitRaised(t)
	cancel()
	waitClipWriteHandler(t, done)

	if argv, _ := tr.calls(); len(argv) != 0 {
		t.Fatal("clear clipboard write pulled remote bytes")
	}
	calls := writer.snapshot()
	if len(calls) != 1 || calls[0].kind != "clear" || len(calls[0].data) != 0 {
		t.Fatalf("clear writer calls = %+v", calls)
	}
	assertCredAudit(t, deps.Audit, []string{
		"clip-written", "host=box", "kind=clear", "cleared",
	})
}

func TestClipWriteAuditKindCanonicalized(t *testing.T) {
	deps, _, _, _, _ := newClipWriteTestDeps(t, nil)
	req := &agentclient.ClipWriteEvent{
		Kind: "text\nfake-line\tkind=clear", SHA: "bad", Size: 1,
	}

	resp := serveClipWriteRequest(context.Background(), deps, req)

	assertClipWriteResponse(t, resp, 0, 0, false, "badsha")
	assertCredAudit(t, deps.Audit, []string{
		"clip-write-denied", "host=box", "kind=unknown", "reason=badsha",
	})
	data, err := os.ReadFile(deps.Audit.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("forged kind created multiple audit lines: %q", data)
	}
}

func TestServeClipWriteRequest_SHAMismatch(t *testing.T) {
	expected := []byte("same")
	tests := []struct {
		name   string
		pulled []byte
	}{
		{name: "digest", pulled: []byte("diff")},
		{name: "length", pulled: []byte("sam")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _, writer, _, _ := newClipWriteTestDeps(t, tt.pulled)
			req := clipWriteTextEvent(expected)

			resp := serveClipWriteRequest(context.Background(), deps, req)

			assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, false, "shamismatch")
			assertCredAudit(t, deps.Audit, []string{
				"clip-write-denied", "host=box", "kind=text", "reason=shamismatch",
			})
			if len(writer.snapshot()) != 0 {
				t.Fatal("SHA-mismatched clipboard bytes reached writer")
			}
		})
	}
}

func TestServeClipWriteRequest_TextHappy(t *testing.T) {
	data := []byte("text\x00bytes")
	deps, tr, writer, _, _ := newClipWriteTestDeps(t, data)
	req := clipWriteTextEvent(data)

	resp := serveClipWriteRequest(context.Background(), deps, req)

	assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, true, "")
	calls := writer.snapshot()
	if len(calls) != 1 || calls[0].kind != "text" || !bytes.Equal(calls[0].data, data) {
		t.Fatalf("text writer calls = %+v", calls)
	}
	argv, stdin := tr.calls()
	if len(argv) != 1 || len(stdin) != 1 || stdin[0] != nil {
		t.Fatalf("pull calls argv=%v stdin=%v", argv, stdin)
	}
	if len(argv[0]) != 5 || argv[0][0] != "bash" || argv[0][1] != "--noprofile" ||
		argv[0][2] != "--norc" || argv[0][3] != "-c" {
		t.Fatalf("pull argv = %v", argv[0])
	}
	joined := strings.Join(argv[0], " ")
	wantPath := `$HOME/.cache/portal/clip/copy-` + req.SHA + `.txt`
	if !strings.Contains(joined, wantPath) ||
		!strings.Contains(joined, "exec head -c "+strconv.FormatInt(req.Size, 10)) ||
		!strings.Contains(joined, `[ -L "$f" ]`) ||
		!strings.Contains(joined, `[ ! -f "$f" ]`) {
		t.Fatalf("pull command did not pin path/size/file shape: %q", joined)
	}
	assertNoClipWriteAudit(t, deps.Audit)
}

func TestServeClipWriteRequest_ImagePNG(t *testing.T) {
	data := append([]byte("\x89PNG\r\n\x1a\n"), []byte("image")...)
	deps, tr, writer, _, _ := newClipWriteTestDeps(t, data)
	req := &agentclient.ClipWriteEvent{
		Nonce: 71, Epoch: 11, Kind: "image", Format: "png",
		SHA: clipupload.ShortSHA(data), Size: int64(len(data)),
	}

	resp := serveClipWriteRequest(context.Background(), deps, req)

	assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, true, "")
	calls := writer.snapshot()
	if len(calls) != 1 || calls[0].kind != "png" || !bytes.Equal(calls[0].data, data) {
		t.Fatalf("PNG writer calls = %+v", calls)
	}
	argv, _ := tr.calls()
	if len(argv) != 1 || !strings.Contains(strings.Join(argv[0], " "),
		`$HOME/.cache/portal/clip/copy-`+req.SHA+`.png`) {
		t.Fatalf("PNG pull command = %v", argv)
	}
	assertNoClipWriteAudit(t, deps.Audit)
}

func TestServeClipWriteRequest_PullAndWriterErrors(t *testing.T) {
	data := []byte("errors")
	t.Run("pull", func(t *testing.T) {
		deps, tr, writer, logs, _ := newClipWriteTestDeps(t, data)
		tr.err = errors.New("exec failed")
		tr.stderr = "remote failure"
		req := clipWriteTextEvent(data)

		resp := serveClipWriteRequest(context.Background(), deps, req)

		assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, false, "")
		assertNoClipWriteAudit(t, deps.Audit)
		if len(writer.snapshot()) != 0 {
			t.Fatal("pull failure reached writer")
		}
		if len(logs.snapshot()) != 1 {
			t.Fatalf("pull failure logs = %v", logs.snapshot())
		}
	})

	t.Run("writer", func(t *testing.T) {
		deps, _, writer, logs, _ := newClipWriteTestDeps(t, data)
		writer.textErr = errors.New("pasteboard failed")
		req := clipWriteTextEvent(data)

		resp := serveClipWriteRequest(context.Background(), deps, req)

		assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, false, "")
		assertNoClipWriteAudit(t, deps.Audit)
		if len(logs.snapshot()) != 1 {
			t.Fatalf("writer failure logs = %v", logs.snapshot())
		}
	})
}

func TestRunClipWriteHandler_Inflight(t *testing.T) {
	data := []byte("blocked")
	deps, _, writer, _, banner := newClipWriteTestDeps(t, data)
	started := make(chan struct{})
	release := make(chan struct{})
	writer.started = started
	writer.release = release

	events := make(chan agentclient.EngineEvent, 2)
	responses := make(chan *protocol.ClipWriteResponse, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClipWriteHandlerWithDeps(ctx, events, deps, func(resp *protocol.ClipWriteResponse) error {
			copyResp := *resp
			responses <- &copyResp
			return nil
		}, nil)
	}()

	first := clipWriteTextEvent(data)
	first.Nonce = 81
	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: first}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first clipboard writer did not start")
	}
	second := clipWriteTextEvent(data)
	second.Nonce = 82
	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: second}

	select {
	case resp := <-responses:
		assertClipWriteResponse(t, resp, 82, 7, false, "inflight")
	case <-time.After(time.Second):
		t.Fatal("inflight response did not arrive")
	}
	close(release)
	select {
	case resp := <-responses:
		assertClipWriteResponse(t, resp, 81, 7, true, "")
	case <-time.After(time.Second):
		t.Fatal("first clipboard-write response did not arrive")
	}
	banner.waitRaised(t)
	cancel()
	waitClipWriteHandler(t, done)

	if len(writer.snapshot()) != 1 {
		t.Fatalf("writer calls = %v, want one", writer.snapshot())
	}
	assertCredAudit(t, deps.Audit,
		[]string{"clip-write-denied", "host=box", "kind=text", "reason=inflight"},
		[]string{"clip-written", "host=box", "kind=text",
			"sha=" + first.SHA + " size=" + strconv.FormatInt(first.Size, 10)},
	)
}

func TestRunClipWriteHandler_Ordering(t *testing.T) {
	data := []byte("ordering")
	deps, tr, writer, _, _ := newClipWriteTestDeps(t, data)
	order := &clipWriteOrder{}
	tr.order = order
	writer.order = order
	banner, bannerHarness := newClipWriteBannerHarness("box", order)
	deps.Banner = banner

	events := make(chan agentclient.EngineEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClipWriteHandlerWithDeps(ctx, events, deps, func(*protocol.ClipWriteResponse) error {
			order.add("send")
			return nil
		}, nil)
	}()
	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: clipWriteTextEvent(data)}
	bannerHarness.waitRaised(t)
	cancel()
	waitClipWriteHandler(t, done)

	want := []string{"pull", "set", "send", "banner"}
	got := order.snapshot()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("clipboard-write order = %v, want %v", got, want)
	}
}

func TestClipWriteBanner_Coalescing(t *testing.T) {
	banner, h := newClipWriteBannerHarness("box", nil)

	banner.note("text", "", 42)
	banner.note("image", "png", 118*1024)
	banner.note("clear", "", 0)
	banner.note("text", "", 5)

	calls := h.snapshot()
	if len(calls) != 1 || calls[0] != (clipWriteBannerCall{
		title: "Clipboard set from box", subtitle: "text, 42 bytes",
	}) {
		t.Fatalf("leading banner calls = %+v", calls)
	}
	h.mu.Lock()
	if len(h.durations) != 1 || h.durations[0] != clipWriteBannerWindow {
		t.Fatalf("banner durations = %v", h.durations)
	}
	h.mu.Unlock()

	h.callback(t, 0)()
	calls = h.snapshot()
	if len(calls) != 2 || calls[1] != (clipWriteBannerCall{
		title: "3 more clipboard writes from box",
	}) {
		t.Fatalf("coalesced banner calls = %+v", calls)
	}

	banner.note("image", "png", 120831)
	calls = h.snapshot()
	if len(calls) != 3 || calls[2] != (clipWriteBannerCall{
		title: "Clipboard set from box", subtitle: "image/png, 118 KB",
	}) {
		t.Fatalf("fresh leading banner calls = %+v", calls)
	}
	banner.close()
}

func TestClipWriteBanner_NoSummaryWhenNoneSuppressed(t *testing.T) {
	banner, h := newClipWriteBannerHarness("box", nil)
	banner.note("text", "", 1)

	h.callback(t, 0)()

	if calls := h.snapshot(); len(calls) != 1 {
		t.Fatalf("banner with no suppressed writes = %+v", calls)
	}
	banner.close()
}

func TestClipWriteBanner_CloseFlushesPendingSummary(t *testing.T) {
	t.Run("suppressed", func(t *testing.T) {
		banner, h := newClipWriteBannerHarness("box", nil)
		banner.note("text", "", 1)
		banner.note("text", "", 2)
		banner.note("text", "", 3)
		callback := h.callback(t, 0)

		banner.close()

		calls := h.snapshot()
		if len(calls) != 2 || calls[1].title != "2 more clipboard writes from box" {
			t.Fatalf("close-flushed banner calls = %+v", calls)
		}
		h.mu.Lock()
		stopCalls := h.stopCalls
		h.mu.Unlock()
		if stopCalls != 1 {
			t.Fatalf("timer stop calls = %d, want 1", stopCalls)
		}

		callback()
		if calls := h.snapshot(); len(calls) != 2 {
			t.Fatalf("lost timer race duplicated summary: %+v", calls)
		}
	})

	t.Run("none", func(t *testing.T) {
		banner, h := newClipWriteBannerHarness("box", nil)
		banner.note("text", "", 1)
		banner.close()
		if calls := h.snapshot(); len(calls) != 1 {
			t.Fatalf("close with no suppressed writes = %+v", calls)
		}
	})
}

func TestClipWriteBanner_NotesAfterCloseStillRaise(t *testing.T) {
	banner, h := newClipWriteBannerHarness("box", nil)
	banner.close()

	banner.note("clear", "", 0)

	calls := h.snapshot()
	if len(calls) != 1 || calls[0] != (clipWriteBannerCall{
		title: "Clipboard set from box", subtitle: "cleared",
	}) {
		t.Fatalf("post-close banner calls = %+v", calls)
	}
	h.mu.Lock()
	timerStarts := h.timerStarts
	h.mu.Unlock()
	if timerStarts != 0 {
		t.Fatalf("post-close note armed %d timers", timerStarts)
	}
}

func TestRunClipWriteHandler_BannerSurvivesCancellation(t *testing.T) {
	data := []byte("cancel-race")
	deps, _, writer, _, banner := newClipWriteTestDeps(t, data)
	started := make(chan struct{})
	release := make(chan struct{})
	writer.started = started
	writer.release = release

	events := make(chan agentclient.EngineEvent, 1)
	responses := make(chan *protocol.ClipWriteResponse, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClipWriteHandlerWithDeps(ctx, events, deps, func(resp *protocol.ClipWriteResponse) error {
			copyResp := *resp
			responses <- &copyResp
			return nil
		}, nil)
	}()
	req := clipWriteTextEvent(data)
	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: req}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("clipboard writer did not start")
	}

	cancel()
	waitClipWriteHandler(t, done)
	close(release)

	select {
	case resp := <-responses:
		assertClipWriteResponse(t, resp, req.Nonce, req.Epoch, true, "")
	case <-time.After(time.Second):
		t.Fatal("post-cancellation response did not arrive")
	}
	banner.waitRaised(t)
	assertCredAudit(t, deps.Audit, []string{
		"clip-written", "host=box", "kind=text",
		"sha=" + req.SHA + " size=" + strconv.FormatInt(req.Size, 10),
	})
}

func TestRunClipWriteHandler_BannerNotGatedOnNotify(t *testing.T) {
	data := []byte("notify-off")
	deps, _, _, _, banner := newClipWriteTestDeps(t, data)
	deps.FeatureEnabled = func(feature string) bool {
		switch feature {
		case config.FeatureClipWrite:
			return true
		case config.FeatureNotify:
			return false
		default:
			return false
		}
	}
	events := make(chan agentclient.EngineEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClipWriteHandlerWithDeps(ctx, events, deps,
			func(*protocol.ClipWriteResponse) error { return nil }, nil)
	}()
	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: clipWriteTextEvent(data)}

	banner.waitRaised(t)
	cancel()
	waitClipWriteHandler(t, done)
}

func TestClipWriteBanner_DispatchDoesNotBlockNote(t *testing.T) {
	banner := newClipWriteBanner("box")
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	banner.raise = func(string, string) {
		once.Do(func() { close(started) })
		<-release
	}
	t.Cleanup(func() {
		close(release)
		banner.close()
	})

	returned := make(chan struct{})
	go func() {
		banner.note("text", "", 1)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("banner note blocked on notification delivery")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("detached banner raise did not start")
	}
}

func TestClipWriteNoContentPreview(t *testing.T) {
	data := []byte("s3cret-token")
	deps, _, _, _, banner := newClipWriteTestDeps(t, data)
	events := make(chan agentclient.EngineEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runClipWriteHandlerWithDeps(ctx, events, deps,
			func(*protocol.ClipWriteResponse) error { return nil }, nil)
	}()
	events <- agentclient.EngineEvent{Kind: agentclient.KindClipWriteRequest, ClipWrite: clipWriteTextEvent(data)}
	banner.waitRaised(t)
	cancel()
	waitClipWriteHandler(t, done)

	for _, call := range banner.snapshot() {
		if strings.Contains(call.title, string(data)) || strings.Contains(call.subtitle, string(data)) {
			t.Fatalf("clipboard content appeared in banner: %+v", call)
		}
	}
	auditData, err := os.ReadFile(deps.Audit.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(auditData, data) {
		t.Fatal("clipboard content appeared in audit log")
	}
}
