package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
)

type fakeSetupBehavior struct {
	validationFails bool
	configureErr    error
	panicConfigure  bool
	concurrentLines bool
	blockValidate   bool
	releaseValidate <-chan struct{}
}

type fakeSetupRunner struct {
	sink     func(api.SetupEvent)
	behavior fakeSetupBehavior

	mu            sync.Mutex
	validateHost  string
	validateForce bool
	configureHost string
	deployHost    string
	verifyHost    string

	validateStarted chan struct{}
	canceled        chan struct{}
	closed          chan struct{}
	startOnce       sync.Once
	cancelOnce      sync.Once
	closeOnce       sync.Once
}

func newFakeSetupRunner(sink func(api.SetupEvent), behavior fakeSetupBehavior) *fakeSetupRunner {
	return &fakeSetupRunner{
		sink:            sink,
		behavior:        behavior,
		validateStarted: make(chan struct{}),
		canceled:        make(chan struct{}),
		closed:          make(chan struct{}),
	}
}

func (r *fakeSetupRunner) Validate(ctx context.Context, host string, force bool) bool {
	r.mu.Lock()
	r.validateHost = host
	r.validateForce = force
	r.mu.Unlock()
	r.sink(api.SetupEvent{Step: "validate", Status: "running"})

	if r.behavior.blockValidate {
		r.startOnce.Do(func() { close(r.validateStarted) })
		select {
		case <-ctx.Done():
			r.cancelOnce.Do(func() { close(r.canceled) })
			return false
		case <-r.behavior.releaseValidate:
		}
	}

	if r.behavior.concurrentLines {
		var wg sync.WaitGroup
		for _, line := range []string{"auth one", "auth two"} {
			wg.Add(1)
			go func(line string) {
				defer wg.Done()
				r.sink(api.SetupEvent{Step: "validate", Status: "running", Line: line})
			}(line)
		}
		wg.Wait()
	} else {
		r.sink(api.SetupEvent{Step: "validate", Status: "running", Line: "auth progress"})
	}

	if r.behavior.validationFails {
		status := "fail"
		if force {
			status = "warn"
		}
		r.sink(api.SetupEvent{
			Step:   "validate",
			Status: status,
			Error:  &api.ErrorDetail{Code: "validation_failed", Message: "unreachable"},
		})
		return force
	}
	r.sink(api.SetupEvent{Step: "validate", Status: "ok"})
	return true
}

func (r *fakeSetupRunner) Configure(_ context.Context, host string) error {
	r.mu.Lock()
	r.configureHost = host
	r.mu.Unlock()
	r.sink(api.SetupEvent{Step: "configure", Status: "running"})
	if r.behavior.panicConfigure {
		panic("configure exploded")
	}
	if r.behavior.configureErr != nil {
		r.sink(api.SetupEvent{
			Step:   "configure",
			Status: "fail",
			Error:  &api.ErrorDetail{Code: "configure_failed", Message: r.behavior.configureErr.Error()},
		})
		return r.behavior.configureErr
	}
	r.sink(api.SetupEvent{Step: "configure", Status: "ok"})
	return nil
}

func (r *fakeSetupRunner) DeployRemote(_ context.Context, host string) {
	r.mu.Lock()
	r.deployHost = host
	r.mu.Unlock()
	for _, step := range []string{"xdg-open", "clip-shims", "agent-symlink"} {
		r.sink(api.SetupEvent{Step: step, Status: "running"})
		r.sink(api.SetupEvent{Step: step, Status: "ok"})
	}
}

func (r *fakeSetupRunner) Verify(_ context.Context, host string) *doctor.Report {
	r.mu.Lock()
	r.verifyHost = host
	r.mu.Unlock()
	rep := &doctor.Report{Host: host}
	raw, _ := json.Marshal(rep)
	r.sink(api.SetupEvent{Step: "doctor", Status: "running"})
	r.sink(api.SetupEvent{Step: "doctor", Status: "ok", Report: raw})
	return rep
}

func (r *fakeSetupRunner) Close(context.Context) {
	r.closeOnce.Do(func() { close(r.closed) })
}

func (r *fakeSetupRunner) hosts() (validate string, force bool, configure, deploy, verify string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.validateHost, r.validateForce, r.configureHost, r.deployHost, r.verifyHost
}

type fakeSetupFactory struct {
	mu       sync.Mutex
	behavior fakeSetupBehavior
	runners  []*fakeSetupRunner
}

func (f *fakeSetupFactory) New(sink func(api.SetupEvent)) SetupRunner {
	r := newFakeSetupRunner(sink, f.behavior)
	f.mu.Lock()
	f.runners = append(f.runners, r)
	f.mu.Unlock()
	return r
}

func (f *fakeSetupFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runners)
}

func (f *fakeSetupFactory) last() *fakeSetupRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runners) == 0 {
		return nil
	}
	return f.runners[len(f.runners)-1]
}

func waitSetupRunner(t *testing.T, f *fakeSetupFactory) *fakeSetupRunner {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.last(); r != nil {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("setup runner was not constructed")
	return nil
}

func assertSetupRunnerClosed(t *testing.T, r *fakeSetupRunner) {
	t.Helper()
	select {
	case <-r.closed:
	default:
		t.Fatal("setup runner was not closed")
	}
}

type fakeSetupActivator struct {
	mu    sync.Mutex
	err   error
	hosts []string
}

func (a *fakeSetupActivator) Activate(_ context.Context, host string) error {
	a.mu.Lock()
	a.hosts = append(a.hosts, host)
	a.mu.Unlock()
	return a.err
}

func (a *fakeSetupActivator) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.hosts...)
}

func normalizeSetupTestHost(raw string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		default:
			return r
		}
	}, raw)
}

func newSetupTestServer(t *testing.T, oldHost string, f *fakeSetupFactory, act *fakeSetupActivator, a *audit.Log) (*Server, string) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "api.sock")
	s := New(Deps{
		Host:          func() (string, error) { return oldHost, nil },
		Audit:         a,
		NewSetup:      f.New,
		Activate:      act.Activate,
		NormalizeHost: normalizeSetupTestHost,
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
			t.Fatal("setup server did not stop")
		}
	})
	waitVersion(t, path)
	return s, path
}

func postSetup(t *testing.T, path, body string) (*http.Response, []api.SetupEvent) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://unix/v1/setup", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new setup request: %v", err)
	}
	resp, err := streamingClient(path).Do(req)
	if err != nil {
		t.Fatalf("POST /v1/setup: %v", err)
	}
	events := readSetupEvents(t, resp.Body)
	resp.Body.Close()
	return resp, events
}

func readSetupEvents(t *testing.T, body io.Reader) []api.SetupEvent {
	t.Helper()
	var events []api.SetupEvent
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		var ev api.SetupEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode setup line %q: %v", sc.Text(), err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read setup stream: %v", err)
	}
	return events
}

func assertSetupGrammar(t *testing.T, events []api.SetupEvent, expectedSteps []string, doneStatus string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("setup stream was empty")
	}
	if got := events[len(events)-1]; got.Step != "done" || got.Status != doneStatus {
		t.Fatalf("last event = %+v, want done/%s", got, doneStatus)
	}

	states := make(map[string]int)
	var seenSteps []string
	doneCount := 0
	for i, ev := range events {
		if ev.Step == "done" {
			doneCount++
			if i != len(events)-1 {
				t.Fatalf("done event at index %d of %d, want last", i, len(events))
			}
			if ev.Line != "" {
				t.Fatalf("done event carried line: %+v", ev)
			}
			continue
		}
		state := states[ev.Step]
		if ev.Line != "" {
			if ev.Status != "running" || state != 1 {
				t.Fatalf("line event outside running phase: %+v state=%d", ev, state)
			}
			continue
		}
		switch ev.Status {
		case "running":
			if state != 0 {
				t.Fatalf("duplicate/out-of-order running event: %+v state=%d", ev, state)
			}
			states[ev.Step] = 1
			seenSteps = append(seenSteps, ev.Step)
		case "ok", "warn", "fail":
			if state != 1 {
				t.Fatalf("terminal without exactly one running event: %+v state=%d", ev, state)
			}
			states[ev.Step] = 2
		default:
			t.Fatalf("unexpected setup status: %+v", ev)
		}
	}
	if doneCount != 1 {
		t.Fatalf("done event count = %d, want 1", doneCount)
	}
	if fmt.Sprint(seenSteps) != fmt.Sprint(expectedSteps) {
		t.Fatalf("step order = %v, want %v", seenSteps, expectedSteps)
	}
	for _, step := range expectedSteps {
		if states[step] != 2 {
			t.Fatalf("step %q final state = %d, want terminal", step, states[step])
		}
	}
}

func findSetupEvent(events []api.SetupEvent, step, status string) *api.SetupEvent {
	for i := range events {
		if events[i].Step == step && events[i].Status == status {
			return &events[i]
		}
	}
	return nil
}

func TestHandleSetupHappyStreamAndAudit(t *testing.T) {
	a := audit.New(t.TempDir())
	f := &fakeSetupFactory{behavior: fakeSetupBehavior{concurrentLines: true}}
	act := &fakeSetupActivator{}
	_, path := newSetupTestServer(t, "oldbox", f, act, a)

	resp, events := postSetup(t, path, `{"host":" new box ","force":false}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", got)
	}
	assertSetupGrammar(t, events,
		[]string{"validate", "configure", "xdg-open", "clip-shims", "agent-symlink", "activate", "doctor"}, "ok")
	if ev := findSetupEvent(events, "doctor", "ok"); ev == nil || len(ev.Report) == 0 {
		t.Fatalf("doctor terminal event missing report: %+v", ev)
	}
	if got := act.calls(); fmt.Sprint(got) != "[newbox]" {
		t.Fatalf("Activate calls = %v, want [newbox]", got)
	}
	r := waitSetupRunner(t, f)
	validate, force, configure, deploy, verify := r.hosts()
	if validate != "newbox" || force || configure != "newbox" || deploy != "newbox" || verify != "newbox" {
		t.Fatalf("runner hosts/force = %q/%v %q %q %q, want newbox/false throughout", validate, force, configure, deploy, verify)
	}
	select {
	case <-r.closed:
	default:
		t.Fatal("runner.Close was not called")
	}

	lines := waitAuditLines(t, a.Path(), 1, 2*time.Second)
	fields := auditFields(lines[0])
	if fields["host"] != "newbox" || fields["forced"] != "false" || fields["verdict"] != "ok" {
		t.Fatalf("setup audit fields = %v", fields)
	}
	if fields["steps"] != "validate=ok configure=ok xdg-open=ok clip-shims=ok agent-symlink=ok activate=ok doctor=ok" {
		t.Fatalf("setup audit steps = %q", fields["steps"])
	}
	if fields["activation"] != "oldbox→newbox" {
		t.Fatalf("setup audit activation = %q", fields["activation"])
	}
}

func TestHandleSetupInBandFailuresAndForce(t *testing.T) {
	allSteps := []string{"validate", "configure", "xdg-open", "clip-shims", "agent-symlink", "activate", "doctor"}

	t.Run("configure fail stops remaining phases", func(t *testing.T) {
		f := &fakeSetupFactory{behavior: fakeSetupBehavior{configureErr: errors.New("disk full")}}
		act := &fakeSetupActivator{}
		a := audit.New(t.TempDir())
		_, path := newSetupTestServer(t, "old", f, act, a)
		resp, events := postSetup(t, path, `{"host":"box"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertSetupGrammar(t, events, []string{"validate", "configure"}, "fail")
		if ev := findSetupEvent(events, "configure", "fail"); ev == nil || ev.Error == nil || ev.Error.Code != "configure_failed" {
			t.Fatalf("configure failure event = %+v", ev)
		}
		if len(act.calls()) != 0 {
			t.Fatalf("Activate called after configure failure: %v", act.calls())
		}
		assertSetupRunnerClosed(t, f.last())
		fields := auditFields(waitAuditLines(t, a.Path(), 1, 2*time.Second)[0])
		if fields["forced"] != "false" || fields["steps"] != "validate=ok configure=fail" || fields["activation"] != "" || fields["verdict"] != "fail" {
			t.Fatalf("configure-failure audit fields = %v", fields)
		}
	})

	t.Run("validate fail without force stops before configure", func(t *testing.T) {
		f := &fakeSetupFactory{behavior: fakeSetupBehavior{validationFails: true}}
		act := &fakeSetupActivator{}
		_, path := newSetupTestServer(t, "old", f, act, audit.New(t.TempDir()))
		resp, events := postSetup(t, path, `{"host":"box"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertSetupGrammar(t, events, []string{"validate"}, "fail")
		_, _, configure, _, _ := f.last().hosts()
		if configure != "" || len(act.calls()) != 0 {
			t.Fatalf("configure=%q activate=%v after validate failure", configure, act.calls())
		}
		assertSetupRunnerClosed(t, f.last())
	})

	t.Run("force degrades validation failure and continues", func(t *testing.T) {
		f := &fakeSetupFactory{behavior: fakeSetupBehavior{validationFails: true}}
		act := &fakeSetupActivator{}
		a := audit.New(t.TempDir())
		_, path := newSetupTestServer(t, "old", f, act, a)
		resp, events := postSetup(t, path, `{"host":"box","force":true}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertSetupGrammar(t, events, allSteps, "ok")
		if findSetupEvent(events, "validate", "warn") == nil {
			t.Fatalf("forced validation stream missing warn: %+v", events)
		}
		_, force, _, _, _ := f.last().hosts()
		if !force {
			t.Fatal("force was not passed to Validate")
		}
		fields := auditFields(waitAuditLines(t, a.Path(), 1, 2*time.Second)[0])
		if fields["forced"] != "true" || fields["steps"] != "validate=warn configure=ok xdg-open=ok clip-shims=ok agent-symlink=ok activate=ok doctor=ok" || fields["activation"] != "old→box" || fields["verdict"] != "ok" {
			t.Fatalf("forced-warning audit fields = %v", fields)
		}
	})

	t.Run("activate no-op is ok", func(t *testing.T) {
		f := &fakeSetupFactory{}
		act := &fakeSetupActivator{}
		_, path := newSetupTestServer(t, "box", f, act, audit.New(t.TempDir()))
		_, events := postSetup(t, path, `{"host":"box"}`)
		assertSetupGrammar(t, events, allSteps, "ok")
		if findSetupEvent(events, "activate", "ok") == nil {
			t.Fatalf("same-host activate did not emit ok: %+v", events)
		}
	})

	t.Run("activate fail stops doctor", func(t *testing.T) {
		f := &fakeSetupFactory{}
		act := &fakeSetupActivator{err: errors.New("construct failed")}
		a := audit.New(t.TempDir())
		_, path := newSetupTestServer(t, "old", f, act, a)
		resp, events := postSetup(t, path, `{"host":"box"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		assertSetupGrammar(t, events,
			[]string{"validate", "configure", "xdg-open", "clip-shims", "agent-symlink", "activate"}, "fail")
		if ev := findSetupEvent(events, "activate", "fail"); ev == nil || ev.Error == nil || ev.Error.Code != "activate_failed" {
			t.Fatalf("activate failure event = %+v", ev)
		}
		if findSetupEvent(events, "doctor", "running") != nil {
			t.Fatalf("doctor ran after activate failure: %+v", events)
		}
		assertSetupRunnerClosed(t, f.last())
		fields := auditFields(waitAuditLines(t, a.Path(), 1, 2*time.Second)[0])
		if fields["forced"] != "false" || fields["steps"] != "validate=ok configure=ok xdg-open=ok clip-shims=ok agent-symlink=ok activate=fail" || fields["activation"] != "old→box (failed)" || fields["verdict"] != "fail" {
			t.Fatalf("activate-failure audit fields = %v", fields)
		}
	})
}

func TestHandleSetupPanicAfterHeadersIsInBand(t *testing.T) {
	f := &fakeSetupFactory{behavior: fakeSetupBehavior{panicConfigure: true}}
	act := &fakeSetupActivator{}
	_, path := newSetupTestServer(t, "old", f, act, audit.New(t.TempDir()))

	resp, events := postSetup(t, path, `{"host":"box"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", resp.StatusCode)
	}
	assertSetupGrammar(t, events, []string{"validate", "configure"}, "fail")
	if ev := findSetupEvent(events, "configure", "fail"); ev == nil || ev.Error == nil || ev.Error.Code != "internal" {
		t.Fatalf("configure panic event = %+v, want in-band internal failure", ev)
	}
	assertSetupRunnerClosed(t, f.last())
}

func TestHandleSetupSingleFlightBeforeDecode(t *testing.T) {
	release := make(chan struct{})
	f := &fakeSetupFactory{behavior: fakeSetupBehavior{blockValidate: true, releaseValidate: release}}
	act := &fakeSetupActivator{}
	s, path := newSetupTestServer(t, "old", f, act, audit.New(t.TempDir()))

	req, err := http.NewRequest(http.MethodPost, "http://unix/v1/setup", strings.NewReader(`{"host":"box"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := streamingClient(path).Do(req)
	if err != nil {
		t.Fatalf("first setup: %v", err)
	}
	r := waitSetupRunner(t, f)
	select {
	case <-r.validateStarted:
	case <-time.After(2 * time.Second):
		resp.Body.Close()
		t.Fatal("first setup did not enter Validate")
	}

	for _, body := range []string{`{"host":"other"}`, `{not json`} {
		rec := doReq(s, http.MethodPost, "/v1/setup", body)
		if rec.Code != http.StatusConflict {
			resp.Body.Close()
			t.Fatalf("concurrent body %q status = %d, want 409", body, rec.Code)
		}
		if code := decodeErrCode(t, rec); code != "setup_in_progress" {
			resp.Body.Close()
			t.Fatalf("concurrent body %q code = %q", body, code)
		}
	}
	if f.count() != 1 {
		resp.Body.Close()
		t.Fatalf("runner count during single-flight = %d, want 1", f.count())
	}

	close(release)
	events := readSetupEvents(t, resp.Body)
	resp.Body.Close()
	assertSetupGrammar(t, events,
		[]string{"validate", "configure", "xdg-open", "clip-shims", "agent-symlink", "activate", "doctor"}, "ok")

	rec := doReq(s, http.MethodPost, "/v1/setup", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("post-completion malformed status = %d, want 400", rec.Code)
	}
}

func TestHandleSetupDisconnectCancelsAndAudits(t *testing.T) {
	a := audit.New(t.TempDir())
	f := &fakeSetupFactory{behavior: fakeSetupBehavior{blockValidate: true, releaseValidate: make(chan struct{})}}
	act := &fakeSetupActivator{}
	s, path := newSetupTestServer(t, "old", f, act, a)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/setup", strings.NewReader(`{"host":"box"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := streamingClient(path).Do(req)
	if err != nil {
		t.Fatalf("POST /v1/setup: %v", err)
	}
	r := waitSetupRunner(t, f)
	select {
	case <-r.validateStarted:
	case <-time.After(2 * time.Second):
		cancel()
		resp.Body.Close()
		t.Fatal("setup did not enter Validate")
	}
	cancel()
	resp.Body.Close()

	select {
	case <-r.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not observe request cancellation")
	}
	select {
	case <-r.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("runner.Close was not called after disconnect")
	}
	lines := waitAuditLines(t, a.Path(), 1, 2*time.Second)
	if verdict := auditFields(lines[0])["verdict"]; verdict != "canceled" {
		t.Fatalf("disconnect audit verdict = %q, want canceled: %s", verdict, lines[0])
	}
	rec := doReq(s, http.MethodPost, "/v1/setup", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("follow-up status = %d, want 400 proving lock release", rec.Code)
	}
}

type failDoneWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *failDoneWriter) Header() http.Header { return w.header }

func (w *failDoneWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *failDoneWriter) Write(p []byte) (int, error) {
	var ev api.SetupEvent
	if json.Unmarshal(bytes.TrimSpace(p), &ev) == nil && ev.Step == "done" {
		return 0, errors.New("client disconnected before done")
	}
	return w.body.Write(p)
}

func (w *failDoneWriter) Flush() {}

func (w *failDoneWriter) SetWriteDeadline(time.Time) error { return nil }

func TestHandleSetupDoneWriteFailureAuditsNonSuccess(t *testing.T) {
	a := audit.New(t.TempDir())
	f := &fakeSetupFactory{}
	act := &fakeSetupActivator{}
	s := New(Deps{
		Host:          func() (string, error) { return "old", nil },
		Audit:         a,
		NewSetup:      f.New,
		Activate:      act.Activate,
		NormalizeHost: normalizeSetupTestHost,
	})
	w := &failDoneWriter{header: make(http.Header)}
	r := httptest.NewRequest(http.MethodPost, "/v1/setup", strings.NewReader(`{"host":"box"}`))

	s.handleSetup(w, r)

	if w.status != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", w.status)
	}
	if strings.Contains(w.body.String(), `"step":"done"`) {
		t.Fatalf("failing writer unexpectedly delivered done: %s", w.body.String())
	}
	if !strings.Contains(w.body.String(), `"step":"doctor","status":"ok"`) {
		t.Fatalf("happy phases did not finish before done failure: %s", w.body.String())
	}
	lines := waitAuditLines(t, a.Path(), 1, 2*time.Second)
	if verdict := auditFields(lines[0])["verdict"]; verdict != "canceled" {
		t.Fatalf("done-write audit verdict = %q, want canceled: %s", verdict, lines[0])
	}
}

func TestHandleSetupPreStreamValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		host    string
		want    int
		wantRun int
	}{
		{name: "absent body", body: "", host: "box", want: http.StatusBadRequest},
		{name: "null body", body: "null", host: "box", want: http.StatusBadRequest},
		{name: "scalar body", body: `"box"`, host: "box", want: http.StatusBadRequest},
		{name: "array body", body: `[]`, host: "box", want: http.StatusBadRequest},
		{name: "null host", body: `{"host":null}`, host: "box", want: http.StatusBadRequest},
		{name: "null force", body: `{"force":null}`, host: "box", want: http.StatusBadRequest},
		{name: "trailing second value", body: `{"host":"box"}{}`, host: "box", want: http.StatusBadRequest},
		{name: "trailing bracket", body: `{"host":"box"}]`, host: "box", want: http.StatusBadRequest},
		{name: "trailing brace", body: `{"host":"box"}}`, host: "box", want: http.StatusBadRequest},
		{name: "no resolvable host", body: `{}`, host: "", want: http.StatusBadRequest},
		{name: "trailing whitespace", body: "{\"host\":\"box\"}\n \t", host: "old", want: http.StatusOK, wantRun: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSetupFactory{}
			act := &fakeSetupActivator{}
			a := audit.New(t.TempDir())
			s := New(Deps{
				Host:          func() (string, error) { return tc.host, nil },
				Audit:         a,
				NewSetup:      f.New,
				Activate:      act.Activate,
				NormalizeHost: normalizeSetupTestHost,
			})
			rec := doReq(s, http.MethodPost, "/v1/setup", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusBadRequest {
				if code := decodeErrCode(t, rec); code != "invalid_request" {
					t.Fatalf("error code = %q, want invalid_request", code)
				}
			}
			if f.count() != tc.wantRun {
				t.Fatalf("runner count = %d, want %d", f.count(), tc.wantRun)
			}
			if tc.wantRun == 0 && len(act.calls()) != 0 {
				t.Fatalf("Activate called on pre-stream reject: %v", act.calls())
			}
			if tc.want == http.StatusBadRequest {
				if _, err := os.Stat(a.Path()); !os.IsNotExist(err) {
					t.Fatalf("pre-stream reject wrote an audit entry: err=%v", err)
				}
			}
		})
	}
}

func TestHandleSetupDefaultsToActiveHost(t *testing.T) {
	a := audit.New(t.TempDir())
	f := &fakeSetupFactory{}
	act := &fakeSetupActivator{}
	_, path := newSetupTestServer(t, "box", f, act, a)

	resp, events := postSetup(t, path, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	assertSetupGrammar(t, events,
		[]string{"validate", "configure", "xdg-open", "clip-shims", "agent-symlink", "activate", "doctor"}, "ok")
	validate, _, _, _, _ := f.last().hosts()
	if validate != "box" || fmt.Sprint(act.calls()) != "[box]" {
		t.Fatalf("defaulted runner/activate hosts = %q/%v, want box", validate, act.calls())
	}
	line := waitAuditLines(t, a.Path(), 1, 2*time.Second)[0]
	fields := auditFields(line)
	if fields["host"] != "box" || fields["activation"] != "box→box" || fields["verdict"] != "ok" {
		t.Fatalf("default-host audit fields = %v", fields)
	}
}
