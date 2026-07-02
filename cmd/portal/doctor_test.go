package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipshim"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/doctor"
)

// doctorFakeTransport implements sshctl.Transport for the doctor tests. It
// scripts ssh-exec replies by matching a substring of the script the doctor
// passes to Exec (the doctor builds distinct scripts per probe), so a test can
// simulate any combination of "shim wins / real wins / missing", "portald
// present / absent", and "clipboard has content / empty".
type doctorFakeTransport struct {
	pid int
	// execReply maps a substring that must appear in the Exec script to the
	// stdout to return. The FIRST matching key (in matchOrder) wins, so order
	// disambiguates overlapping scripts.
	execReply  map[string]string
	matchOrder []string
}

func (f *doctorFakeTransport) MasterPID(ctx context.Context) (int, error) { return f.pid, nil }
func (f *doctorFakeTransport) EnsureMaster(ctx context.Context) (int, bool, error) {
	return f.pid, false, nil
}
func (f *doctorFakeTransport) Forward(ctx context.Context, l, r int) error { return nil }
func (f *doctorFakeTransport) Cancel(ctx context.Context, l, r int) error  { return nil }
func (f *doctorFakeTransport) Exit(ctx context.Context) (bool, error)      { return true, nil }
func (f *doctorFakeTransport) Host() string                                { return "fakehost" }
func (f *doctorFakeTransport) Sock() string                                { return "/tmp/sock-fake" }
func (f *doctorFakeTransport) ExecBytes(_ context.Context, _ []byte, _ ...string) (string, string, error) {
	return "", "", nil
}
func (f *doctorFakeTransport) ExecStream(_ context.Context, _ ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}

func (f *doctorFakeTransport) Exec(_ context.Context, _ string, argv ...string) (string, error) {
	joined := strings.Join(argv, " ")
	for _, key := range f.matchOrder {
		if strings.Contains(joined, key) {
			return f.execReply[key], nil
		}
	}
	return "", nil
}

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
