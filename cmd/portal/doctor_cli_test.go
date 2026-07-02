package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipshim"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/doctor"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localapi"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/run"
)

// serveDoctorDaemon starts a real localapi.Server on a temp /tmp unix socket
// whose POST /v1/doctor closure returns rep. It reuses the package-main fakes
// from daemon_test.go for the deps the doctor endpoint does not touch, waits for
// the socket to answer, registers teardown, and returns the socket path. A /tmp
// dir keeps the path under the ~104-char sun_path limit macOS enforces.
func serveDoctorDaemon(t *testing.T, cfg *config.Store, rep *doctor.Report) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "portal-doctor-api-")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")

	deps := localapi.Deps{
		Version: localapi.VersionInfo{Version: "test", GitSHA: "deadbeef", ProtoVersion: protocol.ProtoVersion},
		Host:    cfg.ReadHost,
		Agent:   &fakeAgentSource{},
		Master:  &fakeMasterProber{pid: 4242},
		Ports:   &fakeForwardLister{},
		Service: &fakeServiceStater{},
		Config:  cfg,
		Hub:     hub.New(),
		Doctor:  func(context.Context) *doctor.Report { return rep },
	}

	ln, err := localapi.Listen(path)
	if err != nil {
		t.Fatalf("localapi.Listen: %v", err)
	}
	srv := localapi.New(deps)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() { cancel(); <-done })

	lc := localclient.New(path)
	deadline := time.Now().Add(3 * time.Second)
	for !lc.Available(context.Background()) {
		if time.Now().After(deadline) {
			t.Fatal("doctor daemon did not come up")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return path
}

// TestRunDoctorCmd_DaemonUp_Pass drives the daemon-up branch: a canned all-PASS
// *doctor.Report served over the socket must render byte-identically to
// renderDoctor and return nil. This genuinely exercises the socket path — the
// report is JSON-encoded by the handler and decoded by lc.Doctor via u1's
// doctor.Status.UnmarshalJSON, so a matching render proves the round trip.
func TestRunDoctorCmd_DaemonUp_Pass(t *testing.T) {
	cfg := newTestConfig(t, "fakehost")
	canned := &doctor.Report{Host: "devbox"}
	canned.Add("ssh master", doctor.Pass, "UP (pid=4242)")
	canned.Add("PATH winner: xclip", doctor.Pass, "/home/u/.local/bin/xclip (portal shim)")
	canned.Add("agent verb: clip", doctor.Pass, "")
	canned.Add("smoke: clip targets", doctor.Pass, "Mac clipboard served (image/png)")

	sock := serveDoctorDaemon(t, cfg, canned)
	a := newDaemonTestApp(t, sock, cfg)

	var out bytes.Buffer
	if err := runDoctorCmd(context.Background(), &out, a); err != nil {
		t.Fatalf("runDoctorCmd returned %v, want nil", err)
	}
	if got, want := out.String(), reportString(canned); got != want {
		t.Errorf("daemon-up render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRunDoctorCmd_DaemonUp_Fail: a FAIL report over the socket still yields
// errSilent (so the exit code matches today) and the FAIL trailer is rendered.
func TestRunDoctorCmd_DaemonUp_Fail(t *testing.T) {
	cfg := newTestConfig(t, "fakehost")
	canned := &doctor.Report{Host: "devbox"}
	canned.Add("ssh master", doctor.Fail, "DOWN — start the daemon: "+app.Tool+" start")

	sock := serveDoctorDaemon(t, cfg, canned)
	a := newDaemonTestApp(t, sock, cfg)

	var out bytes.Buffer
	err := runDoctorCmd(context.Background(), &out, a)
	if !errors.Is(err, errSilent) {
		t.Fatalf("runDoctorCmd returned %v, want errSilent", err)
	}
	const trailer = "then re-run `portal doctor`.\n"
	if !strings.HasSuffix(out.String(), trailer) {
		t.Errorf("FAIL report should end with the FAIL trailer, got:\n%s", out.String())
	}
}

// TestRunDoctor_LocalRenderAllGreen validates the fallback's runDoctor+
// renderDoctor path directly (reviewer option (a)): the fresh-sshctl fallback
// builds its own transport from a.Runner, so it cannot be driven to an all-green
// PASS by an injected transport. Instead we call runDoctor with a
// doctorFakeTransport scripting the exact all-green run from
// TestRunDoctor_AllGreen and assert the rendered block and rep.OK().
func TestRunDoctor_LocalRenderAllGreen(t *testing.T) {
	tr := &doctorFakeTransport{
		pid: 4242,
		matchOrder: []string{
			"command -v xclip",
			"command -v wl-paste",
			"line=$(grep -F",
			"PORTALD_OK",
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
	want := "portal doctor — fakehost\n" +
		"  [PASS] ssh master: UP (pid=4242)\n" +
		"  [PASS] PATH winner: xclip: /home/u/.local/bin/xclip (portal shim)\n" +
		"  [PASS] PATH winner: wl-paste: /home/u/.local/bin/wl-paste (portal shim)\n" +
		fmt.Sprintf("  [PASS] shim version: v%s (current)\n", clipshim.Version) +
		"  [PASS] portald binary: ~/.cache/portal/portald present + executable\n" +
		"  [PASS] agent verb: clip\n" +
		"  [PASS] agent verb: notify\n" +
		"  [PASS] smoke: clip targets: Mac clipboard served (image/png)\n" +
		"\nRESULT: PASS — clipboard paste should work over plain ssh.\n"
	if got := reportString(rep); got != want {
		t.Errorf("local all-green render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRunDoctorCmd_FallbackSelection is EC2: the socket errors (nonexistent
// APISock) so lc.Doctor fails and runDoctorCmd takes the in-process fallback.
// The fallback builds a FRESH sshctl transport from a.Runner (never a.Transport)
// — proving that a.Runner is the fallback seam: an empty run.Fake makes
// `ssh -O check` match no "Master running (pid=N)" line, so MasterPID returns 0
// and the doctor renders an all-DOWN FAIL. Nothing is written to os.Stderr.
func TestRunDoctorCmd_FallbackSelection(t *testing.T) {
	cfg := newTestConfig(t, "fakehost")
	a := &app.App{
		Paths: app.Paths{
			// A path that cannot be dialed: lc.Doctor errors → fallback taken.
			APISock: filepath.Join(t.TempDir(), "does-not-exist", "api.sock"),
			Sock:    "/tmp/cm-nonexistent.sock",
		},
		Cfg: cfg,
		// Empty Default reply: `ssh -O check` returns no pid line → master DOWN.
		Runner: &run.Fake{},
	}

	// Capture os.Stderr to prove the fallback leaks no ssh stderr (the fresh
	// sshctl transport is constructed with no StderrSink).
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	var out bytes.Buffer
	runErr := runDoctorCmd(context.Background(), &out, a)

	os.Stderr = origStderr
	_ = w.Close()
	var errBuf bytes.Buffer
	if _, err := errBuf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	_ = r.Close()

	if !errors.Is(runErr, errSilent) {
		t.Fatalf("runDoctorCmd returned %v, want errSilent (master DOWN → FAIL)", runErr)
	}
	if s := out.String(); !strings.Contains(s, "[FAIL] ssh master: DOWN") {
		t.Errorf("fallback report should render a master-DOWN FAIL, got:\n%s", s)
	}
	if errBuf.Len() != 0 {
		t.Errorf("runDoctorCmd wrote to os.Stderr: %q", errBuf.String())
	}
}
