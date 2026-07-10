package clipshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCredentialShimVersionAndMarkers(t *testing.T) {
	if Version != "6" {
		t.Fatalf("Version = %q, want 6", Version)
	}
	if Marker != "Installed by portal clip-shim v6" {
		t.Fatalf("Marker = %q, want v6 marker", Marker)
	}

	tests := []struct {
		name   string
		script string
		exec   string
	}{
		{"portal", portalShim, `exec "$_portald" "$@"`},
		{"portal-askpass", portalAskpassShim, `exec "$_portald" keychain askpass "$@"`},
		{"sudo", sudoShim, `exec "$_real" -A "$@"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.script, Marker) {
				t.Errorf("script missing Marker %q", Marker)
			}
			if !strings.Contains(tc.script, tc.exec) {
				t.Errorf("script missing exec line %q", tc.exec)
			}
		})
	}
}

func TestSudoShimSafetyMatrixText(t *testing.T) {
	for _, want := range []string{
		`for _d in $PATH; do`,
		`[ -n "$_d" ] || continue`,
		`[ -x "$_d/sudo" ]`,
		`[ -z "$_real" ]`,
		`printf '%s\n' 'portal sudo: real sudo not found' >&2`,
		`[ -t 0 ] || [ -t 1 ] || [ -t 2 ] || ( : < /dev/tty ) 2>/dev/null`,
		`[ -z "${SUDO_ASKPASS:-}" ]`,
		`[ ! -x "$SUDO_ASKPASS" ]`,
		`for a in "$@"; do`,
		`--askpass|--stdin|--non-interactive|--edit)`,
		`case "${a#-}" in`,
		`*[ASnehVKkv]*)`,
		`--)
            break`,
		`--*)`,
		`-*)`,
		`*)
            break`,
		`exec "$_real" "$@"`,
		`exec "$_real" -A "$@"`,
	} {
		if !strings.Contains(sudoShim, want) {
			t.Errorf("sudo shim missing safety branch text %q", want)
		}
	}

	var addingAskpass []string
	for _, line := range strings.Split(sudoShim, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "exec ") && strings.Contains(line, " -A ") {
			addingAskpass = append(addingAskpass, line)
		}
	}
	if len(addingAskpass) != 1 || addingAskpass[0] != `exec "$_real" -A "$@"` {
		t.Fatalf("exec lines adding -A = %q, want only the guarded injection branch", addingAskpass)
	}
	noRealBlock := `if [ -z "$_real" ]; then
    printf '%s\n' 'portal sudo: real sudo not found' >&2
    exit 1
fi`
	if !strings.Contains(sudoShim, noRealBlock) {
		t.Fatal("missing-real-sudo branch must report one line and exit 1")
	}

	ttyCheck := strings.Index(sudoShim, `[ -t 0 ] || [ -t 1 ] || [ -t 2 ] || ( : < /dev/tty ) 2>/dev/null`)
	if ttyCheck < 0 {
		t.Fatal("sudo shim missing TTY check")
	}
	ttyPassthrough := strings.Index(sudoShim[ttyCheck:], `exec "$_real" "$@"`)
	injection := strings.LastIndex(sudoShim, `exec "$_real" -A "$@"`)
	if ttyPassthrough < 0 || injection < ttyCheck+ttyPassthrough {
		t.Fatal("TTY passthrough must precede the sole askpass injection branch")
	}
}

func TestSudoShimDetachedBehavior(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sudo shim is /bin/sh")
	}

	home := t.TempDir()
	shimDir := filepath.Join(home, ".local", "bin")
	realDir := filepath.Join(home, "realbin")
	for _, dir := range []string{shimDir, realDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shimPath := filepath.Join(shimDir, "sudo")
	askpassPath := filepath.Join(shimDir, "portal-askpass")
	writeExec(t, shimPath, "%s", sudoShim)
	writeExec(t, askpassPath, "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(realDir, "sudo"), "%s", `#!/bin/sh
for a in "$@"; do
    printf '<%s>\n' "$a"
done
`)

	path := coreutilsPath(shimDir, realDir)
	tests := []struct {
		name    string
		askpass string
		args    []string
		want    string
	}{
		{"eligible injects askpass", askpassPath, []string{"whoami"}, "<-A>\n<whoami>\n"},
		{"explicit short askpass passes through", askpassPath, []string{"-A", "whoami"}, "<-A>\n<whoami>\n"},
		{"explicit askpass passes through", askpassPath, []string{"--askpass", "whoami"}, "<--askpass>\n<whoami>\n"},
		{"explicit stdin passes through", askpassPath, []string{"-S", "whoami"}, "<-S>\n<whoami>\n"},
		{"explicit long stdin passes through", askpassPath, []string{"--stdin", "whoami"}, "<--stdin>\n<whoami>\n"},
		{"non-interactive passes through", askpassPath, []string{"-n", "whoami"}, "<-n>\n<whoami>\n"},
		{"long non-interactive passes through", askpassPath, []string{"--non-interactive", "whoami"}, "<--non-interactive>\n<whoami>\n"},
		{"edit passes through", askpassPath, []string{"-e", "file"}, "<-e>\n<file>\n"},
		{"help passes through", askpassPath, []string{"-h"}, "<-h>\n"},
		{"version passes through", askpassPath, []string{"-V"}, "<-V>\n"},
		{"invalidate timestamp passes through", askpassPath, []string{"-K"}, "<-K>\n"},
		{"reset timestamp passes through", askpassPath, []string{"-k"}, "<-k>\n"},
		{"timestamp passes through", askpassPath, []string{"-v"}, "<-v>\n"},
		{"bundled stdin passes through", askpassPath, []string{"-Sk", "apt", "update"}, "<-Sk>\n<apt>\n<update>\n"},
		{"bundled non-interactive passes through", askpassPath, []string{"-nH", "x"}, "<-nH>\n<x>\n"},
		{"bundled reset and stdin passes through", askpassPath, []string{"-kS", "x"}, "<-kS>\n<x>\n"},
		{"command short flag still injects", askpassPath, []string{"apt", "install", "-v"}, "<-A>\n<apt>\n<install>\n<-v>\n"},
		{"command numeric flag still injects", askpassPath, []string{"systemctl", "-n", "3", "x"}, "<-A>\n<systemctl>\n<-n>\n<3>\n<x>\n"},
		{"double dash ends sudo scan", askpassPath, []string{"--", "printf", "-n"}, "<-A>\n<-->\n<printf>\n<-n>\n"},
		{"missing askpass passes through", filepath.Join(home, "missing-askpass"), []string{"whoami"}, "<whoami>\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(shimPath, tc.args...)
			detachFromControllingTerminal(cmd)
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + path,
				"SUDO_ASKPASS=" + tc.askpass,
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("sudo %v: %v (out=%q)", tc.args, err, out)
			}
			if string(out) != tc.want {
				t.Fatalf("sudo %v output = %q, want %q", tc.args, out, tc.want)
			}
		})
	}
}

func TestSudoShimControllingTerminalPassthroughWithRedirectedStdin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin script(1) command form provides the hermetic controlling terminal")
	}
	scriptPath, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is unavailable")
	}

	home := t.TempDir()
	shimDir := filepath.Join(home, "shims")
	realDir := filepath.Join(home, "real")
	for _, dir := range []string{shimDir, realDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shimPath := filepath.Join(shimDir, "sudo")
	askpassPath := filepath.Join(shimDir, "portal-askpass")
	writeExec(t, shimPath, "%s", sudoShim)
	writeExec(t, askpassPath, "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(realDir, "sudo"), "%s", `#!/bin/sh
for a in "$@"; do
    printf '<%s>\n' "$a"
done
`)

	inputPath := filepath.Join(home, "input")
	outputPath := filepath.Join(home, "output")
	errorPath := filepath.Join(home, "stderr")
	if err := os.WriteFile(inputPath, []byte("redirected input"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := shellQuote(shimPath) + ` tee /etc/example < ` + shellQuote(inputPath) +
		` > ` + shellQuote(outputPath) + ` 2> ` + shellQuote(errorPath)
	cmd := exec.Command(scriptPath, "-q", "/dev/null", "/bin/sh", "-c", command)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + coreutilsPath(shimDir, realDir),
		"SUDO_ASKPASS=" + askpassPath,
	}
	transcript, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sudo with controlling terminal: %v (transcript=%q)", err, transcript)
	}
	out, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "<tee>\n</etc/example>\n"; got != want {
		t.Fatalf("redirected-stdin sudo output = %q, want passthrough %q", got, want)
	}
}

func TestAskpassEnvSnippetIsIndependentAndGuarded(t *testing.T) {
	if AskpassMarkerStart != "# >>> portal askpass (sudo) >>>" {
		t.Fatalf("AskpassMarkerStart = %q", AskpassMarkerStart)
	}
	if AskpassMarkerEnd != "# <<< portal askpass (sudo) <<<" {
		t.Fatalf("AskpassMarkerEnd = %q", AskpassMarkerEnd)
	}
	want := `if [ -z "${SUDO_ASKPASS:-}" ] && [ -x "$HOME/.local/bin/portal-askpass" ]; then
    export SUDO_ASKPASS="$HOME/.local/bin/portal-askpass"
fi`
	if !strings.Contains(askpassEnvSnippet, want) {
		t.Errorf("askpass env block missing preservation guard/export %q", want)
	}
	if strings.Contains(askpassEnvSnippet, `[ -x "$HOME/.local/bin/portal-askpass" ] && export`) {
		t.Error("askpass env block still overwrites a user-selected helper")
	}
	if strings.Contains(pathPrependSnippet, AskpassMarkerStart) {
		t.Error("SUDO_ASKPASS block must remain separate from the PATH block")
	}
	if strings.Contains(askpassEnvSnippet, PathMarkerStart) {
		t.Error("SUDO_ASKPASS block must not absorb the PATH block")
	}
}

func TestAskpassEnvSnippetPreservesUserValue(t *testing.T) {
	home := t.TempDir()
	helper := filepath.Join(home, ".local", "bin", "portal-askpass")
	if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, helper, "#!/bin/sh\nexit 0\n")

	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{name: "unset selects portal", want: helper},
		{name: "existing value preserved", existing: "/user/selected/askpass", want: "/user/selected/askpass"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", askpassEnvSnippet+"\nprintf '%s' \"${SUDO_ASKPASS-}\"")
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
			if tc.existing != "" {
				cmd.Env = append(cmd.Env, "SUDO_ASKPASS="+tc.existing)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("source askpass block: %v (out=%q)", err, out)
			}
			if string(out) != tc.want {
				t.Fatalf("SUDO_ASKPASS = %q, want %q", out, tc.want)
			}
		})
	}
}
