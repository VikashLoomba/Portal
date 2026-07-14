package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/api"
)

func TestSetupHappyPath(t *testing.T) {
	serverResult := make(chan error, 1)
	wantEvents := []api.SetupEvent{
		{Step: "validate", Status: "running"},
		{Step: "validate", Status: "ok"},
		{Step: "configure", Status: "running"},
		{Step: "configure", Status: "ok"},
		{Step: "xdg-open", Status: "running"},
		{Step: "xdg-open", Status: "ok"},
		{Step: "clip-shims", Status: "running"},
		{Step: "clip-shims", Status: "ok"},
		{Step: "agent-symlink", Status: "running"},
		{Step: "agent-symlink", Status: "ok"},
		{Step: "activate", Status: "running"},
		{Step: "activate", Status: "ok"},
		{Step: "doctor", Status: "running"},
		{Step: "doctor", Status: "ok", Report: json.RawMessage(`{"host":"devbox"}`)},
		{Step: "done", Status: "ok"},
	}
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverResult <- func() error {
			if r.Method != http.MethodPost {
				return fmt.Errorf("method = %s, want POST", r.Method)
			}
			if r.URL.Path != "/v1/setup" {
				return fmt.Errorf("path = %q, want /v1/setup", r.URL.Path)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				return fmt.Errorf("Content-Type = %q, want application/json", got)
			}
			var got api.SetupRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				return fmt.Errorf("decode request: %w", err)
			}
			if got.Host != "user@devbox" || got.Force {
				return fmt.Errorf("request = %+v, want host user@devbox and force false", got)
			}
			return writeSetupEvents(w, wantEvents)
		}()
	}))

	seq, err := New(path).Setup(context.Background(), api.SetupRequest{Host: "user@devbox"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if seq == nil {
		t.Fatal("Setup sequence = nil")
	}

	var events []api.SetupEvent
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("Setup iterator error: %v", err)
		}
		events = append(events, ev)
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	assertSetupServerResult(t, serverResult)
}

func TestSetupInBandFail(t *testing.T) {
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = writeSetupEvents(w, []api.SetupEvent{
			{Step: "validate", Status: "running"},
			{
				Step:   "validate",
				Status: "fail",
				Error:  &api.ErrorDetail{Code: "validation_failed", Message: "ssh unreachable"},
			},
			{Step: "done", Status: "fail"},
		})
	}))

	seq, err := New(path).Setup(context.Background(), api.SetupRequest{Host: "badbox"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var events []api.SetupEvent
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("in-band failure surfaced as iterator error: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events))
	}
	if fail := events[1]; fail.Status != "fail" || fail.Error == nil || fail.Error.Code != "validation_failed" {
		t.Fatalf("failure event = %+v, want validation_failed detail", fail)
	}
	if done := events[2]; done.Step != "done" || done.Status != "fail" {
		t.Fatalf("terminal event = %+v, want done/fail", done)
	}
}

func TestSetupForceWarn(t *testing.T) {
	serverResult := make(chan error, 1)
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.SetupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			serverResult <- err
			return
		}
		if req.Host != "badbox" || !req.Force {
			serverResult <- fmt.Errorf("request = %+v, want badbox with force", req)
			return
		}
		serverResult <- writeSetupEvents(w, []api.SetupEvent{
			{Step: "validate", Status: "running"},
			{Step: "validate", Status: "warn", Error: &api.ErrorDetail{Code: "validation_failed", Message: "ssh unreachable"}},
			{Step: "done", Status: "ok"},
		})
	}))

	seq, err := New(path).Setup(context.Background(), api.SetupRequest{Host: "badbox", Force: true})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var events []api.SetupEvent
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("forced warning surfaced as iterator error: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 3 || events[1].Status != "warn" || events[2].Step != "done" || events[2].Status != "ok" {
		t.Fatalf("events = %+v, want validate warn followed by done ok", events)
	}
	assertSetupServerResult(t, serverResult)
}

func TestSetupActivateFailurePreservesActiveState(t *testing.T) {
	const activeHost = "old-box"
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/setup":
			_ = writeSetupEvents(w, []api.SetupEvent{
				{Step: "activate", Status: "running"},
				{Step: "activate", Status: "fail", Error: &api.ErrorDetail{Code: "activate_failed", Message: "construct failed"}},
				{Step: "done", Status: "fail"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.Status{Host: activeHost})
		default:
			http.NotFound(w, r)
		}
	}))

	c := New(path)
	seq, err := c.Setup(context.Background(), api.SetupRequest{Host: "new-box"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var events []api.SetupEvent
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("activate failure surfaced as iterator error: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 3 || events[1].Error == nil || events[1].Error.Code != "activate_failed" || events[2].Status != "fail" {
		t.Fatalf("events = %+v, want activate failure followed by done fail", events)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status after failed activation: %v", err)
	}
	if st.Host != activeHost {
		t.Fatalf("active host after failed activation = %q, want %q", st.Host, activeHost)
	}
}

func TestSetupSkipsBlankNDJSONLines(t *testing.T) {
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "\n{\"step\":\"validate\",\"status\":\"running\"}\n\n{\"step\":\"done\",\"status\":\"ok\"}\n\n")
	}))

	seq, err := New(path).Setup(context.Background(), api.SetupRequest{Host: "box"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	var events []api.SetupEvent
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("blank line surfaced as iterator error: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 2 || events[0].Step != "validate" || events[1].Step != "done" {
		t.Fatalf("events = %+v, want validate and done", events)
	}
}

func TestSetupPreStreamReject(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "invalid request", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "setup in progress", status: http.StatusConflict, code: "setup_in_progress"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(api.ErrorBody{Error: api.ErrorDetail{Code: tt.code, Message: tt.name}})
			}))

			seq, err := New(path).Setup(context.Background(), api.SetupRequest{Host: "devbox"})
			if seq != nil {
				t.Fatal("Setup sequence is non-nil after pre-stream reject")
			}
			var apiErr *api.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("Setup error = %v (%T), want *api.APIError", err, err)
			}
			if apiErr.Status != tt.status || apiErr.Code != tt.code {
				t.Fatalf("APIError = %+v, want status %d code %q", apiErr, tt.status, tt.code)
			}
		})
	}
}

func TestSetupDisconnectCancel(t *testing.T) {
	handlerDone := make(chan struct{}, 1)
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(api.SetupEvent{Step: "validate", Status: "running"})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		handlerDone <- struct{}{}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	seq, err := New(path).Setup(ctx, api.SetupRequest{Host: "devbox"})
	if err != nil {
		cancel()
		t.Fatalf("Setup: %v", err)
	}

	first := make(chan api.SetupEvent, 1)
	iteratorDone := make(chan error, 1)
	go func() {
		var terminal error
		for ev, err := range seq {
			if err != nil {
				terminal = err
				break
			}
			first <- ev
		}
		iteratorDone <- terminal
	}()

	select {
	case ev := <-first:
		if ev.Step != "validate" || ev.Status != "running" {
			cancel()
			t.Fatalf("first event = %+v, want validate/running", ev)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for first setup event")
	}
	cancel()

	select {
	case <-iteratorDone:
		// Cancellation may surface as a read error or a clean stream stop.
	case <-time.After(2 * time.Second):
		t.Fatal("Setup iterator did not return after context cancellation")
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stub handler did not observe client disconnect")
	}
}

func TestWaitReadyBecomesReady(t *testing.T) {
	serverResult := make(chan error, 1)
	var probes atomic.Int32
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/version" {
			serverResult <- fmt.Errorf("request = %s %s, want GET /v1/version", r.Method, r.URL.Path)
			return
		}
		if probes.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				serverResult <- errors.New("response writer does not implement http.Hijacker")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				serverResult <- fmt.Errorf("hijack first probe: %w", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		serverResult <- nil
	}))

	if err := New(path).WaitReady(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	assertSetupServerResult(t, serverResult)
	if got := probes.Load(); got < 2 {
		t.Fatalf("probe count = %d, want at least 2", got)
	}
}

func TestWaitReadyAcceptsAnyHTTPResponse(t *testing.T) {
	serverResult := make(chan error, 1)
	path := startSetupStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/version" {
			serverResult <- fmt.Errorf("request = %s %s, want GET /v1/version", r.Method, r.URL.Path)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		serverResult <- nil
	}))

	if err := New(path).WaitReady(context.Background(), time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	assertSetupServerResult(t, serverResult)
}

func TestWaitReadyTimeout(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "nonexistent.sock")
	start := time.Now()
	err := New(path).WaitReady(context.Background(), 40*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WaitReady elapsed = %v, want bounded timeout", elapsed)
	}
}

func TestWaitReadyTimeoutCancelsHungProbe(t *testing.T) {
	requestStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	path := startSetupStub(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-handlerDone
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ctx.Done()
		close(handlerDone)
	}()
	start := time.Now()
	err := New(path).WaitReady(ctx, 40*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady error = %v, want context.DeadlineExceeded", err)
	}
	cancel()
	select {
	case <-requestStarted:
	default:
		t.Fatal("WaitReady did not start a /v1/version probe")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("WaitReady elapsed = %v, want bounded hung-probe timeout", elapsed)
	}
}

func TestWaitReadyContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(filepath.Join(shortTempDir(t), "nonexistent.sock")).WaitReady(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReady error = %v, want context.Canceled", err)
	}
}

func startSetupStub(t *testing.T, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("stub server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("stub server did not stop")
		}
	})
	return path
}

func writeSetupEvents(w http.ResponseWriter, events []api.SetupEvent) error {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not implement http.Flusher")
	}
	enc := json.NewEncoder(w)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		flusher.Flush()
	}
	return nil
}

func assertSetupServerResult(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stub handler did not finish")
	}
}
