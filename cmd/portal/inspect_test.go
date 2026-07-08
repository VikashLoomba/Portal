package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/service"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// EC1: status sourced over the socket includes the agent line plus the master/
// forward lines from the daemon's deps.
func TestRunStatusTo_DaemonUp_AgentLine(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg,
		withHelloAck(&protocol.HelloAck{
			AgentPID:    4321,
			AgentGitSHA: "0123456789abcdef", // > 12 runes: truncation asserted below
			Kernel:      "Linux-test",
		}),
		withMasterPID(4242),
		withForwardLines([]string{"127.0.0.1:9000"}),
	)
	a := newDaemonTestApp(t, d.path, cfg)

	var buf bytes.Buffer
	if err := runStatusTo(context.Background(), &buf, a); err != nil {
		t.Fatalf("runStatusTo: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"agent: pid=4321 sha=0123456789ab kernel=Linux-test\n",
		"ssh master: UP (pid=4242) host=devbox\n",
		"active forwards (local listeners owned by master):\n",
		"  127.0.0.1:9000\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// EC4 (§5.3.4): the daemon-up `portal status` render must be byte-identical to
// today's layout apart from the agent line. Unlike TestRunStatusTo_DaemonUp_AgentLine
// (Contains-only, never inspects the service block) and TestRenderStatus_Golden
// (drives renderStatus with hand-built statusView, never viewFromStatus), this
// test drives the FULL socket seam end-to-end — GET /v1/status ->
// viewFromStatus -> renderStatus — and compares the entire buffer, so a
// viewFromStatus refactor that drops StateLines or inverts Loaded fails here.
func TestRunStatusTo_DaemonUp_ByteIdentical(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg,
		withService(service.Status{Loaded: true, StateLines: []string{"state = running"}}),
		withHelloAck(&protocol.HelloAck{
			AgentPID:    1234,
			AgentGitSHA: "abcdef123456789", // > 12 runes: truncation asserted below
			Kernel:      "Linux",
		}),
		withMasterPID(4242),
		withForwardLines([]string{"127.0.0.1:8080", "[::1]:8080"}),
	)
	a := newDaemonTestApp(t, d.path, cfg)

	var buf bytes.Buffer
	if err := runStatusTo(context.Background(), &buf, a); err != nil {
		t.Fatalf("runStatusTo: %v", err)
	}
	want := "dev box: devbox\n" +
		"service (com.test.portal):\n" +
		"state = running\n" +
		"\n" +
		"ssh master: UP (pid=4242) host=devbox\n" +
		"agent: pid=1234 sha=abcdef123456 kernel=Linux\n" +
		"active forwards (local listeners owned by master):\n" +
		"  127.0.0.1:8080\n" +
		"  [::1]:8080\n"
	if got := buf.String(); got != want {
		t.Errorf("daemon-up status not byte-identical:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// EC4: the shared renderer produces today's exact layout. Case (a) vs (b) proves
// the ONLY delta is the agent line.
func TestRenderStatus_Golden(t *testing.T) {
	const label = "com.test.portal"
	const sock = "/tmp/cm.sock"

	base := statusView{
		host:       "devbox",
		label:      label,
		hostKnown:  true,
		loaded:     true,
		stateLines: []string{"state = running"},
		masterUp:   true,
		masterPID:  4242,
		sock:       sock,
		forwards:   []string{"127.0.0.1:8080", "[::1]:8080"},
	}
	withAgent := base
	withAgent.agent = &statusAgentView{pid: 1234, sha: "abc123def456", kernel: "Linux"}

	tests := []struct {
		name string
		view statusView
		want string
	}{
		{
			name: "host_master_up_agent_forwards",
			view: withAgent,
			want: "dev box: devbox\n" +
				"service (com.test.portal):\n" +
				"state = running\n" +
				"\n" +
				"ssh master: UP (pid=4242) host=devbox\n" +
				"agent: pid=1234 sha=abc123def456 kernel=Linux\n" +
				"active forwards (local listeners owned by master):\n" +
				"  127.0.0.1:8080\n" +
				"  [::1]:8080\n",
		},
		{
			name: "same_without_agent",
			view: base,
			want: "dev box: devbox\n" +
				"service (com.test.portal):\n" +
				"state = running\n" +
				"\n" +
				"ssh master: UP (pid=4242) host=devbox\n" +
				"active forwards (local listeners owned by master):\n" +
				"  127.0.0.1:8080\n" +
				"  [::1]:8080\n",
		},
		{
			name: "master_down_early_return",
			view: statusView{
				host: "devbox", label: label, hostKnown: true,
				loaded: false, masterUp: false, sock: sock,
			},
			want: "dev box: devbox\n" +
				"service (com.test.portal):\n" +
				"  not loaded (run: portal install)\n" +
				"\n" +
				"ssh master: DOWN (host=devbox sock=/tmp/cm.sock)\n",
		},
		{
			name: "not_configured_early_return",
			view: statusView{
				label: label, hostKnown: false, loaded: false,
			},
			want: "dev box: <not configured>\n" +
				"service (com.test.portal):\n" +
				"  not loaded (run: portal install)\n" +
				"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			renderStatus(&b, tt.view)
			if got := b.String(); got != tt.want {
				t.Errorf("renderStatus mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
		})
	}
}

// wantLocalStatus is the exact viewFromLocal render for newDaemonTestApp's fakes
// (host devbox, master pid 7777, service loaded, one forward line). No agent
// line — a short-lived CLI has no in-process handshake.
const wantLocalStatus = "dev box: devbox\n" +
	"service (com.test.portal):\n" +
	"state = running\n" +
	"\n" +
	"ssh master: UP (pid=7777) host=devbox\n" +
	"active forwards (local listeners owned by master):\n" +
	"  127.0.0.1:5173\n"

// EC2: status falls back to viewFromLocal for a nonexistent socket, a plain
// file, and a hung listener — each with no agent line and no error spam.
func TestRunStatusTo_DaemonDown_Fallback(t *testing.T) {
	t.Run("nonexistent_socket", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)
		assertLocalFallback(t, context.Background(), a)
	})

	t.Run("plain_file", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		f := filepath.Join(t.TempDir(), "notasocket")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := newDaemonTestApp(t, f, cfg)
		assertLocalFallback(t, context.Background(), a)
	})

	t.Run("hung_listener", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		// A listener that never accepts: the dial succeeds, the HTTP request
		// stalls, and the short ctx (not StatusTimeout) tears it down.
		dir, err := os.MkdirTemp("/tmp", "portal-hung-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "api.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		a := newDaemonTestApp(t, sock, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		assertLocalFallback(t, ctx, a)
	})
}

// assertLocalFallback runs runStatusTo and asserts the local render byte-for-byte
// (which also proves no error text leaked into the output).
func assertLocalFallback(t *testing.T, ctx context.Context, a *app.App) {
	t.Helper()
	var buf bytes.Buffer
	if err := runStatusTo(ctx, &buf, a); err != nil {
		t.Fatalf("runStatusTo: %v", err)
	}
	if got := buf.String(); got != wantLocalStatus {
		t.Errorf("fallback render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, wantLocalStatus)
	}
	if strings.Contains(buf.String(), "agent:") {
		t.Errorf("fallback must not print an agent line, got:\n%s", buf.String())
	}
}

// EC (ports): daemon-up serves the cached Snapshot as the port list.
func TestRunPorts_DaemonUp_Snapshot(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg, withSnapshot([]uint16{8081, 9090}, true))
	a := newDaemonTestApp(t, d.path, cfg)

	var out, errb bytes.Buffer
	if err := runPorts(context.Background(), &out, &errb, a); err != nil {
		t.Fatalf("runPorts: %v", err)
	}
	want := "loopback dev ports listening on devbox (will be forwarded):\n" +
		"  8081\n" +
		"  9090\n"
	if out.String() != want {
		t.Errorf("ports output mismatch:\n--- got ---\n%s\n--- want ---\n%s", out.String(), want)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}

// EC (ports): daemon up but no cached Snapshot yet (503 not_connected) prints
// the header only — no CLI-side agent is spun up.
func TestRunPorts_DaemonUp_NotConnected(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg, withSnapshot(nil, false))
	a := newDaemonTestApp(t, d.path, cfg)

	var out, errb bytes.Buffer
	if err := runPorts(context.Background(), &out, &errb, a); err != nil {
		t.Fatalf("runPorts: %v", err)
	}
	want := "loopback dev ports listening on devbox (will be forwarded):\n"
	if out.String() != want {
		t.Errorf("header-only mismatch:\n--- got ---\n%s\n--- want ---\n%s", out.String(), want)
	}
}

// EC10c: viewFromLocal must set masterUp from Health.Up, NOT from Pid>0. Under a
// native-shaped Health ({Up:true, Pid:0}) the fallback view must still report the
// master up and fetch forwards via App.PF.ForwardLines.
func TestViewFromLocal_NativeHealthPidZero(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	pf := &recordingForwarder{lines: []string{"127.0.0.1:5173"}}
	a := &app.App{
		Cfg:       cfg,
		Paths:     app.Paths{Label: "com.test.portal", Sock: "/tmp/cm-fake.sock"},
		Transport: nativeHealthTransport{up: true, pid: 0},
		PF:        pf,
		Service:   &appFakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
	}

	v := viewFromLocal(context.Background(), a)
	if !v.masterUp {
		t.Errorf("masterUp = false, want true (gated on Health.Up, not Pid>0)")
	}
	if v.masterPID != 0 {
		t.Errorf("masterPID = %d, want 0 (native transport carries no pid)", v.masterPID)
	}
	if len(v.forwards) != 1 || v.forwards[0] != "127.0.0.1:5173" {
		t.Errorf("forwards = %v, want [127.0.0.1:5173] (via App.PF.ForwardLines)", v.forwards)
	}
}

// T8/T9: with the system transport selected the status output is byte-identical
// to today — NO `transport:` line (the impl is "system-ssh", which the render
// gate excludes). newDaemonTestApp's fake transport reports system-ssh.
func TestStatus_System_NoTransportLine(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)

	var buf bytes.Buffer
	renderStatus(&buf, viewFromLocal(context.Background(), a))
	if strings.Contains(buf.String(), "transport:") {
		t.Errorf("system status must not show a transport line, got:\n%s", buf.String())
	}
	if got := buf.String(); got != wantLocalStatus {
		t.Errorf("system status not byte-identical:\n--- got ---\n%s\n--- want ---\n%s", got, wantLocalStatus)
	}
}

// T8: with the native transport selected (Describe().Impl == native-ssh, a
// healthy Health{Up:true,Pid:0}) the status shows the `transport: native-ssh`
// line immediately after the `ssh master: UP ...` line.
func TestStatus_Native_TransportLine(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	pf := &recordingForwarder{lines: []string{"127.0.0.1:5173"}}
	a := &app.App{
		Cfg:       cfg,
		Paths:     app.Paths{Label: "com.test.portal", Sock: "/tmp/cm-fake.sock"},
		Transport: nativeHealthTransport{up: true, pid: 0},
		PF:        pf,
		Service:   &appFakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
	}

	var buf bytes.Buffer
	renderStatus(&buf, viewFromLocal(context.Background(), a))
	want := "dev box: devbox\n" +
		"service (com.test.portal):\n" +
		"state = running\n" +
		"\n" +
		"ssh master: UP (pid=0) host=devbox\n" +
		"transport: native-ssh\n" +
		"active forwards (local listeners owned by master):\n" +
		"  127.0.0.1:5173\n"
	if got := buf.String(); got != want {
		t.Errorf("native status mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// T8: with the native transport selected but DOWN (box unreachable), the status
// DOWN branch must still surface `transport: native-ssh` AND must NOT print a
// `sock=` path — native never creates the ControlMaster socket, so naming one
// falsely implies system ssh, exactly when the user needs to know which
// transport is failing. Mirrors runDoctor's unconditional transport surfacing.
func TestStatus_Native_Down_TransportLineNoSock(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := &app.App{
		Cfg:       cfg,
		Paths:     app.Paths{Label: "com.test.portal", Sock: "/tmp/cm-fake.sock"},
		Transport: nativeHealthTransport{up: false, pid: 0},
		PF:        &recordingForwarder{},
		Service:   &appFakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
	}

	var buf bytes.Buffer
	renderStatus(&buf, viewFromLocal(context.Background(), a))
	want := "dev box: devbox\n" +
		"service (com.test.portal):\n" +
		"state = running\n" +
		"\n" +
		"ssh master: DOWN (host=devbox)\n" +
		"transport: native-ssh\n"
	if got := buf.String(); got != want {
		t.Errorf("native DOWN status mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(buf.String(), "sock=") {
		t.Errorf("native DOWN status must not print a ControlMaster sock= path, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "/tmp/cm-fake.sock") {
		t.Errorf("native DOWN status leaked the fictional ControlMaster path, got:\n%s", buf.String())
	}
}

// T8/T9: with the SYSTEM transport DOWN the DOWN branch stays byte-identical to
// today — the `sock=` path is present and NO `transport:` line appears.
func TestStatus_System_Down_ByteCompat(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := &app.App{
		Cfg:       cfg,
		Paths:     app.Paths{Label: "com.test.portal", Sock: "/tmp/cm-sys.sock"},
		Transport: systemDownTransport{},
		PF:        &recordingForwarder{},
		Service:   &appFakeService{st: service.Status{Loaded: true, StateLines: []string{"state = running"}}},
	}

	var buf bytes.Buffer
	renderStatus(&buf, viewFromLocal(context.Background(), a))
	want := "dev box: devbox\n" +
		"service (com.test.portal):\n" +
		"state = running\n" +
		"\n" +
		"ssh master: DOWN (host=devbox sock=/tmp/cm-sys.sock)\n"
	if got := buf.String(); got != want {
		t.Errorf("system DOWN status not byte-identical:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(buf.String(), "transport:") {
		t.Errorf("system DOWN status must not show a transport line, got:\n%s", buf.String())
	}
}

// EC2 (ports): a bad socket falls through to the local EnsureMaster/DesiredPorts
// path.
func TestRunPorts_DaemonDown_Fallback(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)

	var out, errb bytes.Buffer
	if err := runPorts(context.Background(), &out, &errb, a); err != nil {
		t.Fatalf("runPorts: %v", err)
	}
	want := "loopback dev ports listening on devbox (will be forwarded):\n" +
		"  5173\n" +
		"  6006\n"
	if out.String() != want {
		t.Errorf("fallback ports mismatch:\n--- got ---\n%s\n--- want ---\n%s", out.String(), want)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}
