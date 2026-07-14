package main

import (
	"bytes"
	"context"
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
	sink     setup.Sink
	proceed  bool
	calls    []string
	report   *doctor.Report
	clipWarn bool
}

func (f *installFakeSetup) Validate(_ context.Context, _ string, force bool) bool {
	f.calls = append(f.calls, "validate")
	f.sink(api.SetupEvent{Step: "validate", Status: "running"})
	if f.proceed {
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

	var out bytes.Buffer
	if err := runInstall(context.Background(), &out, strings.NewReader(""), false, installTestApp(t), " user @box "); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	wants := []string{
		"checking ssh to user@box ...",
		"ok",
		"configured dev box: user@box",
		"service loaded and started",
		"installed xdg-open wrapper on user@box",
		"WARNING: could not install clipboard shims on user@box: shim denied",
		"clipboard paste into coding agents will NOT work until this succeeds",
		"is not on your PATH",
		"running self-test (portal doctor) ...",
		"portal doctor — user@box",
		"RESULT: PASS",
		"try:  portal status",
	}
	assertTextInOrder(t, out.String(), wants...)
	if got := strings.Join(fake.calls, ","); got != "validate,configure,deploy,verify,close" {
		t.Fatalf("phase calls = %q", got)
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

func assertTextInOrder(t *testing.T, text string, wants ...string) {
	t.Helper()
	pos := 0
	for _, want := range wants {
		i := strings.Index(text[pos:], want)
		if i < 0 {
			t.Fatalf("output missing %q after byte %d:\n%s", want, pos, text)
		}
		pos += i + len(want)
	}
}
