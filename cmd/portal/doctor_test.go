package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/clipshim"
	"github.com/VikashLoomba/Portal/internal/doctor"
	"github.com/VikashLoomba/Portal/internal/run"
	"github.com/VikashLoomba/Portal/internal/sshnative"
	"github.com/VikashLoomba/Portal/internal/transport"
)

// doctorFakeTransport implements transport.Transport for the doctor tests. It
// scripts ssh-exec replies by matching a substring of the script the doctor
// passes to Exec (the doctor builds distinct scripts per probe), so a test can
// simulate any combination of "shim wins / real wins / missing", "portald
// present / absent", and "clipboard has content / empty".
type doctorFakeTransport struct {
	pid int
	// forceUp reports Health.Up=true even when pid==0 — the native-transport
	// shape (a pidless connection). Left false, Health gates on pid>0 as before.
	forceUp bool
	// impl overrides Describe().Impl; empty means the default "system-ssh" so the
	// existing byte-compat fixtures are unaffected.
	impl string
	// needsEnsure models the native shape: a freshly-built native client has not
	// dialed, so Health reports DOWN until Ensure connects it. runDoctor MUST
	// Ensure before the Health gate or a healthy native box wrongly fails the
	// master check. ensured records whether runDoctor called Ensure.
	needsEnsure bool
	ensured     bool
	// execReply maps a substring that must appear in the Exec script to the
	// stdout to return. The FIRST matching key (in matchOrder) wins, so order
	// disambiguates overlapping scripts.
	execReply  map[string]string
	matchOrder []string
}

func (f *doctorFakeTransport) Ensure(context.Context) (bool, error) {
	f.ensured = true
	return true, nil
}
func (f *doctorFakeTransport) Health(context.Context) (transport.Health, error) {
	if f.needsEnsure && !f.ensured {
		return transport.Health{Up: false}, nil
	}
	if f.forceUp {
		return transport.Health{Up: true, Pid: f.pid, Detail: fmt.Sprintf("pid=%d", f.pid)}, nil
	}
	if f.pid <= 0 {
		return transport.Health{Up: false}, nil
	}
	return transport.Health{Up: true, Pid: f.pid, Detail: fmt.Sprintf("pid=%d", f.pid)}, nil
}
func (f *doctorFakeTransport) Close(context.Context) (bool, error) { return true, nil }
func (f *doctorFakeTransport) Describe() transport.Desc {
	impl := f.impl
	if impl == "" {
		impl = "system-ssh"
	}
	return transport.Desc{Impl: impl, Host: "fakehost", Endpoint: "/tmp/sock-fake"}
}
func (f *doctorFakeTransport) Stream(_ context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}

func (f *doctorFakeTransport) Exec(_ context.Context, _ []byte, argv ...string) (string, string, error) {
	joined := strings.Join(argv, " ")
	for _, key := range f.matchOrder {
		if strings.Contains(joined, key) {
			return f.execReply[key], "", nil
		}
	}
	return "", "", nil
}

var _ transport.Transport = (*doctorFakeTransport)(nil)

// TestRunDoctor_AllGreen exercises the happy path: master up, both shims win
// PATH, current shim version, portald present advertising both verbs, and a
// clipboard with content served by the smoke probe.
func TestRunDoctor_AllGreen(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 4242,
		// Keys are substrings unique to each doctor probe's script. Order so that
		// the most specific probes match before the generic ones.
		matchOrder: []string{
			"command -v xclip",
			"command -v wl-paste",
			"line=$(grep -F", // shim version
			"PORTALD_OK",     // verb probe (echoes PORTALD_OK on success)
			"clip targets xclip; echo",
		},
		execReply: map[string]string{
			"command -v xclip":         "SHIM /home/u/.local/bin/xclip",
			"command -v wl-paste":      "SHIM /home/u/.local/bin/wl-paste",
			"line=$(grep -F":           clipshim.Version + ". Intercepts",
			"PORTALD_OK":               "PORTALD_OK\nCLIP_OK\nNOTIFY_OK\n",
			"clip targets xclip; echo": "image/png\nEXIT=0",
		},
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if !rep.OK() {
		t.Fatalf("expected PASS, got report:\n%s", reportString(rep))
	}
	// Spot-check the make-or-break PATH-winner lines passed.
	assertCheck(t, rep, "PATH winner: xclip", doctor.Pass)
	assertCheck(t, rep, "PATH winner: wl-paste", doctor.Pass)
	assertCheck(t, rep, "shim version", doctor.Pass)
	assertCheck(t, rep, "agent verb: clip", doctor.Pass)
	assertCheck(t, rep, "agent verb: notify", doctor.Pass)
}

// TestRunDoctor_EnsuresBeforeHealthGate pins finding 4: runDoctor must Ensure
// the transport before the step-1 Health check. A freshly-built native client
// reports Health.Up=false until it dials (New never dials), so the two direct
// callers (install self-test, daemon-down fallback) would render a flatly-wrong
// "ssh master DOWN" on a healthy native box without this Ensure. The fake here
// is DOWN until Ensure is called, exactly like an undialed native client.
func TestRunDoctor_EnsuresBeforeHealthGate(t *testing.T) {
	order, reply := greenReplies()
	tr := &doctorFakeTransport{
		needsEnsure: true, // DOWN until Ensure dials (native shape)
		forceUp:     true, // after Ensure: Up with pid 0
		pid:         0,
		impl:        "native-ssh",
		matchOrder:  order,
		execReply:   reply,
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if !tr.ensured {
		t.Fatal("runDoctor must call Ensure before the Health gate (native needs a dial to become healthy)")
	}
	assertCheck(t, rep, "ssh master", doctor.Pass)
	if !rep.OK() {
		t.Fatalf("expected PASS after Ensure brought the native transport up, got:\n%s", reportString(rep))
	}
}

// TestRunDoctor_MasterDown bails after the master check fails — no remote probe
// can run without the ControlMaster.
func TestRunDoctor_MasterDown(t *testing.T) {
	tr := &doctorFakeTransport{pid: 0}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if rep.OK() {
		t.Fatal("expected FAIL when master is down")
	}
	assertCheck(t, rep, "ssh master", doctor.Fail)
	// No further probes should have been attempted.
	if len(rep.Checks) != 1 {
		t.Errorf("expected only the master check, got %d checks", len(rep.Checks))
	}
}

// TestRunDoctor_SystemNeverEnsures pins the finding: for the REAL system
// transport, sshctl.Ensure is not a passive dial-check — it removes the stale
// socket and SPAWNS a persistent ControlMaster. runDoctor must therefore NEVER
// call Ensure on a system transport, or a daemon-down `portal doctor` (fresh
// boot / post-`portal stop`) would spawn an orphan master, flip the master check
// false-green, and hide that the relay daemon is not running.
//
// This fake models that spawn: needsEnsure makes Health report DOWN until Ensure
// is called, exactly as a would-be-spawned master would flip it UP. With the fix
// runDoctor leaves the system transport untouched, so Health stays DOWN, the
// master check is FAIL, and Ensure was never invoked. The pre-fix code called
// Ensure unconditionally, which would have flipped this report to false-green.
func TestRunDoctor_SystemNeverEnsures(t *testing.T) {
	tr := &doctorFakeTransport{
		needsEnsure: true, // Health is DOWN unless Ensure spawns a master
		forceUp:     true, // if Ensure WERE (wrongly) called, Health would go UP
		pid:         0,
		// impl defaults to system-ssh
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if tr.ensured {
		t.Fatal("runDoctor must NOT call Ensure on a system transport (would spawn an orphan ControlMaster)")
	}
	if rep.OK() {
		t.Fatal("expected FAIL: a daemon-down system doctor must report the master DOWN, not spawn one")
	}
	assertCheck(t, rep, "ssh master", doctor.Fail)
	c := findCheck(rep, "ssh master")
	if c == nil || !strings.Contains(c.Detail, "start the daemon") {
		t.Errorf("system master-down detail must keep the byte-compat `start the daemon` diagnostic, got %q", detailOf(c))
	}
}

// TestRunDoctor_RealBinaryWinsPATH is the single make-or-break regression: a
// real /usr/bin/xclip resolves ahead of the shim, so the feature is silently
// dead. The doctor MUST flag this as FAIL, not pass it off.
func TestRunDoctor_RealBinaryWinsPATH(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 1,
		matchOrder: []string{
			"command -v xclip", "command -v wl-paste",
			"line=$(grep -F", "PORTALD_OK", "clip targets xclip; echo",
		},
		execReply: map[string]string{
			"command -v xclip":         "REAL /usr/bin/xclip", // real binary wins!
			"command -v wl-paste":      "SHIM /home/u/.local/bin/wl-paste",
			"line=$(grep -F":           clipshim.Version,
			"PORTALD_OK":               "PORTALD_OK\nCLIP_OK\nNOTIFY_OK\n",
			"clip targets xclip; echo": "EXIT=1",
		},
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if rep.OK() {
		t.Fatal("expected FAIL when a real binary wins PATH ahead of the shim")
	}
	assertCheck(t, rep, "PATH winner: xclip", doctor.Fail)
	// The detail must name the cause so the user can fix it.
	c := findCheck(rep, "PATH winner: xclip")
	if c == nil || !strings.Contains(c.Detail, "real binary wins") {
		t.Errorf("xclip FAIL detail should name the real-binary-wins cause, got %q", detailOf(c))
	}
}

// TestRunDoctor_NoShimResolves covers ~/.local/bin not on PATH (or the shim
// never deployed): nothing resolves for the tool. That is a FAIL.
func TestRunDoctor_NoShimResolves(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 1,
		matchOrder: []string{
			"command -v xclip", "command -v wl-paste",
			"line=$(grep -F", "PORTALD_OK", "clip targets xclip; echo",
		},
		execReply: map[string]string{
			"command -v xclip":         "NONE",
			"command -v wl-paste":      "NONE",
			"line=$(grep -F":           clipshim.Version,
			"PORTALD_OK":               "PORTALD_OK\nCLIP_OK\nNOTIFY_OK\n",
			"clip targets xclip; echo": "EXIT=1",
		},
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if rep.OK() {
		t.Fatal("expected FAIL when no shim resolves")
	}
	assertCheck(t, rep, "PATH winner: xclip", doctor.Fail)
}

// TestRunDoctor_PortaldMissing: the agent binary isn't uploaded yet (dangling
// symlink window). portald is a FAIL; the smoke probe is skipped.
func TestRunDoctor_PortaldMissing(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 1,
		matchOrder: []string{
			"command -v xclip", "command -v wl-paste",
			"line=$(grep -F", "PORTALD_OK", "clip targets xclip; echo",
		},
		execReply: map[string]string{
			"command -v xclip":    "SHIM /home/u/.local/bin/xclip",
			"command -v wl-paste": "SHIM /home/u/.local/bin/wl-paste",
			"line=$(grep -F":      clipshim.Version,
			"PORTALD_OK":          "NO_PORTALD", // no PORTALD_OK token
		},
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if rep.OK() {
		t.Fatal("expected FAIL when portald is missing")
	}
	assertCheck(t, rep, "portald binary", doctor.Fail)
	if findCheck(rep, "smoke: clip targets") != nil {
		t.Error("smoke probe should be skipped when portald is missing")
	}
}

// TestRunDoctor_EmptyClipboardSmoke: everything is wired but nothing is on the
// Mac clipboard, so the smoke probe exits 1. That is the EXPECTED clean
// fall-through state — a WARN, not a FAIL.
func TestRunDoctor_EmptyClipboardSmoke(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 1,
		matchOrder: []string{
			"command -v xclip", "command -v wl-paste",
			"line=$(grep -F", "PORTALD_OK", "clip targets xclip; echo",
		},
		execReply: map[string]string{
			"command -v xclip":         "SHIM /home/u/.local/bin/xclip",
			"command -v wl-paste":      "SHIM /home/u/.local/bin/wl-paste",
			"line=$(grep -F":           clipshim.Version,
			"PORTALD_OK":               "PORTALD_OK\nCLIP_OK\nNOTIFY_OK\n",
			"clip targets xclip; echo": "EXIT=1",
		},
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if !rep.OK() {
		t.Fatalf("expected PASS (empty clipboard is a clean WARN), got:\n%s", reportString(rep))
	}
	assertCheck(t, rep, "smoke: clip targets", doctor.Warn)
}

// TestRunDoctor_ShimVersionDrift: a deployed shim older than embedded is a WARN
// (still usable), not a FAIL.
func TestRunDoctor_ShimVersionDrift(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 1,
		matchOrder: []string{
			"command -v xclip", "command -v wl-paste",
			"line=$(grep -F", "PORTALD_OK", "clip targets xclip; echo",
		},
		execReply: map[string]string{
			"command -v xclip":         "SHIM /home/u/.local/bin/xclip",
			"command -v wl-paste":      "SHIM /home/u/.local/bin/wl-paste",
			"line=$(grep -F":           "0. old marker text", // version "0"
			"PORTALD_OK":               "PORTALD_OK\nCLIP_OK\nNOTIFY_OK\n",
			"clip targets xclip; echo": "EXIT=1",
		},
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if !rep.OK() {
		t.Fatalf("version drift should be a WARN (still PASS overall), got:\n%s", reportString(rep))
	}
	assertCheck(t, rep, "shim version", doctor.Warn)
}

// TestRenderDoctor_Golden pins the CLI rendering byte-for-byte (EC7). Scripts
// parse `portal doctor` output; this guards the header, every per-check line
// shape (with and without detail, across all three tags), and both the PASS and
// FAIL trailers against silent drift after the report types moved to
// internal/doctor.
func TestRenderDoctor_Golden(t *testing.T) {
	tests := []struct {
		name string
		rep  *doctor.Report
		want string
	}{
		{
			name: "mixed_fail",
			rep: &doctor.Report{
				Host: "devbox",
				Checks: []doctor.Check{
					{Name: "ssh master", Status: doctor.Pass, Detail: "UP (pid=4242)"},
					{Name: "agent verb: clip", Status: doctor.Pass},
					{Name: "shim version", Status: doctor.Warn, Detail: "deployed=v2 embedded=v3 — re-run install to converge"},
					{Name: "PATH winner: xclip", Status: doctor.Fail, Detail: "/usr/bin/xclip (real binary wins ahead of the shim) — re-run install to fix PATH order"},
				},
			},
			want: "portal doctor — devbox\n" +
				"  [PASS] ssh master: UP (pid=4242)\n" +
				"  [PASS] agent verb: clip\n" +
				"  [WARN] shim version: deployed=v2 embedded=v3 — re-run install to converge\n" +
				"  [FAIL] PATH winner: xclip: /usr/bin/xclip (real binary wins ahead of the shim) — re-run install to fix PATH order\n" +
				"\nRESULT: FAIL — clipboard paste will NOT work. Fix the FAIL lines above\n" +
				"        (often: re-run `portal install devbox`), then re-run `portal doctor`.\n",
		},
		{
			name: "warn_only_pass",
			rep: &doctor.Report{
				Host: "box2",
				Checks: []doctor.Check{
					{Name: "ssh master", Status: doctor.Pass, Detail: "UP (pid=1)"},
					{Name: "shim version", Status: doctor.Warn, Detail: "could not read deployed shim version"},
				},
			},
			want: "portal doctor — box2\n" +
				"  [PASS] ssh master: UP (pid=1)\n" +
				"  [WARN] shim version: could not read deployed shim version\n" +
				"\nRESULT: PASS — clipboard paste should work over plain ssh.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			renderDoctor(&b, tt.rep)
			if got := b.String(); got != tt.want {
				t.Errorf("renderDoctor mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
		})
	}
}

// greenReplies returns the AllGreen matchOrder/execReply so both a system and a
// native fixture can produce an all-green report body.
func greenReplies() (order []string, reply map[string]string) {
	order = []string{
		"command -v xclip",
		"command -v wl-paste",
		"line=$(grep -F",
		"PORTALD_OK",
		"clip targets xclip; echo",
	}
	reply = map[string]string{
		"command -v xclip":         "SHIM /home/u/.local/bin/xclip",
		"command -v wl-paste":      "SHIM /home/u/.local/bin/wl-paste",
		"line=$(grep -F":           clipshim.Version + ". Intercepts",
		"PORTALD_OK":               "PORTALD_OK\nCLIP_OK\nNOTIFY_OK\n",
		"clip targets xclip; echo": "image/png\nEXIT=0",
	}
	return order, reply
}

// T8/T9: a system-Impl transport adds NEITHER a `transport` nor a `forward
// lifetime` Check — the system doctor report stays byte-identical to today.
func TestRunDoctor_SystemImpl_NoExtraChecks(t *testing.T) {
	order, reply := greenReplies()
	tr := &doctorFakeTransport{pid: 4242, matchOrder: order, execReply: reply} // impl defaults system-ssh
	rep := runDoctor(context.Background(), "fakehost", tr)
	if !rep.OK() {
		t.Fatalf("expected PASS, got:\n%s", reportString(rep))
	}
	if findCheck(rep, "transport") != nil {
		t.Error("system transport must not add a `transport` check (byte-compat)")
	}
	if findCheck(rep, "forward lifetime") != nil {
		t.Error("system transport must not add a `forward lifetime` check (byte-compat)")
	}
}

// T8/T10: a native-Impl transport with Health{Up:true,Pid:0} adds the
// `transport: native-ssh` check and the `forward lifetime` Warn note, renders
// the master line as UP (pid=0), and overall RESULT stays PASS (Warn tolerated).
func TestRunDoctor_NativeImpl_TransportAndForwardChecks(t *testing.T) {
	order, reply := greenReplies()
	tr := &doctorFakeTransport{
		forceUp:    true, // Up with Pid 0 — the native shape
		pid:        0,
		impl:       "native-ssh",
		matchOrder: order,
		execReply:  reply,
	}
	rep := runDoctor(context.Background(), "fakehost", tr)
	if !rep.OK() {
		t.Fatalf("expected PASS (Warn tolerated), got:\n%s", reportString(rep))
	}
	assertCheck(t, rep, "transport", doctor.Pass)
	if c := findCheck(rep, "transport"); c == nil || c.Detail != "native-ssh" {
		t.Errorf("transport check detail = %q, want native-ssh", detailOf(c))
	}
	assertCheck(t, rep, "forward lifetime", doctor.Warn)
	if m := findCheck(rep, "ssh master"); m == nil || m.Detail != "UP (pid=0)" {
		t.Errorf("master line detail = %q, want UP (pid=0)", detailOf(m))
	}
}

// TestRunDoctorCmd_FallbackNativeSelection: with the daemon down AND the native
// transport selected in config, the fallback transport is factory-constructed so
// the report surfaces `transport: native-ssh` (selection honored even with the
// daemon down, T8). runDoctor now Ensures before the Health gate (finding 4), so
// the native client attempts a dial; 127.0.0.1:1 refuses instantly (no DNS, no
// timeout), the master check FAILs, and we assert only that the selection
// surfaced — not overall PASS.
func TestRunDoctorCmd_FallbackNativeSelection(t *testing.T) {
	cfg := newTestConfig(t, "user@127.0.0.1:1")
	if err := cfg.SetTransport("native"); err != nil {
		t.Fatal(err)
	}
	a := &app.App{
		Paths: app.Paths{
			APISock: filepath.Join(t.TempDir(), "does-not-exist", "api.sock"),
			Sock:    "/tmp/cm-nonexistent.sock",
		},
		Cfg:    cfg,
		Runner: &run.Fake{}, // native ignores the runner; present for the factory signature.
	}

	var out bytes.Buffer
	// T5 hermeticity: inject a temp-dir known_hosts path so the native client the
	// fallback constructs never reads the runner's real ~/.ssh/known_hosts.
	hermetic := sshnative.WithKnownHostsPath(filepath.Join(t.TempDir(), "known_hosts"))
	_ = runDoctorCmd(context.Background(), &out, a, hermetic) // FAIL expected (native undialed)
	if !strings.Contains(out.String(), "[PASS] transport: native-ssh") {
		t.Errorf("daemon-down fallback must honor the native selection, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "forward lifetime") {
		t.Errorf("native fallback should include the T10 forward-lifetime note, got:\n%s", out.String())
	}
}

// TestDoctorTransport_NativeUsesLiveTransport (findings 1 & 3): on a healthy
// native daemon, the daemon-up /v1/doctor probe must run over the daemon's LIVE
// transport (a.Transport), NOT a fresh factory client — a fresh native client
// reports Health.Up=false without dialing, yielding a flatly-wrong "ssh master
// DOWN" on a supported path. doctorTransport returning a.Transport is the fix.
func TestDoctorTransport_NativeUsesLiveTransport(t *testing.T) {
	cfg := newTestConfig(t, "user@devbox")
	if err := cfg.SetTransport("native"); err != nil {
		t.Fatal(err)
	}
	live := &doctorFakeTransport{forceUp: true, pid: 0, impl: "native-ssh"}
	a := &app.App{Cfg: cfg, Transport: live, Runner: &run.Fake{}}

	tr, err := doctorTransport(context.Background(), a, "user@devbox")
	if err != nil {
		t.Fatalf("doctorTransport: %v", err)
	}
	if tr != live {
		t.Fatal("native doctor must run over the daemon's LIVE a.Transport, not a fresh factory client")
	}

	// End to end: the report over the live transport shows the master UP — the
	// exact line a fresh, undialed native client would have (wrongly) failed.
	order, reply := greenReplies()
	live.matchOrder, live.execReply = order, reply
	rep := runDoctor(context.Background(), "user@devbox", tr)
	assertCheck(t, rep, "ssh master", doctor.Pass)
	assertCheck(t, rep, "transport", doctor.Pass)
}

// TestDoctorTransport_SystemUsesFreshTransport: the system daemon-up probe must
// use a FRESH nil-sink transport (not a.Transport, whose StderrSink=os.Stderr
// would tee ssh stderr into the launchd log), while still selecting system-ssh.
func TestDoctorTransport_SystemUsesFreshTransport(t *testing.T) {
	cfg := newTestConfig(t, "user@devbox") // transport file absent -> system
	live := &doctorFakeTransport{forceUp: true, pid: 4242}
	a := &app.App{
		Cfg:       cfg,
		Transport: live,
		Runner:    &run.Fake{},
		Paths:     app.Paths{Sock: "/tmp/cm.sock"},
	}

	tr, err := doctorTransport(context.Background(), a, "user@devbox")
	if err != nil {
		t.Fatalf("doctorTransport: %v", err)
	}
	if tr == live {
		t.Fatal("system doctor must use a FRESH nil-sink transport, not a.Transport (avoids teeing ssh stderr)")
	}
	if got := tr.Describe().Impl; got != "system-ssh" {
		t.Errorf("system doctor transport Impl = %q, want system-ssh", got)
	}
}

// --- helpers ---

func findCheck(r *doctor.Report, name string) *doctor.Check {
	for i := range r.Checks {
		if r.Checks[i].Name == name {
			return &r.Checks[i]
		}
	}
	return nil
}

func assertCheck(t *testing.T, r *doctor.Report, name string, want doctor.Status) {
	t.Helper()
	c := findCheck(r, name)
	if c == nil {
		t.Errorf("missing check %q", name)
		return
	}
	if c.Status != want {
		t.Errorf("check %q: status = %s, want %s (detail=%q)", name, c.Status.Tag(), want.Tag(), c.Detail)
	}
}

func detailOf(c *doctor.Check) string {
	if c == nil {
		return ""
	}
	return c.Detail
}

func reportString(r *doctor.Report) string {
	var b strings.Builder
	renderDoctor(&b, r)
	return b.String()
}
