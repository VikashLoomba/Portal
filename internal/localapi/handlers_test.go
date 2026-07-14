package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/hub"
)

// recordingPushAllow records the allowlists pushed to the AgentClient so a test
// can assert the "~100ms" push actually happened (deny/ephemeral policy lives
// in run.go, not here).
type recordingPushAllow struct{ calls [][]int }

func (r *recordingPushAllow) push(ports []int) error {
	cp := append([]int(nil), ports...)
	r.calls = append(r.calls, cp)
	return nil
}

// contains reports whether the last recorded push (or any) included port.
func (r *recordingPushAllow) sawPort(port int) bool {
	for _, call := range r.calls {
		for _, p := range call {
			if p == port {
				return true
			}
		}
	}
	return false
}

// mutServer builds a Server with a real config.Store on t.TempDir plus recording
// closures for PushAllow/Kick/Doctor. agent may be nil.
type mutDeps struct {
	push   *recordingPushAllow
	kicked *int
	report *doctor.Report
}

func newMutServer(t *testing.T, agent AgentSource) (*Server, *mutDeps) {
	t.Helper()
	md := &mutDeps{push: &recordingPushAllow{}, kicked: new(int)}
	md.report = &doctor.Report{
		Host: "box",
		Checks: []doctor.Check{
			{Name: "ssh master", Status: doctor.Pass, Detail: "UP (pid=1)"},
			{Name: "agent verb: clip", Status: doctor.Warn},
			{Name: "notify", Status: doctor.Fail, Detail: "no path"},
		},
	}
	s := New(Deps{
		Version:   api.VersionInfo{Version: "9.9", GitSHA: "deadbeef", ProtoVersion: 3},
		Agent:     agent,
		Config:    config.New(t.TempDir()),
		Hub:       hub.New(),
		PushAllow: md.push.push,
		Kick:      func() { *md.kicked++ },
		Doctor:    func(context.Context) *doctor.Report { return md.report },
	})
	return s, md
}

func doReq(s *Server, method, target string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	s.mux.ServeHTTP(rec, r)
	return rec
}

func decodeErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var eb api.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return eb.Error.Code
}

func TestHandlePorts(t *testing.T) {
	t.Run("not_configured without active stack", func(t *testing.T) {
		s, _ := newMutServer(t, &fakeAgent{ok: false})
		s.deps.PinStack = func(context.Context) (StackView, func()) {
			return StackView{HostKnown: true}, func() {}
		}
		rec := doReq(s, http.MethodGet, "/v1/ports", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if code := decodeErrCode(t, rec); code != "not_configured" {
			t.Errorf("error code = %q, want not_configured", code)
		}
	})
	t.Run("not_connected before first snapshot", func(t *testing.T) {
		agent := &fakeAgent{ok: false}
		s, _ := newMutServer(t, agent)
		s.deps.PinStack = func(context.Context) (StackView, func()) {
			return StackView{Host: "box", HostKnown: true, Agent: agent}, func() {}
		}
		rec := doReq(s, http.MethodGet, "/v1/ports", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if code := decodeErrCode(t, rec); code != "not_connected" {
			t.Errorf("error code = %q, want not_connected", code)
		}
	})
	t.Run("lists ports from snapshot", func(t *testing.T) {
		s, _ := newMutServer(t, &fakeAgent{ok: true, ports: []uint16{5000, 6000}})
		rec := doReq(s, http.MethodGet, "/v1/ports", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got []api.PortStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got) != 2 || got[0].Port != 5000 || got[1].Port != 6000 {
			t.Errorf("ports = %+v, want [5000 6000]", got)
		}
	})
}

func TestHandleAllow(t *testing.T) {
	t.Run("put adds and pushes", func(t *testing.T) {
		s, md := newMutServer(t, nil)
		rec := doReq(s, http.MethodPut, "/v1/allow/40085", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got allowlistResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !containsInt(got.Allowed, 40085) {
			t.Errorf("allowlist = %v, want to contain 40085", got.Allowed)
		}
		if !md.push.sawPort(40085) {
			t.Errorf("PushAllow calls = %v, want a push containing 40085", md.push.calls)
		}
	})
	t.Run("delete removes and pushes", func(t *testing.T) {
		s, md := newMutServer(t, nil)
		_ = doReq(s, http.MethodPut, "/v1/allow/40085", "")
		rec := doReq(s, http.MethodDelete, "/v1/allow/40085", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got allowlistResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if containsInt(got.Allowed, 40085) {
			t.Errorf("allowlist = %v, want 40085 removed", got.Allowed)
		}
		// The last push must reflect the deletion.
		last := md.push.calls[len(md.push.calls)-1]
		if containsInt(last, 40085) {
			t.Errorf("last push = %v, want 40085 removed", last)
		}
	})
	t.Run("invalid ports are 400", func(t *testing.T) {
		for _, target := range []string{"/v1/allow/70000", "/v1/allow/abc"} {
			s, _ := newMutServer(t, nil)
			rec := doReq(s, http.MethodPut, target, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, want 400", target, rec.Code)
			}
			if code := decodeErrCode(t, rec); code != "invalid_port" {
				t.Errorf("%s error code = %q, want invalid_port", target, code)
			}
		}
	})
}

func TestHandleFeatures(t *testing.T) {
	t.Run("get defaults on", func(t *testing.T) {
		s, _ := newMutServer(t, nil)
		rec := doReq(s, http.MethodGet, "/v1/features", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, f := range []string{config.FeatureClipImage, config.FeatureClipText, config.FeatureNotify, config.FeatureExec, config.FeatureCred} {
			if v, ok := got[f]; !ok || !v {
				t.Errorf("feature %q = (%v,%v), want present and true", f, v, ok)
			}
		}
	})
	t.Run("put toggles and persists", func(t *testing.T) {
		dir := t.TempDir()
		s := New(Deps{Config: config.New(dir), Hub: hub.New()})
		rec := doReq(s, http.MethodPut, "/v1/features/"+config.FeatureClipText, `{"enabled":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got[config.FeatureClipText] {
			t.Errorf("clip-text = true, want false in response")
		}
		// Persisted to the config file: a fresh Store on the same dir sees it off.
		if config.New(dir).FeatureEnabled(config.FeatureClipText) {
			t.Errorf("clip-text still enabled after persist")
		}
	})
	t.Run("unknown feature is 404", func(t *testing.T) {
		s, _ := newMutServer(t, nil)
		rec := doReq(s, http.MethodPut, "/v1/features/bogus", `{"enabled":true}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if code := decodeErrCode(t, rec); code != "feature_unknown" {
			t.Errorf("error code = %q, want feature_unknown", code)
		}
	})
	t.Run("malformed body is 400", func(t *testing.T) {
		s, _ := newMutServer(t, nil)
		rec := doReq(s, http.MethodPut, "/v1/features/"+config.FeatureNotify, "{not json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if code := decodeErrCode(t, rec); code != "invalid_request" {
			t.Errorf("error code = %q, want invalid_request", code)
		}
	})
}

func TestHandleReconcile(t *testing.T) {
	s, md := newMutServer(t, nil)
	rec := doReq(s, http.MethodPost, "/v1/reconcile", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if *md.kicked != 1 {
		t.Errorf("Kick called %d times, want 1", *md.kicked)
	}
}

func TestHandleDoctor(t *testing.T) {
	s, md := newMutServer(t, nil)
	rec := doReq(s, http.MethodPost, "/v1/doctor", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// doctor.Status marshals to PASS/WARN/FAIL but has no UnmarshalJSON, so
	// decode into a string-status view of the report.
	type checkView struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}
	var got struct {
		Host   string      `json:"host"`
		Checks []checkView `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Host != md.report.Host || len(got.Checks) != len(md.report.Checks) {
		t.Fatalf("report = %+v, want %+v", got, *md.report)
	}
	want := []checkView{
		{Name: "ssh master", Status: "PASS", Detail: "UP (pid=1)"},
		{Name: "agent verb: clip", Status: "WARN"},
		{Name: "notify", Status: "FAIL", Detail: "no path"},
	}
	for i, w := range want {
		if got.Checks[i] != w {
			t.Errorf("check[%d] = %+v, want %+v", i, got.Checks[i], w)
		}
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
