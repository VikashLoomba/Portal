package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/service"
	"github.com/VikashLoomba/Portal/internal/setup"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
)

type installFakeSetup struct {
	sink      setup.Sink
	proceed   bool
	calls     []string
	report    *doctor.Report
	clipWarn  bool
	missingSS bool
}

func (f *installFakeSetup) Validate(_ context.Context, _ string, force bool) bool {
	f.calls = append(f.calls, "validate")
	f.sink(api.SetupEvent{Step: "validate", Status: "running"})
	if f.missingSS {
		f.sink(api.SetupEvent{Step: "validate", Status: "running", Line: "WARNING: 'box' is reachable but has no 'ss' command — is it Linux? Port discovery may not work.\n"})
		f.sink(api.SetupEvent{Step: "validate", Status: "warn"})
	} else if f.proceed {
		f.sink(api.SetupEvent{Step: "validate", Status: "ok"})
	} else {
		f.sink(api.SetupEvent{Step: "validate", Status: "fail", Error: &api.ErrorDetail{Code: "validation_failed", Message: "unreachable"}})
	}
	return f.proceed
}

func (f *installFakeSetup) Configure(context.Context, string) error {
	f.calls = append(f.calls, "configure")
	f.sink(api.SetupEvent{Step: "configure", Status: "running"})
	f.sink(api.SetupEvent{Step: "configure", Status: "ok"})
	return nil
}

func (f *installFakeSetup) DeployRemote(context.Context, string) {
	f.calls = append(f.calls, "deploy")
	f.sink(api.SetupEvent{Step: "xdg-open", Status: "running"})
	f.sink(api.SetupEvent{Step: "xdg-open", Status: "ok"})
	f.sink(api.SetupEvent{Step: "clip-shims", Status: "running"})
	if f.clipWarn {
		f.sink(api.SetupEvent{Step: "clip-shims", Status: "warn", Error: &api.ErrorDetail{Code: "clip_shims_failed", Message: "shim denied"}})
	} else {
		f.sink(api.SetupEvent{Step: "clip-shims", Status: "ok"})
	}
	f.sink(api.SetupEvent{Step: "agent-symlink", Status: "running"})
	f.sink(api.SetupEvent{Step: "agent-symlink", Status: "ok"})
}

func (f *installFakeSetup) Verify(context.Context, string) *doctor.Report {
	f.calls = append(f.calls, "verify")
	f.sink(api.SetupEvent{Step: "doctor", Status: "running"})
	f.sink(api.SetupEvent{Step: "doctor", Status: "ok"})
	return f.report
}

func (f *installFakeSetup) Close(context.Context) { f.calls = append(f.calls, "close") }

type installFakeService struct{}

func (installFakeService) Install(context.Context) error          { return nil }
func (installFakeService) Uninstall(context.Context) error        { return nil }
func (installFakeService) Reload(context.Context) error           { return nil }
func (installFakeService) Start(context.Context) error            { return nil }
func (installFakeService) Stop(context.Context) error             { return nil }
func (installFakeService) Restart(context.Context) error          { return nil }
func (installFakeService) IsLoaded(context.Context) (bool, error) { return true, nil }
func (installFakeService) Status(context.Context) (service.Status, error) {
	return service.Status{Loaded: true}, nil
}

func installTestApp(t *testing.T) *app.App {
	t.Helper()
	self, err := app.ResolveSelf()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	paths := app.Paths{
		ConfigDir: filepath.Join(dir, "config"),
		HostFile:  filepath.Join(dir, "config", "host"),
		BinDir:    filepath.Join(dir, "bin"),
		BinPath:   self,
		Log:       filepath.Join(dir, "logs", "portal.log"),
		Label:     "local.portal.test",
	}
	return &app.App{Paths: paths, Cfg: config.New(paths.ConfigDir), Service: installFakeService{}}
}

func useInstallFake(t *testing.T, fake *installFakeSetup) {
	t.Helper()
	original := newSetupRunner
	newSetupRunner = func(_ *app.App, sink setup.Sink) setupRunner {
		fake.sink = sink
		return fake
	}
	t.Cleanup(func() { newSetupRunner = original })
}

func TestRunInstallOutputRegression(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	fake := &installFakeSetup{
		proceed:  true,
		clipWarn: true,
		report: &doctor.Report{Host: "user@box", Checks: []doctor.Check{
			{Name: "ssh master", Status: doctor.Pass, Detail: "UP (pid=1)"},
		}},
	}
	useInstallFake(t, fake)

	a := installTestApp(t)
	if err := os.MkdirAll(a.Paths.BinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a.Paths.BinPath = filepath.Join(a.Paths.BinDir, "portal")
	var out bytes.Buffer
	if err := runInstall(context.Background(), &out, strings.NewReader(""), false, a, " user @box "); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	want := fmt.Sprintf("checking ssh to user@box ...\n"+
		"ok\n"+
		"configured dev box: user@box  (saved to %s)\n"+
		"installed command -> %s\n"+
		"service loaded and started (%s)\n"+
		"installed xdg-open wrapper on user@box\n"+
		"WARNING: could not install clipboard shims on user@box: shim denied\n"+
		"         clipboard paste into coding agents will NOT work until this succeeds.\n"+
		"         fix the cause above and re-run: portal install user@box\n"+
		"NOTE: %s is not on your PATH. Add it to your shell profile:\n"+
		"      export PATH=\"$HOME/.local/bin:$PATH\"\n"+
		"\nrunning self-test (portal doctor) ...\n"+
		"portal doctor — user@box\n"+
		"  [PASS] ssh master: UP (pid=1)\n"+
		"\nRESULT: PASS — clipboard paste should work over plain ssh.\n"+
		"\ntry:  portal status\n", a.Paths.HostFile, a.Paths.BinPath, a.Paths.Label, a.Paths.BinDir)
	if got := out.String(); got != want {
		t.Fatalf("install output mismatch:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	if got := strings.Join(fake.calls, ","); got != "validate,configure,deploy,verify,close" {
		t.Fatalf("phase calls = %q", got)
	}
}

func TestRunInstallMissingSSWarningDoesNotJoinTerminalStatus(t *testing.T) {
	fake := &installFakeSetup{proceed: true, missingSS: true, report: &doctor.Report{Host: "box"}}
	useInstallFake(t, fake)
	a := installTestApp(t)
	var out bytes.Buffer
	if err := runInstall(context.Background(), &out, strings.NewReader(""), false, a, "box"); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	if !strings.Contains(out.String(), "Port discovery may not work.\nok\n") {
		t.Fatalf("missing-ss output joined warning and status: %q", out.String())
	}
}

func TestRunInstallValidationFailureNonTTYDoesNotPrompt(t *testing.T) {
	fake := &installFakeSetup{report: &doctor.Report{Host: "box"}}
	useInstallFake(t, fake)
	var out bytes.Buffer
	err := runInstall(context.Background(), &out, strings.NewReader("y\n"), false, installTestApp(t), "box")
	if err == nil || err.Error() != "ssh validation failed for box" {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(out.String(), "install anyway?") {
		t.Fatalf("non-TTY output contains prompt: %q", out.String())
	}
	if got := strings.Join(fake.calls, ","); got != "validate,close" {
		t.Fatalf("phase calls = %q", got)
	}
}

func TestRunInstallValidationFailurePromptYesContinuesWithoutRevalidate(t *testing.T) {
	fake := &installFakeSetup{report: &doctor.Report{Host: "box"}}
	useInstallFake(t, fake)
	var out bytes.Buffer
	if err := runInstall(context.Background(), &out, strings.NewReader("yes\n"), true, installTestApp(t), "box"); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	if !strings.Contains(out.String(), "install anyway? [y/N] ") {
		t.Fatalf("output missing prompt: %q", out.String())
	}
	if got := strings.Join(fake.calls, ","); got != "validate,configure,deploy,verify,close" {
		t.Fatalf("phase calls = %q, want one validate followed by remaining phases", got)
	}
}

func TestRunInstallValidationFailurePromptDeclineAborts(t *testing.T) {
	fake := &installFakeSetup{report: &doctor.Report{Host: "box"}}
	useInstallFake(t, fake)
	var out bytes.Buffer
	err := runInstall(context.Background(), &out, strings.NewReader("n\n"), true, installTestApp(t), "box")
	if err == nil || err.Error() != "install aborted" {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(out.String(), "install anyway? [y/N] aborted.") {
		t.Fatalf("decline output = %q", out.String())
	}
	if got := strings.Join(fake.calls, ","); got != "validate,close" {
		t.Fatalf("phase calls = %q", got)
	}
}

func TestInstallCommandUsesTTYSeam(t *testing.T) {
	fake := &installFakeSetup{report: &doctor.Report{Host: "box"}}
	useInstallFake(t, fake)
	original := installIsTTY
	installIsTTY = func(*os.File) bool { return false }
	t.Cleanup(func() { installIsTTY = original })

	cmd := newInstallCmd(installTestApp(t))
	cmd.SetArgs([]string{"box"})
	cmd.SetOut(io.Discard)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || err.Error() != "ssh validation failed for box" {
		t.Fatalf("command error = %v", err)
	}
}
