package clipshim

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/transport/ptyx"
)

type shimShell struct {
	name    string
	argv    []string
	missing string
}

func shimShells() []shimShell {
	shells := []shimShell{{name: "sh", argv: []string{"/bin/sh"}}}

	dash, err := exec.LookPath("dash")
	if err != nil {
		if _, statErr := os.Stat("/bin/dash"); statErr == nil {
			dash = "/bin/dash"
		} else {
			shells = append(shells, shimShell{name: "dash", missing: "dash is unavailable"})
		}
	}
	if dash != "" {
		shells = append(shells, shimShell{name: "dash", argv: []string{dash}})
	}

	busybox, err := exec.LookPath("busybox")
	if err != nil {
		shells = append(shells, shimShell{name: "busybox-ash", missing: "busybox is unavailable"})
	} else {
		shells = append(shells, shimShell{name: "busybox-ash", argv: []string{busybox, "sh"}})
	}
	return shells
}

func requireShimShell(t *testing.T, shell shimShell) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shim scripts are /bin/sh")
	}
	if shell.missing == "" {
		return
	}
	if os.Getenv("PORTAL_SHIM_SHELLS_STRICT") != "" {
		t.Fatal(shell.missing)
	}
	t.Skip(shell.missing)
}

type shimRunSpec struct {
	bin         string
	script      string
	args        []string
	stdin       []byte
	withPortald bool
	portaldRC   int
	withReal    bool
	mktempFails bool
}

type shimRunResult struct {
	stdout      []byte
	stderr      []byte
	exitCode    int
	portalArgs  []byte
	portalStdin []byte
	realArgs    []byte
	realStdin   []byte
	tmpNames    []string
}

func runShimScript(t *testing.T, shell shimShell, spec shimRunSpec) shimRunResult {
	t.Helper()
	requireShimShell(t, shell)

	home := t.TempDir()
	shimDir := filepath.Join(home, "shims")
	realDir := filepath.Join(home, "real")
	cacheDir := filepath.Join(home, ".cache", "portal")
	tmpDir := filepath.Join(home, "tmp")
	toolDir := filepath.Join(home, "tools")
	for _, dir := range []string{shimDir, realDir, cacheDir, tmpDir, toolDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	shimPath := filepath.Join(shimDir, spec.bin)
	writeExec(t, shimPath, "%s", spec.script)

	portalArgs := filepath.Join(home, "portal-args")
	portalStdin := filepath.Join(home, "portal-stdin")
	realArgs := filepath.Join(home, "real-args")
	realStdin := filepath.Join(home, "real-stdin")
	if spec.withPortald {
		writeExec(t, filepath.Join(cacheDir, "portald"), "%s", `#!/bin/sh
printf '%s\n' "$*" >> "$PORTAL_ARGS"
case "$*" in
  "clip copy"*) cat >> "$PORTAL_STDIN" ;;
esac
exit "${PORTALD_RC:-0}"
`)
	}
	if spec.withReal {
		writeExec(t, filepath.Join(realDir, spec.bin), "%s", `#!/bin/sh
printf '%s\n' "$*" >> "$PORTAL_REAL_ARGS"
cat > "$PORTAL_REAL_STDIN"
exit 0
`)
	}
	if spec.mktempFails {
		writeExec(t, filepath.Join(toolDir, "mktemp"), "%s", "#!/bin/sh\nexit 1\n")
	}

	pathParts := []string{shimDir}
	if spec.mktempFails {
		pathParts = append(pathParts, toolDir)
	}
	if spec.withReal {
		pathParts = append(pathParts, realDir)
	}
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			pathParts = append(pathParts, dir)
		}
	}

	argv := append([]string{}, shell.argv[1:]...)
	argv = append(argv, shimPath)
	argv = append(argv, spec.args...)
	cmd := exec.Command(shell.argv[0], argv...)
	cmd.Dir = home
	cmd.Stdin = bytes.NewReader(spec.stdin)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + strings.Join(pathParts, ":"),
		"TMPDIR=" + tmpDir,
		"PORTAL_ARGS=" + portalArgs,
		"PORTAL_STDIN=" + portalStdin,
		"PORTAL_REAL_ARGS=" + realArgs,
		"PORTAL_REAL_STDIN=" + realStdin,
		"PORTALD_RC=" + strconv.Itoa(spec.portaldRC),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("%s via %s: %v", spec.bin, shell.name, err)
		}
		exitCode = exitErr.ExitCode()
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	tmpNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		tmpNames = append(tmpNames, entry.Name())
	}
	return shimRunResult{
		stdout:      stdout.Bytes(),
		stderr:      stderr.Bytes(),
		exitCode:    exitCode,
		portalArgs:  readOptional(t, portalArgs),
		portalStdin: readOptional(t, portalStdin),
		realArgs:    readOptional(t, realArgs),
		realStdin:   readOptional(t, realStdin),
		tmpNames:    tmpNames,
	}
}

func readOptional(t *testing.T, path string) []byte {
	t.Helper()
	got, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertShimSuccess(t *testing.T, result shimRunResult, wantArgs string, wantStdin []byte) {
	t.Helper()
	if result.exitCode != 0 {
		t.Fatalf("exit = %d, want 0 (stdout=%q stderr=%q)", result.exitCode, result.stdout, result.stderr)
	}
	if len(result.stdout) != 0 || len(result.stderr) != 0 {
		t.Fatalf("stdout/stderr = %q/%q, want empty", result.stdout, result.stderr)
	}
	if got, want := string(result.portalArgs), wantArgs+"\n"; got != want {
		t.Fatalf("portald argv = %q, want %q", got, want)
	}
	if !bytes.Equal(result.portalStdin, wantStdin) {
		t.Fatalf("portald stdin = %q, want byte-exact %q", result.portalStdin, wantStdin)
	}
	if len(result.realArgs) != 0 {
		t.Fatalf("real binary unexpectedly ran: %q", result.realArgs)
	}
	assertNoShimTempLitter(t, result)
}

func assertNoShimTempLitter(t *testing.T, result shimRunResult) {
	t.Helper()
	for _, name := range result.tmpNames {
		if strings.HasPrefix(name, "portal-clip.") || strings.HasPrefix(name, "portal-pbcopy.") {
			t.Errorf("temporary clipboard file survived: %s", name)
		}
	}
}

func TestWriteShimXclipV7ReadParity(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"targets", []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}, "clip targets xclip"},
		{"png", []string{"-selection", "clipboard", "-t", "image/png", "-o"}, "clip image png"},
		{"bare selection first", []string{"-selection", "clipboard", "-o"}, "clip text"},
		{"UTF8_STRING", []string{"-selection", "clipboard", "-t", "UTF8_STRING", "-o"}, "clip text"},
		{"TEXT", []string{"-selection", "clipboard", "-t", "TEXT", "-o"}, "clip text"},
		{"STRING", []string{"-selection", "clipboard", "-t", "STRING", "-o"}, "clip text"},
		{"text/plain", []string{"-selection", "clipboard", "-t", "text/plain", "-o"}, "clip text"},
		{"output first", []string{"-o", "-selection", "clipboard"}, "clip text"},
		{"target before output", []string{"-t", "UTF8_STRING", "-o", "-selection", "clipboard"}, "clip text"},
		{"primary abbreviation", []string{"-sel", "p", "-o"}, "clip text"},
		{"bare output", []string{"-o"}, "clip text"},
		{"secondary output", []string{"-out", "-sel", "s"}, "clip text"},
	}
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					result := runShimScript(t, shell, shimRunSpec{
						bin:         "xclip",
						script:      xclipShim,
						args:        tc.args,
						withPortald: true,
					})
					assertShimSuccess(t, result, tc.want, nil)
				})
			}
		})
	}
}

func TestWriteShimRouting(t *testing.T) {
	payload := []byte("payload\n")
	tests := []struct {
		name      string
		bin       string
		script    string
		args      []string
		stdin     []byte
		wantArgs  string
		wantStdin []byte
	}{
		{"xclip clipboard", "xclip", xclipShim, []string{"-sel", "c", "-i"}, payload, "clip copy text", payload},
		{"xclip full selection", "xclip", xclipShim, []string{"-selection", "clipboard", "-i"}, payload, "clip copy text", payload},
		{"xclip primary", "xclip", xclipShim, []string{"-sel", "p", "-i"}, payload, "clip copy text", payload},
		{"xclip default input primary", "xclip", xclipShim, nil, payload, "clip copy text", payload},
		{"xclip input abbreviation", "xclip", xclipShim, []string{"-in"}, payload, "clip copy text", payload},
		{"xclip selection abbreviation", "xclip", xclipShim, []string{"-selec", "c", "-i"}, payload, "clip copy text", payload},
		{"xclip uppercase selection", "xclip", xclipShim, []string{"-sel", "CLIPBOARD", "-i"}, payload, "clip copy text", payload},
		{"xclip display argument", "xclip", xclipShim, []string{"-d", ":0", "-sel", "c", "-i"}, payload, "clip copy text", payload},
		{"xclip loop argument", "xclip", xclipShim, []string{"-l", "1", "-sel", "c", "-i"}, payload, "clip copy text", payload},
		{"xclip silent", "xclip", xclipShim, []string{"-si", "-sel", "c", "-i"}, payload, "clip copy text", payload},
		{"xclip noutf8", "xclip", xclipShim, []string{"-noutf8", "-sel", "c", "-i"}, payload, "clip copy text", payload},
		{"xclip png", "xclip", xclipShim, []string{"-i", "-t", "image/png", "-sel", "c"}, payload, "clip copy image png", payload},
		{"xclip png canonical order", "xclip", xclipShim, []string{"-selection", "clipboard", "-t", "image/png", "-i"}, payload, "clip copy image png", payload},
		{"xclip trim long", "xclip", xclipShim, []string{"-rmlastnl", "-sel", "c", "-i"}, payload, "clip copy text --trim", payload},
		{"xclip trim short", "xclip", xclipShim, []string{"-r", "-i"}, payload, "clip copy text --trim", payload},
		{"xclip text mime", "xclip", xclipShim, []string{"-t", "text/plain;charset=utf-8", "-sel", "c", "-i"}, payload, "clip copy text", payload},
		{"wl-copy stdin", "wl-copy", wlCopyShim, nil, payload, "clip copy text", payload},
		{"wl-copy argv", "wl-copy", wlCopyShim, []string{"some", "words"}, []byte("ignored"), "clip copy text", []byte("some words")},
		{"wl-copy empty argv", "wl-copy", wlCopyShim, []string{""}, []byte("ignored"), "clip copy text", nil},
		{"wl-copy leading empty argv", "wl-copy", wlCopyShim, []string{"", "X"}, []byte("ignored"), "clip copy text", []byte(" X")},
		{"wl-copy clear long", "wl-copy", wlCopyShim, []string{"--clear"}, payload, "clip copy clear", nil},
		{"wl-copy clear short", "wl-copy", wlCopyShim, []string{"-c"}, payload, "clip copy clear", nil},
		{"wl-copy primary", "wl-copy", wlCopyShim, []string{"-p"}, payload, "clip copy text", payload},
		{"wl-copy trim", "wl-copy", wlCopyShim, []string{"-n"}, payload, "clip copy text --trim", payload},
		{"wl-copy bundled trim", "wl-copy", wlCopyShim, []string{"-pn"}, payload, "clip copy text --trim", payload},
		{"wl-copy bundled trim foreground", "wl-copy", wlCopyShim, []string{"-pnf"}, payload, "clip copy text --trim", payload},
		{"wl-copy text type", "wl-copy", wlCopyShim, []string{"--type=text/plain"}, payload, "clip copy text", payload},
		{"wl-copy png type", "wl-copy", wlCopyShim, []string{"-t", "image/png"}, payload, "clip copy image png", payload},
		{"wl-copy end options", "wl-copy", wlCopyShim, []string{"--", "-n", "text"}, payload, "clip copy text", []byte("-n text")},
		{"wl-copy seat and argv", "wl-copy", wlCopyShim, []string{"-s", "seat0", "hello"}, payload, "clip copy text", []byte("hello")},
		{"pbcopy text", "pbcopy", pbCopyShim, nil, payload, "clip copy text", payload},
		{"pbcopy empty clears", "pbcopy", pbCopyShim, nil, nil, "clip copy clear", nil},
		{"pbpaste reads", "pbpaste", pbPasteShim, nil, nil, "clip text", nil},
		{"xsel bundled input", "xsel", xselShim, []string{"-ib"}, payload, "clip copy text", payload},
		{"xsel bundled output", "xsel", xselShim, []string{"-ob"}, nil, "clip text", nil},
		{"xsel clear", "xsel", xselShim, []string{"-c"}, payload, "clip copy clear", nil},
		{"xsel long input", "xsel", xselShim, []string{"--input"}, payload, "clip copy text", payload},
		{"xsel long output", "xsel", xselShim, []string{"--output"}, nil, "clip text", nil},
		{"xsel input nodetach", "xsel", xselShim, []string{"-in"}, payload, "clip copy text", payload},
		{"xsel long nodetach input", "xsel", xselShim, []string{"--nodetach", "-i"}, payload, "clip copy text", payload},
		{"xsel piped default", "xsel", xselShim, nil, payload, "clip copy text", payload},
		{"xsel clipboard piped default", "xsel", xselShim, []string{"-b"}, payload, "clip copy text", payload},
		{"xsel primary piped default", "xsel", xselShim, []string{"-p"}, payload, "clip copy text", payload},
	}
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					result := runShimScript(t, shell, shimRunSpec{
						bin:         tc.bin,
						script:      tc.script,
						args:        tc.args,
						stdin:       tc.stdin,
						withPortald: true,
					})
					assertShimSuccess(t, result, tc.wantArgs, tc.wantStdin)
				})
			}
		})
	}
}

func TestWriteShimInvalidTokensFallThrough(t *testing.T) {
	tests := []struct {
		name   string
		bin    string
		script string
		args   []string
	}{
		{"xclip invalid", "xclip", xclipShim, []string{"-invalid"}},
		{"xclip input same prefix", "xclip", xclipShim, []string{"-inp"}},
		{"xclip output same prefix", "xclip", xclipShim, []string{"-oops"}},
		{"xclip output too long", "xclip", xclipShim, []string{"-outt"}},
		{"xclip selection same prefix", "xclip", xclipShim, []string{"-selx"}},
		{"xclip target same prefix", "xclip", xclipShim, []string{"-tt"}},
		{"xclip target plural", "xclip", xclipShim, []string{"-targets"}},
		{"xclip trim same prefix", "xclip", xclipShim, []string{"-rr"}},
		{"xclip ambiguous s", "xclip", xclipShim, []string{"-s", "-sel", "c", "-i"}},
		{"xclip ambiguous v", "xclip", xclipShim, []string{"-v"}},
		{"xclip equals selection", "xclip", xclipShim, []string{"-selection=clipboard", "-i"}},
		{"xclip bad selection", "xclip", xclipShim, []string{"-sel", "cheese", "-i"}},
		{"xclip selection same prefix value", "xclip", xclipShim, []string{"-sel", "primaryx", "-i"}},
		{"xclip filter", "xclip", xclipShim, []string{"-f", "-sel", "c", "-i"}},
		{"xclip file", "xclip", xclipShim, []string{"file.txt"}},
		{"xclip dangling target", "xclip", xclipShim, []string{"-t"}},
		{"xclip dangling selection", "xclip", xclipShim, []string{"-sel"}},
		{"xclip jpeg write", "xclip", xclipShim, []string{"-i", "-t", "image/jpeg"}},
		{"xclip targets write", "xclip", xclipShim, []string{"-t", "TARGETS", "-i"}},
		{"wl-copy bad bundle", "wl-copy", wlCopyShim, []string{"-pz"}},
		{"wl-copy long abbreviation", "wl-copy", wlCopyShim, []string{"--prim"}},
		{"wl-copy bare dash", "wl-copy", wlCopyShim, []string{"-"}},
		{"wl-copy gif", "wl-copy", wlCopyShim, []string{"-t", "image/gif"}},
		{"wl-copy application type", "wl-copy", wlCopyShim, []string{"-t", "application/pdf"}},
		{"wl-copy dangling type", "wl-copy", wlCopyShim, []string{"-t"}},
		{"xsel keep", "xsel", xselShim, []string{"-k"}},
		{"xsel append", "xsel", xselShim, []string{"-a"}},
		{"xsel long append", "xsel", xselShim, []string{"--append"}},
		{"xsel conflicting modes", "xsel", xselShim, []string{"-io"}},
		{"xsel clear input", "xsel", xselShim, []string{"-ic"}},
		{"xsel bare dash", "xsel", xselShim, []string{"-"}},
		{"xsel positional", "xsel", xselShim, []string{"foo"}},
	}
	payload := []byte("original stdin")
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					result := runShimScript(t, shell, shimRunSpec{
						bin:         tc.bin,
						script:      tc.script,
						args:        tc.args,
						stdin:       payload,
						withPortald: true,
						withReal:    true,
					})
					if result.exitCode != 0 {
						t.Fatalf("exit = %d, want real-binary success (stderr=%q)", result.exitCode, result.stderr)
					}
					if len(result.portalArgs) != 0 {
						t.Fatalf("invalid argv routed to portald: %q", result.portalArgs)
					}
					if len(result.realArgs) == 0 {
						t.Fatal("invalid argv did not fall through to the real binary")
					}
					if !bytes.Equal(result.realStdin, payload) {
						t.Fatalf("real stdin = %q, want pristine %q", result.realStdin, payload)
					}
					assertNoShimTempLitter(t, result)
				})
			}
		})
	}
}

func TestWriteShimInvalidTokensWithoutRealBinary(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"write invalid is loud", []string{"-invalid"}, 1, clipWriteFailMsg + "\n"},
		{"later read mode degrades", []string{"-f", "-sel", "c", "-o"}, 0, ""},
		{"info mode degrades", []string{"-version"}, 0, ""},
	}
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					result := runShimScript(t, shell, shimRunSpec{
						bin:         "xclip",
						script:      xclipShim,
						args:        tc.args,
						withPortald: true,
					})
					if result.exitCode != tc.wantCode || string(result.stderr) != tc.wantStderr {
						t.Fatalf("exit/stderr = %d/%q, want %d/%q", result.exitCode, result.stderr, tc.wantCode, tc.wantStderr)
					}
					if len(result.stdout) != 0 || len(result.portalArgs) != 0 {
						t.Fatalf("invalid argv produced stdout/portal route: %q/%q", result.stdout, result.portalArgs)
					}
				})
			}

			result := runShimScript(t, shell, shimRunSpec{
				bin:         "wl-copy",
				script:      wlCopyShim,
				args:        []string{"-t", "image/gif"},
				withPortald: true,
			})
			if result.exitCode != 1 || string(result.stderr) != clipWriteFailMsg+"\n" {
				t.Fatalf("unsupported wl-copy type exit/stderr = %d/%q", result.exitCode, result.stderr)
			}
		})
	}
}

func TestWriteShimFailureSemantics(t *testing.T) {
	payload := []byte("copy me")
	writeCases := []struct {
		name   string
		bin    string
		script string
		args   []string
	}{
		{"xclip", "xclip", xclipShim, []string{"-sel", "c", "-i"}},
		{"wl-copy", "wl-copy", wlCopyShim, nil},
		{"xsel", "xsel", xselShim, []string{"-ib"}},
	}
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tc := range writeCases {
				t.Run(tc.name, func(t *testing.T) {
					t.Run("portal success", func(t *testing.T) {
						result := runShimScript(t, shell, shimRunSpec{
							bin:         tc.bin,
							script:      tc.script,
							args:        tc.args,
							stdin:       payload,
							withPortald: true,
							withReal:    true,
						})
						if result.exitCode != 0 || len(result.stderr) != 0 || len(result.realArgs) != 0 {
							t.Fatalf("success leg exit/stderr/real = %d/%q/%q", result.exitCode, result.stderr, result.realArgs)
						}
						assertNoShimTempLitter(t, result)
					})
					t.Run("portal failure real fallback", func(t *testing.T) {
						result := runShimScript(t, shell, shimRunSpec{
							bin:         tc.bin,
							script:      tc.script,
							args:        tc.args,
							stdin:       payload,
							withPortald: true,
							portaldRC:   1,
							withReal:    true,
						})
						if result.exitCode != 0 || len(result.stderr) != 0 || len(result.realArgs) == 0 {
							t.Fatalf("fallback leg exit/stderr/real = %d/%q/%q", result.exitCode, result.stderr, result.realArgs)
						}
						assertNoShimTempLitter(t, result)
					})
					t.Run("portal failure no real", func(t *testing.T) {
						result := runShimScript(t, shell, shimRunSpec{
							bin:         tc.bin,
							script:      tc.script,
							args:        tc.args,
							stdin:       payload,
							withPortald: true,
							portaldRC:   1,
						})
						if result.exitCode != 1 || len(result.stdout) != 0 || string(result.stderr) != clipWriteFailMsg+"\n" {
							t.Fatalf("loud leg exit/stdout/stderr = %d/%q/%q", result.exitCode, result.stdout, result.stderr)
						}
						assertNoShimTempLitter(t, result)
					})
				})
			}

			t.Run("pbcopy", func(t *testing.T) {
				success := runShimScript(t, shell, shimRunSpec{
					bin:         "pbcopy",
					script:      pbCopyShim,
					stdin:       payload,
					withPortald: true,
				})
				if success.exitCode != 0 || len(success.stderr) != 0 {
					t.Fatalf("pbcopy success exit/stderr = %d/%q", success.exitCode, success.stderr)
				}
				assertNoShimTempLitter(t, success)

				failure := runShimScript(t, shell, shimRunSpec{
					bin:         "pbcopy",
					script:      pbCopyShim,
					stdin:       payload,
					withPortald: true,
					portaldRC:   1,
					withReal:    true,
				})
				if failure.exitCode != 1 || string(failure.stderr) != clipWriteFailMsg+"\n" {
					t.Fatalf("pbcopy failure exit/stderr = %d/%q", failure.exitCode, failure.stderr)
				}
				if len(failure.realArgs) != 0 {
					t.Fatalf("pbcopy must not resolve a real binary: %q", failure.realArgs)
				}
				assertNoShimTempLitter(t, failure)
			})

			readCases := []struct {
				name   string
				bin    string
				script string
				args   []string
			}{
				{"xclip", "xclip", xclipShim, []string{"-sel", "c", "-o"}},
				{"xsel", "xsel", xselShim, []string{"-ob"}},
				{"pbpaste", "pbpaste", pbPasteShim, nil},
			}
			for _, tc := range readCases {
				t.Run(tc.name+" read degrade", func(t *testing.T) {
					result := runShimScript(t, shell, shimRunSpec{
						bin:         tc.bin,
						script:      tc.script,
						args:        tc.args,
						withPortald: true,
						portaldRC:   1,
					})
					if result.exitCode != 0 || len(result.stdout) != 0 || len(result.stderr) != 0 {
						t.Fatalf("read degrade exit/stdout/stderr = %d/%q/%q", result.exitCode, result.stdout, result.stderr)
					}
				})
			}
		})
	}
}

func TestWriteShimByteExactFallback(t *testing.T) {
	payload := []byte{'s', 'e', 'c', 0, 'r', 0xc3, 0xa9, 't', 'e', ' ', 't', 'a', 'i', 'l'}
	tests := []struct {
		name   string
		bin    string
		script string
		args   []string
	}{
		{"xclip", "xclip", xclipShim, []string{"-sel", "c", "-i"}},
		{"wl-copy", "wl-copy", wlCopyShim, nil},
		{"xsel", "xsel", xselShim, []string{"-ib"}},
	}
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					t.Run("portald consumes then fails", func(t *testing.T) {
						result := runShimScript(t, shell, shimRunSpec{
							bin:         tc.bin,
							script:      tc.script,
							args:        tc.args,
							stdin:       payload,
							withPortald: true,
							portaldRC:   1,
							withReal:    true,
						})
						if result.exitCode != 0 {
							t.Fatalf("exit = %d, stderr=%q", result.exitCode, result.stderr)
						}
						if !bytes.Equal(result.portalStdin, payload) || !bytes.Equal(result.realStdin, payload) {
							t.Fatalf("portal/real stdin = %q/%q, want byte-exact %q", result.portalStdin, result.realStdin, payload)
						}
						assertNoShimTempLitter(t, result)
					})
					t.Run("no portald leaves stdin pristine", func(t *testing.T) {
						result := runShimScript(t, shell, shimRunSpec{
							bin:      tc.bin,
							script:   tc.script,
							args:     tc.args,
							stdin:    payload,
							withReal: true,
						})
						if result.exitCode != 0 || !bytes.Equal(result.realStdin, payload) {
							t.Fatalf("exit/real stdin = %d/%q, want 0/%q", result.exitCode, result.realStdin, payload)
						}
						assertNoShimTempLitter(t, result)
					})
					t.Run("mktemp failure leaves stdin pristine", func(t *testing.T) {
						result := runShimScript(t, shell, shimRunSpec{
							bin:         tc.bin,
							script:      tc.script,
							args:        tc.args,
							stdin:       payload,
							withPortald: true,
							portaldRC:   1,
							withReal:    true,
							mktempFails: true,
						})
						if result.exitCode != 0 || !bytes.Equal(result.realStdin, payload) {
							t.Fatalf("exit/real stdin = %d/%q, want 0/%q", result.exitCode, result.realStdin, payload)
						}
						if len(result.portalArgs) != 0 {
							t.Fatalf("portald ran after mktemp failure: %q", result.portalArgs)
						}
						assertNoShimTempLitter(t, result)
					})
				})
			}
		})
	}
}

func TestWriteShimXselTTYDefault(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantPortal string
		wantOutput string
	}{
		{name: "default read", wantPortal: "clip text\n"},
		{name: "short append is write", args: []string{"-a"}, wantCode: 1, wantOutput: clipWriteFailMsg + "\n"},
		{name: "long append is write", args: []string{"--append"}, wantCode: 1, wantOutput: clipWriteFailMsg + "\n"},
	}
	for _, shell := range shimShells() {
		t.Run(shell.name, func(t *testing.T) {
			requireShimShell(t, shell)
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					home := t.TempDir()
					shimDir := filepath.Join(home, "shims")
					cacheDir := filepath.Join(home, ".cache", "portal")
					toolDir := filepath.Join(home, "tools")
					for _, dir := range []string{shimDir, cacheDir, toolDir} {
						if err := os.MkdirAll(dir, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					shimPath := filepath.Join(shimDir, "xsel")
					argsPath := filepath.Join(home, "portal-args")
					stdoutPath := filepath.Join(home, "stdout")
					stderrPath := filepath.Join(home, "stderr")
					writeExec(t, shimPath, "%s", xselShim)
					writeExec(t, filepath.Join(cacheDir, "portald"), "%s", `#!/bin/sh
printf '%s\n' "$*" >> "$PORTAL_ARGS"
exit 0
`)
					dirname, err := exec.LookPath("dirname")
					if err != nil {
						t.Skip("dirname is unavailable")
					}
					if err := os.Symlink(dirname, filepath.Join(toolDir, "dirname")); err != nil {
						t.Fatal(err)
					}

					commandArgs := append([]string{}, shell.argv...)
					commandArgs = append(commandArgs, shimPath)
					commandArgs = append(commandArgs, tt.args...)
					quoted := make([]string, 0, len(commandArgs))
					for _, arg := range commandArgs {
						quoted = append(quoted, shellQuote(arg))
					}
					command := strings.Join(quoted, " ") +
						" > " + shellQuote(stdoutPath) + " 2> " + shellQuote(stderrPath)
					argv := append([]string{}, shell.argv[1:]...)
					argv = append(argv, "-c", command)
					cmd := exec.Command(shell.argv[0], argv...)
					cmd.Env = []string{
						"HOME=" + home,
						"PATH=" + strings.Join([]string{shimDir, toolDir}, ":"),
						"PORTAL_ARGS=" + argsPath,
					}
					master, err := ptyx.Start(cmd, 24, 80)
					if err != nil {
						t.Fatalf("start xsel under tty: %v", err)
					}
					waitErr := cmd.Wait()
					_ = master.Close()
					exitCode := 0
					if waitErr != nil {
						var exitErr *exec.ExitError
						if !errors.As(waitErr, &exitErr) {
							t.Fatalf("wait xsel under tty: %v", waitErr)
						}
						exitCode = exitErr.ExitCode()
					}
					if exitCode != tt.wantCode {
						t.Fatalf("exit = %d, want %d (stderr=%q)", exitCode, tt.wantCode, readOptional(t, stderrPath))
					}
					if got := string(readOptional(t, argsPath)); got != tt.wantPortal {
						t.Fatalf("portald argv = %q, want %q", got, tt.wantPortal)
					}
					if got := readOptional(t, stdoutPath); len(got) != 0 {
						t.Fatalf("stdout = %q, want empty", got)
					}
					gotOutput := string(readOptional(t, stderrPath))
					if gotOutput != tt.wantOutput {
						t.Fatalf("stderr = %q, want %q", gotOutput, tt.wantOutput)
					}
				})
			}
		})
	}
}

func TestWriteShimScriptText(t *testing.T) {
	writeScripts := map[string]string{
		"xclip":   xclipShim,
		"wl-copy": wlCopyShim,
		"pbcopy":  pbCopyShim,
		"xsel":    xselShim,
	}
	for name, script := range writeScripts {
		if !strings.HasPrefix(script, "#!/bin/sh\n") {
			t.Errorf("%s does not start with #!/bin/sh", name)
		}
		if !strings.Contains(script, Marker) {
			t.Errorf("%s does not contain marker %q", name, Marker)
		}
		if !strings.Contains(script, clipWriteFailMsg) {
			t.Errorf("%s does not contain the exact write-failure line", name)
		}
	}
	if !strings.HasPrefix(pbPasteShim, "#!/bin/sh\n") || !strings.Contains(pbPasteShim, Marker) {
		t.Error("pbpaste script header/marker is incomplete")
	}
	if !strings.Contains(xclipShim, "# "+Marker+". Intercepts") {
		t.Error("xclip marker line no longer satisfies doctorprobe's version parser")
	}
	for name, script := range map[string]string{"xclip": xclipShim, "wl-copy": wlCopyShim, "xsel": xselShim} {
		if !strings.Contains(script, clipWriteRelay) || !strings.Contains(script, clipWriteTail) {
			t.Errorf("%s does not embed the shared relay and failure tail", name)
		}
		if !strings.Contains(script, `exec 0< "$_tmp"`) {
			t.Errorf("%s does not open stdin before unlinking the replay file", name)
		}
	}
	if !strings.Contains(xselShim, `[ -t 0 ]`) {
		t.Error("xsel does not implement its stdin-tty default")
	}
	for _, forbidden := range []string{"-i*)", "-o*)", "-sel*)", "-t*)", "-r*)", "c*|p*|s*)"} {
		if strings.Contains(xclipShim, forbidden) {
			t.Errorf("xclip parser contains unsafe prefix glob %q", forbidden)
		}
	}
	for _, want := range []string{
		"-o|-ou|-out)",
		"-i|-in)",
		"-selection)",
		"-target)",
		"-rmlastnl)",
		"clipboard)",
	} {
		if !strings.Contains(xclipShim, want) {
			t.Errorf("xclip parser missing enumerated arm %q", want)
		}
	}
	if !strings.Contains(wlCopyShim, `_has_text=1`) ||
		!strings.Contains(wlCopyShim, `if [ "$_has_text" = 1 ]; then _text="$_text $_a"`) {
		t.Error("wl-copy does not preserve positional presence separately from content")
	}

	substringExpansion := regexp.MustCompile(`\$\{[^}\n]+:[0-9]+(?::[0-9]+)?\}`)
	for _, sh := range shims {
		for _, forbidden := range []string{"[[", "local ", "=(", "echo -n", "+="} {
			if strings.Contains(sh.script, forbidden) {
				t.Errorf("%s contains non-POSIX shell text %q", sh.name, forbidden)
			}
		}
		if substringExpansion.MatchString(sh.script) {
			t.Errorf("%s contains non-POSIX substring expansion %q", sh.name, substringExpansion.FindString(sh.script))
		}
	}
}

func TestWriteShimShellMatrixIsStrictInCI(t *testing.T) {
	if os.Getenv("PORTAL_SHIM_SHELLS_STRICT") == "" {
		t.Skip("strict shell availability is enforced in CI")
	}
	for _, shell := range shimShells() {
		if shell.missing != "" {
			t.Errorf("%s", shell.missing)
		}
	}
}
