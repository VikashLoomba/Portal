package doctorprobe

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/clipshim"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/localexec"
)

type versionTransport struct{ out string }

func (*versionTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (*versionTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1}, nil
}
func (t *versionTransport) Exec(context.Context, []byte, ...string) (string, string, error) {
	return t.out, "", nil
}
func (*versionTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}
func (*versionTransport) Close(context.Context) (bool, error) { return false, nil }
func (*versionTransport) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplSystemSSH, Host: "box"}
}

type cancelTransport struct {
	cancel context.CancelFunc
	execs  []string
}

func (t *cancelTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (t *cancelTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1}, nil
}
func (t *cancelTransport) Exec(ctx context.Context, _ []byte, argv ...string) (string, string, error) {
	t.execs = append(t.execs, strings.Join(argv, " "))
	t.cancel()
	return "", "", ctx.Err()
}
func (t *cancelTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}
func (t *cancelTransport) Close(context.Context) (bool, error) { return false, nil }
func (t *cancelTransport) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplSystemSSH, Host: "box"}
}

type recordingTransport struct {
	execs []string
}

func (*recordingTransport) Ensure(context.Context) (bool, error) { return false, nil }
func (*recordingTransport) Health(context.Context) (transport.Health, error) {
	return transport.Health{Up: true, Pid: 1}, nil
}
func (t *recordingTransport) Exec(_ context.Context, _ []byte, argv ...string) (string, string, error) {
	call := strings.Join(argv, " ")
	t.execs = append(t.execs, call)
	switch {
	case strings.Contains(call, "command -v xclip"):
		return "SHIM /home/u/.local/bin/xclip", "", nil
	case strings.Contains(call, "command -v wl-paste"):
		return "SHIM /home/u/.local/bin/wl-paste", "", nil
	case strings.Contains(call, "command -v wl-copy"):
		return "SHIM /home/u/.local/bin/wl-copy", "", nil
	case strings.Contains(call, "command -v pbcopy"):
		return "SHIM /home/u/.local/bin/pbcopy", "", nil
	case strings.Contains(call, "command -v pbpaste"):
		return "SHIM /home/u/.local/bin/pbpaste", "", nil
	case strings.Contains(call, "command -v xsel"):
		return "SHIM /home/u/.local/bin/xsel", "", nil
	case strings.Contains(call, "line=$(grep -F"):
		return clipshim.Version, "", nil
	case strings.Contains(call, "PORTALD_OK"):
		return "PORTALD_OK\nCLIP_OK\nCLIPCOPY_OK\nNOTIFY_OK\n", "", nil
	case strings.Contains(call, "clip targets xclip; echo"):
		return "image/png\nEXIT=0", "", nil
	default:
		return "", "", nil
	}
}
func (*recordingTransport) Stream(context.Context, ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	return nil, nil, nil, nil, nil
}
func (*recordingTransport) Close(context.Context) (bool, error) { return false, nil }
func (*recordingTransport) Describe() transport.Desc {
	return transport.Desc{Impl: transport.ImplSystemSSH, Host: "box"}
}

func TestRunStopsBetweenRemoteProbesOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := &cancelTransport{cancel: cancel}
	rep := Run(ctx, "box", tr)
	if len(tr.execs) != 1 || !strings.Contains(tr.execs[0], "command -v xclip") {
		t.Fatalf("Exec calls = %v, want only xclip PATH probe", tr.execs)
	}
	if len(rep.Checks) != 2 || rep.Checks[1].Name != "PATH winner: xclip" {
		t.Fatalf("partial report = %#v", rep.Checks)
	}
}

func TestDeployedShimVersionRejectsSeparatorOnlyOutput(t *testing.T) {
	for _, out := range []string{".", "..", " .\t. \n"} {
		if version, ok := deployedShimVersion(context.Background(), &versionTransport{out: out}); ok || version != "" {
			t.Fatalf("deployedShimVersion(%q) = %q, %v, want empty false", out, version, ok)
		}
	}
}

func TestRun_ProbesAllSixPathWinnersInOrder(t *testing.T) {
	tr := &recordingTransport{}
	Run(context.Background(), "box", tr)

	var got []string
	for _, call := range tr.execs {
		for _, tool := range []string{"xclip", "wl-paste", "wl-copy", "pbcopy", "pbpaste", "xsel"} {
			if strings.Contains(call, "command -v "+tool) {
				got = append(got, tool)
			}
		}
	}
	want := []string{"xclip", "wl-paste", "wl-copy", "pbcopy", "pbpaste", "xsel"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("PATH probe order = %v, want %v", got, want)
	}
}

func TestGeneratedRemoteProbeCommandsExecute(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	cacheDir := filepath.Join(home, ".cache", "portal")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	profile := "export PATH=" + doctorShellQuote(path) + "\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"xclip", "wl-paste", "wl-copy", "pbcopy", "pbpaste", "xsel"} {
		script := "#!/bin/sh\n# " + clipshim.Marker + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	portald := `#!/bin/sh
if [ "$1:$2" = "clip:targets" ]; then
    printf '%s\n' text/plain
    exit 0
fi
case "$1" in
  clip) printf '%s\n' 'usage: portald clip copy' >&2; exit 1 ;;
  notify) printf '%s\n' 'usage: portald notify' >&2; exit 1 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(cacheDir, "portald"), []byte(portald), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", path)
	tr := localexec.New()

	for _, tool := range []string{"xclip", "wl-paste", "wl-copy", "pbcopy", "pbpaste", "xsel"} {
		got, isShim := resolveShimWinner(context.Background(), tr, tool)
		if got != filepath.Join(binDir, tool) || !isShim {
			t.Fatalf("resolveShimWinner(%q) = %q, %v", tool, got, isShim)
		}
	}
	if got, ok := deployedShimVersion(context.Background(), tr); !ok || got != clipshim.Version {
		t.Fatalf("deployedShimVersion = %q, %v", got, ok)
	}
	present, verbs := probePortaldVerbs(context.Background(), tr)
	if !present || !verbs.clip || !verbs.clipCopy || !verbs.notify {
		t.Fatalf("probePortaldVerbs = %v, %+v", present, verbs)
	}
	if out, code := smokeClipTargets(context.Background(), tr); out != "text/plain" || code != 0 {
		t.Fatalf("smokeClipTargets = %q, %d", out, code)
	}
}

func TestRun_NoDestructiveWriteSmoke(t *testing.T) {
	tr := &recordingTransport{}
	Run(context.Background(), "box", tr)

	for _, call := range tr.execs {
		if strings.Contains(call, "clip copy") ||
			strings.Contains(call, "copy\\ttext") ||
			strings.Contains(call, "copy\ttext") {
			t.Fatalf("doctor issued a clipboard-write smoke: %q", call)
		}
	}
}

func TestRun_ClipWriteServiceNegotiation(t *testing.T) {
	tests := []struct {
		name       string
		view       ServiceView
		wantStatus doctor.Status
		wantDetail string
	}{
		{
			name: "both_v1",
			view: ServiceView{
				Connected: true,
				Agent:     map[string]uint32{"clipwrite": 1},
				Client:    map[string]uint32{"clipwrite": 1},
			},
			wantStatus: doctor.Pass,
			wantDetail: "agent=1 client=1",
		},
		{
			name:       "agent_absent",
			view:       ServiceView{Connected: true, Client: map[string]uint32{"clipwrite": 1}},
			wantStatus: doctor.Warn,
			wantDetail: "agent advertises clipwrite=absent",
		},
		{
			name: "agent_mismatch",
			view: ServiceView{
				Connected: true,
				Agent:     map[string]uint32{"clipwrite": 2},
				Client:    map[string]uint32{"clipwrite": 1},
			},
			wantStatus: doctor.Warn,
			wantDetail: "agent advertises clipwrite=2",
		},
		{
			name:       "client_absent",
			view:       ServiceView{Connected: true, Agent: map[string]uint32{"clipwrite": 1}},
			wantStatus: doctor.Warn,
			wantDetail: "Mac client advertises clipwrite=absent",
		},
		{
			name: "client_mismatch",
			view: ServiceView{
				Connected: true,
				Agent:     map[string]uint32{"clipwrite": 1},
				Client:    map[string]uint32{"clipwrite": 2},
			},
			wantStatus: doctor.Warn,
			wantDetail: "Mac client advertises clipwrite=2",
		},
		{
			name:       "not_connected",
			view:       ServiceView{},
			wantStatus: doctor.Warn,
			wantDetail: "no agent handshake",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := Run(context.Background(), "box", &recordingTransport{},
				WithServices(func() ServiceView { return tt.view }))
			check := checkNamed(rep, "service: clipwrite@1")
			if check == nil {
				t.Fatal("missing service: clipwrite@1 check")
			}
			if check.Status != tt.wantStatus || !strings.Contains(check.Detail, tt.wantDetail) {
				t.Fatalf("service check = %#v, want %s containing %q", check, tt.wantStatus.Tag(), tt.wantDetail)
			}
			if tt.wantStatus == doctor.Warn && !rep.OK() {
				t.Fatal("service negotiation warning must not fail the report")
			}
		})
	}

	rep := Run(context.Background(), "box", &recordingTransport{})
	if checkNamed(rep, "service: clipwrite@1") != nil {
		t.Fatal("service check must be omitted without WithServices")
	}
}

func TestRun_ClipWriteFeatureState(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		rep := Run(context.Background(), "box", &recordingTransport{},
			WithFeatures(func(string) bool { return true }))
		check := checkNamed(rep, "feature: clip-write")
		if check == nil || check.Status != doctor.Pass || check.Detail != "on" {
			t.Fatalf("feature check = %#v, want PASS on", check)
		}
	})
	t.Run("off", func(t *testing.T) {
		rep := Run(context.Background(), "box", &recordingTransport{},
			WithFeatures(func(string) bool { return false }))
		check := checkNamed(rep, "feature: clip-write")
		if check == nil || check.Status != doctor.Warn ||
			!strings.Contains(check.Detail, "features clip-write on") {
			t.Fatalf("feature check = %#v, want WARN with enable command", check)
		}
		if !rep.OK() {
			t.Fatal("disabled feature warning must not fail the report")
		}
	})
	t.Run("absent", func(t *testing.T) {
		rep := Run(context.Background(), "box", &recordingTransport{})
		if checkNamed(rep, "feature: clip-write") != nil {
			t.Fatal("feature check must be omitted without WithFeatures")
		}
	})
}

func checkNamed(rep *doctor.Report, name string) *doctor.Check {
	for i := range rep.Checks {
		if rep.Checks[i].Name == name {
			return &rep.Checks[i]
		}
	}
	return nil
}
