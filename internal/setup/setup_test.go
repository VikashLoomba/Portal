package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

type recordTransport struct {
	calls     []string
	out       map[string]string
	err       map[string]error
	ensureErr error
	onExec    func(string) error
}

func (r *recordTransport) Ensure(context.Context) (bool, error) {
	r.calls = append(r.calls, "ensure")
	return true, r.ensureErr
}

func (r *recordTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1}, nil
}

func (r *recordTransport) Exec(ctx context.Context, _ []byte, argv ...string) (string, string, error) {
	joined := strings.Join(argv, " ")
	r.calls = append(r.calls, "exec:"+joined)
	if r.onExec != nil {
		if err := r.onExec(joined); err != nil {
			return "", "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	for match, err := range r.err {
		if strings.Contains(joined, match) {
			return "", "", err
		}
	}
	for match, out := range r.out {
		if strings.Contains(joined, match) {
			return out, "", nil
		}
	}
	return "", "", nil
}

func (r *recordTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}

func (r *recordTransport) Close(context.Context) (bool, error) {
	r.calls = append(r.calls, "close")
	return true, nil
}

func (r *recordTransport) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplSystemSSH, Host: "box", Endpoint: "/tmp/setup-cm.sock"}
}

type fakeValidator struct {
	err    error
	hasSS  bool
	stderr string
}

func (v *fakeValidator) Validate(_ context.Context, _ string, stderrW io.Writer) error {
	if v.stderr != "" {
		_, _ = io.WriteString(stderrW, v.stderr)
	}
	return v.err
}

func (v *fakeValidator) HasSS(context.Context, string) bool { return v.hasSS }

func testRunner(t *testing.T) (*Runner, *recordTransport, *[]api.SetupEvent) {
	t.Helper()
	dir := t.TempDir()
	paths := app.Paths{
		ConfigDir: filepath.Join(dir, "config"),
		BinDir:    filepath.Join(dir, "bin"),
		Log:       filepath.Join(dir, "logs", "portal.log"),
		Sock:      filepath.Join(dir, "shared.sock"),
	}
	cfg := config.New(paths.ConfigDir)
	events := &[]api.SetupEvent{}
	r := New(paths, cfg, func(ev api.SetupEvent) { *events = append(*events, ev) })
	tr := &recordTransport{out: map[string]string{
		"grep -qF \"Installed by portal\" ~/.local/bin/xdg-open": "ok",
		"echo current || echo stale":                             "current",
	}}
	r.newTransport = func(context.Context, string) (transport.Transport, error) { return tr, nil }
	return r, tr, events
}

func TestNormalizeHost(t *testing.T) {
	if got := NormalizeHost(" user @\tdev\nbox "); got != "user@devbox" {
		t.Fatalf("NormalizeHost = %q, want user@devbox", got)
	}
}

func TestValidateEventsAndForce(t *testing.T) {
	tests := []struct {
		name       string
		validator  *fakeValidator
		force      bool
		want       bool
		wantStatus string
		wantLine   bool
		wantError  bool
	}{
		{name: "reachable", validator: &fakeValidator{hasSS: true, stderr: "auth url\n"}, want: true, wantStatus: "ok", wantLine: true},
		{name: "missing ss", validator: &fakeValidator{hasSS: false}, want: true, wantStatus: "warn"},
		{name: "unreachable", validator: &fakeValidator{err: errors.New("no route")}, wantStatus: "fail", wantError: true},
		{name: "forced", validator: &fakeValidator{err: errors.New("no route")}, force: true, want: true, wantStatus: "warn", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, events := testRunner(t)
			r.newValidator = func() validator { return tt.validator }
			if got := r.Validate(context.Background(), "box", tt.force); got != tt.want {
				t.Fatalf("Validate = %v, want %v", got, tt.want)
			}
			got := *events
			if got[0].Step != "validate" || got[0].Status != "running" {
				t.Fatalf("first event = %#v", got[0])
			}
			terminal := got[len(got)-1]
			if terminal.Status != tt.wantStatus {
				t.Fatalf("terminal status = %q, want %q", terminal.Status, tt.wantStatus)
			}
			if (terminal.Error != nil) != tt.wantError {
				t.Fatalf("terminal error = %#v, wantError %v", terminal.Error, tt.wantError)
			}
			if tt.wantLine {
				if len(got) != 3 || got[1].Line != "auth url\n" || got[1].Status != "running" {
					t.Fatalf("stderr relay events = %#v", got)
				}
			}
		})
	}
}

func TestConfigureWritesHostAndDirectories(t *testing.T) {
	r, _, events := testRunner(t)
	if err := r.Configure(context.Background(), "user@box"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	for _, dir := range []string{r.paths.ConfigDir, r.paths.BinDir, filepath.Dir(r.paths.Log)} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("directory %s not created: %v", dir, err)
		}
	}
	if got, err := r.cfg.ReadHost(); err != nil || got != "user@box" {
		t.Fatalf("ReadHost = %q, %v", got, err)
	}
	assertEventStatuses(t, *events, "configure:running", "configure:ok")
}

func TestConfigureWriteHostFailureEmitsFail(t *testing.T) {
	r, _, events := testRunner(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.cfg = config.New(blocked)
	if err := r.Configure(context.Background(), "box"); err == nil {
		t.Fatal("Configure error = nil")
	}
	assertEventStatuses(t, *events, "configure:running", "configure:fail")
	if (*events)[1].Error == nil {
		t.Fatal("configure fail missing error detail")
	}
}

func TestDeployAndVerifyOrderCachesEstablishedTransport(t *testing.T) {
	r, tr, events := testRunner(t)
	doctorCalled := false
	r.doctor = func(_ context.Context, host string, got transport.Transport) *doctor.Report {
		doctorCalled = true
		if got != tr {
			t.Fatal("doctor received a different transport")
		}
		tr.calls = append(tr.calls, "doctor")
		return &doctor.Report{Host: host, Checks: []doctor.Check{{Name: "probe", Status: doctor.Fail}}}
	}

	r.DeployRemote(context.Background(), "box")
	rep := r.Verify(context.Background(), "box")
	if !doctorCalled || rep.OK() {
		t.Fatalf("Verify report = %#v, doctorCalled=%v", rep, doctorCalled)
	}
	if len(tr.calls) == 0 || tr.calls[0] != "ensure" {
		t.Fatalf("transport calls start %v, want ensure first", tr.calls)
	}
	if got := countCall(tr.calls, "ensure"); got != 1 {
		t.Fatalf("Ensure calls = %d, want 1", got)
	}
	if got := countCall(tr.calls, "doctor"); got != 1 {
		t.Fatalf("doctor calls = %d, want 1", got)
	}
	assertTerminalStepOrder(t, *events, "xdg-open", "clip-shims", "agent-symlink", "doctor")
	doctorEvent := (*events)[len(*events)-1]
	var decoded doctor.Report
	if err := json.Unmarshal(doctorEvent.Report, &decoded); err != nil || decoded.Host != "box" {
		t.Fatalf("doctor event report = %s, %v", doctorEvent.Report, err)
	}
}

func TestDeployConstructionFailureWarnsEveryStep(t *testing.T) {
	r, _, events := testRunner(t)
	boom := errors.New("transport construction failed")
	r.newTransport = func(context.Context, string) (transport.Transport, error) { return nil, boom }
	r.DeployRemote(context.Background(), "box")
	assertEventStatuses(t, *events,
		"xdg-open:running", "xdg-open:warn",
		"clip-shims:running", "clip-shims:warn",
		"agent-symlink:running", "agent-symlink:warn")
	for i := 1; i < len(*events); i += 2 {
		if (*events)[i].Error == nil || (*events)[i].Error.Message != boom.Error() {
			t.Fatalf("event %d error = %#v", i, (*events)[i].Error)
		}
	}
}

func TestDeployEstablishmentFailureWarnsEveryStepWithoutExec(t *testing.T) {
	r, tr, events := testRunner(t)
	tr.ensureErr = errors.New("dial failed")
	r.DeployRemote(context.Background(), "box")
	assertEventStatuses(t, *events,
		"xdg-open:running", "xdg-open:warn",
		"clip-shims:running", "clip-shims:warn",
		"agent-symlink:running", "agent-symlink:warn")
	if got := countCallPrefix(tr.calls, "exec:"); got != 0 {
		t.Fatalf("Exec calls = %d, want 0; calls=%v", got, tr.calls)
	}
}

func TestDeployCancellationDuringTransportSetupEmitsNothing(t *testing.T) {
	r, tr, events := testRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.newTransport = func(ctx context.Context, _ string) (transport.Transport, error) {
		return nil, ctx.Err()
	}
	r.DeployRemote(ctx, "box")
	if len(*events) != 0 || len(tr.calls) != 0 {
		t.Fatalf("events=%#v calls=%v, want none", *events, tr.calls)
	}
}

func TestDeployCancellationDuringEnsureEmitsNothing(t *testing.T) {
	r, tr, events := testRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	tr.ensureErr = context.Canceled
	tr.onExec = func(string) error {
		t.Fatal("Exec called after canceled Ensure")
		return nil
	}
	r.newTransport = func(context.Context, string) (transport.Transport, error) {
		cancel()
		return tr, nil
	}
	r.DeployRemote(ctx, "box")
	if len(*events) != 0 || countCallPrefix(tr.calls, "exec:") != 0 {
		t.Fatalf("events=%#v calls=%v, want no deploy work", *events, tr.calls)
	}
}

func TestDeployCancellationDuringXDGStopsLaterSteps(t *testing.T) {
	r, tr, events := testRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := true
	tr.onExec = func(string) error {
		if first {
			first = false
			cancel()
			return ctx.Err()
		}
		return nil
	}
	r.DeployRemote(ctx, "box")
	assertEventStatuses(t, *events, "xdg-open:running", "xdg-open:warn")
	if got := countCallPrefix(tr.calls, "exec:"); got != 1 {
		t.Fatalf("Exec calls = %d, want 1; calls=%v", got, tr.calls)
	}
}

func TestAgentSymlinkObservesExecFailure(t *testing.T) {
	r, tr, events := testRunner(t)
	tr.err = map[string]error{"ln -sf": errors.New("permission denied")}
	r.DeployRemote(context.Background(), "box")
	var terminal *api.SetupEvent
	for i := range *events {
		if (*events)[i].Step == "agent-symlink" && (*events)[i].Status != "running" {
			terminal = &(*events)[i]
		}
	}
	if terminal == nil || terminal.Status != "warn" || terminal.Error == nil {
		t.Fatalf("agent terminal = %#v", terminal)
	}
	for _, call := range tr.calls {
		if strings.Contains(call, "ln -sf") && strings.Contains(call, "2>/dev/null || true") {
			t.Fatalf("symlink error is swallowed in %q", call)
		}
	}
}

func TestDedicatedSocketAndCloseAreIdempotent(t *testing.T) {
	r, tr, _ := testRunner(t)
	want := filepath.Join(r.paths.ConfigDir, "setup-cm.sock")
	if r.setupSock != want || r.setupSock == r.paths.Sock {
		t.Fatalf("setupSock = %q, shared = %q, want %q", r.setupSock, r.paths.Sock, want)
	}
	defaultTr, err := r.defaultNewTransport(context.Background(), "box")
	if err != nil {
		t.Fatalf("defaultNewTransport: %v", err)
	}
	if got := defaultTr.Describe().Endpoint; got != want {
		t.Fatalf("default transport endpoint = %q, want %q", got, want)
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.tr = tr
	r.Close(context.Background())
	r.Close(context.Background())
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("setup socket still exists: %v", err)
	}
	if got := countCall(tr.calls, "close"); got != 1 {
		t.Fatalf("Close calls = %d, want 1", got)
	}
}

func assertEventStatuses(t *testing.T, events []api.SetupEvent, want ...string) {
	t.Helper()
	got := make([]string, len(events))
	for i, ev := range events {
		got[i] = ev.Step + ":" + ev.Status
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func assertTerminalStepOrder(t *testing.T, events []api.SetupEvent, want ...string) {
	t.Helper()
	var got []string
	for _, ev := range events {
		if ev.Status != "running" {
			got = append(got, ev.Step)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("terminal step order = %v, want %v", got, want)
	}
}

func countCall(calls []string, want string) int {
	n := 0
	for _, call := range calls {
		if call == want {
			n++
		}
	}
	return n
}

func countCallPrefix(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}
