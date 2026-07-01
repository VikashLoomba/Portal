package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/service"
)

// fakeAgent is a function-level AgentSource fake (no wire, no goroutines).
type fakeAgent struct {
	ack     *protocol.HelloAck
	seq     uint64
	ports   []uint16
	ok      bool
	lastErr string
}

func (f *fakeAgent) HelloAck() *protocol.HelloAck       { return f.ack }
func (f *fakeAgent) Snapshot() (uint64, []uint16, bool) { return f.seq, f.ports, f.ok }
func (f *fakeAgent) LastDisconnectErr() string          { return f.lastErr }

type fakeMaster struct{ pid int }

func (f fakeMaster) MasterPID(context.Context) (int, error) { return f.pid, nil }

type fakeForwards struct{ lines []string }

func (f fakeForwards) MasterForwardLines(context.Context, int) ([]string, error) {
	return f.lines, nil
}

type fakeService struct{ st service.Status }

func (f fakeService) Status(context.Context) (service.Status, error) { return f.st, nil }

// newTestServer builds a Server over in-file fakes plus a real config.Store on
// t.TempDir. agent may be nil (no handshake).
func newTestServer(t *testing.T, agent AgentSource) *Server {
	t.Helper()
	return New(Deps{
		Version: VersionInfo{Version: "9.9", GitSHA: "deadbeef", ProtoVersion: 3},
		Host:    func() (string, error) { return "", nil },
		Agent:   agent,
		Master:  fakeMaster{pid: 4321},
		Ports:   fakeForwards{lines: []string{"127.0.0.1:8080->box:8080"}},
		Service: fakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
		Config:  config.New(t.TempDir()),
		Hub:     hub.New(),
	})
}

func TestHandleVersion(t *testing.T) {
	s := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got VersionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := VersionInfo{Version: "9.9", GitSHA: "deadbeef", ProtoVersion: 3}
	if got != want {
		t.Errorf("version = %+v, want %+v", got, want)
	}
}

func TestHandleOpenAPI(t *testing.T) {
	s := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/openapi.yaml", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if !bytesEqual(rec.Body.Bytes(), openapiSpec) {
		t.Error("body does not match embedded openapi.yaml bytes")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHandleStatus_AgentPresent(t *testing.T) {
	agent := &fakeAgent{
		ack:   &protocol.HelloAck{AgentPID: 777, AgentGitSHA: "abc123", Kernel: "6.1", BootID: "boot-1"},
		seq:   5,
		ports: []uint16{5000, 6000},
		ok:    true,
	}
	s := newTestServer(t, agent)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agent == nil {
		t.Fatal("Agent is nil, want non-nil after handshake")
	}
	if got.Agent.Pid != 777 || got.Agent.SHA != "abc123" {
		t.Errorf("Agent = %+v, want Pid=777 SHA=abc123", *got.Agent)
	}
	if len(got.Ports) != 2 || got.Ports[0].Port != 5000 {
		t.Errorf("Ports = %+v, want [5000 6000]", got.Ports)
	}
	if !got.Master.Up || got.Master.Pid != 4321 {
		t.Errorf("Master = %+v, want Up pid=4321", got.Master)
	}
	if len(got.Forwards) != 1 || got.Forwards[0].Name != "127.0.0.1:8080->box:8080" {
		t.Errorf("Forwards = %+v", got.Forwards)
	}
}

func TestHandleStatus_NoAgent(t *testing.T) {
	s := newTestServer(t, &fakeAgent{ok: false})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	s.mux.ServeHTTP(rec, req)

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agent != nil {
		t.Errorf("Agent = %+v, want nil without a handshake", *got.Agent)
	}
	// Default feature gates are surfaced even without an agent.
	for _, f := range []string{config.FeatureClipImage, config.FeatureClipText, config.FeatureNotify} {
		if _, ok := got.Features[f]; !ok {
			t.Errorf("feature %q missing from status", f)
		}
	}
}
