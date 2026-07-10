package clipshim

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestShimArgvMatcher drives the REAL deployed shim scripts (xclip/wl-paste)
// through /bin/sh against the exact argv shapes Claude Code and opencode emit,
// asserting which branch fires. It is the argv-glob matcher test the spec
// requires (incl. the -t image/bmp falls-through case — DESIGN §6.2).
//
// We do not have a Mac to relay to, so we stub portald: a fake portald that
// prints a sentinel and exits 0 stands in for "the clip path was taken", and a
// fake real xclip/wl-paste that prints a different sentinel stands in for "fell
// through to the real binary". Asserting which sentinel reaches stdout proves
// the case-matching routed the argv to the intended branch.
func TestShimArgvMatcher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim scripts are /bin/sh")
	}

	// The shim scripts resolve $_portald from $HOME/.cache/portal/portald and
	// the real binary by re-scanning $PATH with the shim's own dir excluded.
	// We build a sandbox HOME + a PATH whose first entry is the shim dir and
	// whose second entry holds the fake "real" binaries.
	home := t.TempDir()
	shimDir := filepath.Join(home, ".local", "bin")
	realDir := filepath.Join(home, "real binaries")
	cacheDir := filepath.Join(home, ".cache", "portal")
	for _, d := range []string{shimDir, realDir, cacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	const clipSentinel = "CLIP_PATH_TAKEN"
	const realSentinel = "REAL_BINARY"

	// Fake portald: prints the clip sentinel for ANY clip subcommand and exits
	// 0 (so the shim's `&& exit 0` fires). The shim only invokes it for the
	// intercepted branches, so seeing this sentinel == "branch matched".
	writeExec(t, filepath.Join(cacheDir, "portald"),
		"#!/bin/sh\nprintf '%s'\n", clipSentinel)

	// Fake real xclip/wl-paste: prints the real sentinel. Reached only on
	// fall-through.
	for _, name := range []string{"xclip", "wl-paste"} {
		writeExec(t, filepath.Join(realDir, name),
			"#!/bin/sh\nprintf '%s'\n", realSentinel)
	}

	// Deploy the actual shim scripts the package ships.
	writeExec(t, filepath.Join(shimDir, "xclip"), "%s", xclipShim)
	writeExec(t, filepath.Join(shimDir, "wl-paste"), "%s", wlPasteShim)

	// A deliberately short PATH puts the shim first and the fake real binaries
	// second. The real-binary directory contains a space to pin that PATH entries
	// are handled as data rather than reparsed as shell source.
	path := coreutilsPath(shimDir, realDir)
	run := func(bin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(filepath.Join(shimDir, bin), args...)
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+path)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v (out=%q)", bin, args, err, out)
		}
		return string(out)
	}

	tests := []struct {
		name string
		bin  string
		args []string
		want string // clipSentinel (intercepted) or realSentinel (fell through)
	}{
		// --- xclip: Claude Code's shapes ---
		{"xclip targets intercepted", "xclip",
			[]string{"-selection", "clipboard", "-t", "TARGETS", "-o"}, clipSentinel},
		{"xclip image/png intercepted", "xclip",
			[]string{"-selection", "clipboard", "-t", "image/png", "-o"}, clipSentinel},
		{"xclip image/bmp falls through", "xclip",
			[]string{"-selection", "clipboard", "-t", "image/bmp", "-o"}, realSentinel},
		{"xclip image/jpeg falls through", "xclip",
			[]string{"-selection", "clipboard", "-t", "image/jpeg", "-o"}, realSentinel},
		// Text reads are now intercepted (SPEC A): cc-clip's xclip text surface.
		{"xclip bare -o text intercepted", "xclip",
			[]string{"-selection", "clipboard", "-o"}, clipSentinel},
		{"xclip -t UTF8_STRING -o intercepted", "xclip",
			[]string{"-selection", "clipboard", "-t", "UTF8_STRING", "-o"}, clipSentinel},
		{"xclip -t TEXT -o intercepted", "xclip",
			[]string{"-selection", "clipboard", "-t", "TEXT", "-o"}, clipSentinel},
		{"xclip -t STRING -o intercepted", "xclip",
			[]string{"-selection", "clipboard", "-t", "STRING", "-o"}, clipSentinel},
		{"xclip -t text/plain -o intercepted", "xclip",
			[]string{"-selection", "clipboard", "-t", "text/plain", "-o"}, clipSentinel},
		{"xclip write (-i) falls through", "xclip",
			[]string{"-selection", "clipboard", "-t", "image/png", "-i"}, realSentinel},
		{"xclip text write (-i) falls through", "xclip",
			[]string{"-selection", "clipboard", "-t", "UTF8_STRING", "-i"}, realSentinel},

		// --- wl-paste: opencode's shapes ---
		{"wl-paste list-types intercepted", "wl-paste",
			[]string{"--list-types"}, clipSentinel},
		{"wl-paste --type image/png intercepted", "wl-paste",
			[]string{"--type", "image/png"}, clipSentinel},
		{"wl-paste -t image/png intercepted", "wl-paste",
			[]string{"-t", "image/png"}, clipSentinel},
		{"wl-paste image/bmp falls through", "wl-paste",
			[]string{"--type", "image/bmp"}, realSentinel},
		// Text reads are now intercepted (SPEC A): bare wl-paste + text/* types.
		{"wl-paste bare text intercepted", "wl-paste",
			[]string{}, clipSentinel},
		{"wl-paste --type text/plain intercepted", "wl-paste",
			[]string{"--type", "text/plain"}, clipSentinel},
		{"wl-paste -t text/plain intercepted", "wl-paste",
			[]string{"-t", "text/plain"}, clipSentinel},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := run(tc.bin, tc.args...)
			if !strings.Contains(got, tc.want) {
				t.Errorf("%s %v: got %q, want it to contain %q", tc.bin, tc.args, got, tc.want)
			}
			// Cross-check: the intercepted cases must NOT also hit the real
			// binary, and the fall-through cases must NOT hit the clip path.
			other := clipSentinel
			if tc.want == clipSentinel {
				other = realSentinel
			}
			if strings.Contains(got, other) {
				t.Errorf("%s %v: output %q unexpectedly contains %q (wrong branch)", tc.bin, tc.args, got, other)
			}
		})
	}
}

// TestShimResolversTreatPATHAsData runs every real-binary fallback with PATH
// entries that would execute command substitutions in the former sh -c
// resolver. It also pins resolution from a directory containing spaces.
func TestShimResolversTreatPATHAsData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim scripts are /bin/sh")
	}

	home := t.TempDir()
	shimDir := filepath.Join(home, "shims")
	realDir := filepath.Join(home, "real binaries")
	injectionDirs := []string{
		filepath.Join(home, "$(touch PATH_SUBSTITUTION_RAN)"),
		filepath.Join(home, "`touch PATH_BACKTICK_RAN`"),
	}
	for _, dir := range append([]string{shimDir, realDir}, injectionDirs...) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	const sentinel = "SAFE_REAL_BINARY"
	for _, name := range []string{"xdg-open", "xclip", "wl-paste", "sudo"} {
		writeExec(t, filepath.Join(realDir, name), "#!/bin/sh\nprintf '%s'\n", sentinel)
	}
	writeExec(t, filepath.Join(shimDir, "xdg-open"), "%s", XDGOpenWrapper)
	writeExec(t, filepath.Join(shimDir, "xclip"), "%s", xclipShim)
	writeExec(t, filepath.Join(shimDir, "wl-paste"), "%s", wlPasteShim)
	writeExec(t, filepath.Join(shimDir, "sudo"), "%s", sudoShim)
	askpass := filepath.Join(home, "portal-askpass")
	writeExec(t, askpass, "#!/bin/sh\nexit 0\n")

	pathParts := append([]string{shimDir}, injectionDirs...)
	pathParts = append(pathParts, realDir, "/usr/bin", "/bin")
	path := strings.Join(pathParts, ":")
	tests := []struct {
		name string
		args []string
	}{
		{name: "xdg-open", args: []string{"https://example.invalid"}},
		{name: "xclip", args: []string{"-selection", "clipboard", "-i"}},
		{name: "wl-paste", args: []string{"--clear"}},
		{name: "sudo", args: []string{"whoami"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(shimDir, tc.name), tc.args...)
			cmd.Dir = home
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + path,
				"SUDO_ASKPASS=" + askpass,
			}
			if tc.name == "sudo" {
				detachFromControllingTerminal(cmd)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s resolver: %v (out=%q)", tc.name, err, out)
			}
			if string(out) != sentinel {
				t.Fatalf("%s resolver output = %q, want %q", tc.name, out, sentinel)
			}
		})
	}

	for _, name := range []string{"PATH_SUBSTITUTION_RAN", "PATH_BACKTICK_RAN"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("PATH text executed and created %s", name)
		}
	}
}

func TestShimResolversSkipEmptyEntriesAndRejectSelf(t *testing.T) {
	tests := []struct {
		name   string
		script string
		tool   string
	}{
		{name: "xdg-open", script: XDGOpenWrapper, tool: "xdg-open"},
		{name: "xclip", script: xclipShim, tool: "xclip"},
		{name: "wl-paste", script: wlPasteShim, tool: "wl-paste"},
		{name: "sudo", script: sudoShim, tool: "sudo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range []string{
				`for _d in $PATH; do`,
				`[ -n "$_d" ] || continue`,
				`[ -x "$_d/` + tc.tool + `" ]`,
				`[ -z "$_real" ]`,
			} {
				if !strings.Contains(tc.script, want) {
					t.Errorf("%s resolver missing %q", tc.name, want)
				}
			}
			for _, forbidden := range []string{"xargs -I", "sh -c 'PATH={}"} {
				if strings.Contains(tc.script, forbidden) {
					t.Errorf("%s resolver still contains unsafe %q", tc.name, forbidden)
				}
			}
		})
	}
}

// TestShimFallsThroughWhenNoPortald proves a missing/non-executable portald
// makes the shim fall through to the real binary (the new-shim/old-agent and
// dangling-symlink windows — DESIGN §9.5), never erroring out.
func TestShimFallsThroughWhenNoPortald(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim scripts are /bin/sh")
	}
	home := t.TempDir()
	shimDir := filepath.Join(home, ".local", "bin")
	realDir := filepath.Join(home, "realbin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Note: NO portald written under ~/.cache/portal, so [ -x "$_portald" ] fails.
	const realSentinel = "REAL_BINARY"
	writeExec(t, filepath.Join(realDir, "xclip"), "#!/bin/sh\nprintf '%s'\n", realSentinel)
	writeExec(t, filepath.Join(shimDir, "xclip"), "%s", xclipShim)

	cmd := exec.Command(filepath.Join(shimDir, "xclip"),
		"-selection", "clipboard", "-t", "image/png", "-o")
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+coreutilsPath(shimDir, realDir))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim errored with no portald: %v (out=%q)", err, out)
	}
	if !strings.Contains(string(out), realSentinel) {
		t.Errorf("no-portald: got %q, want fall-through to real binary", out)
	}
}

// coreutilsPath builds a short PATH: the two test dirs first, then the standard
// coreutils locations, filtered to those that exist on this host.
func coreutilsPath(first, second string) string {
	parts := []string{first, second}
	for _, d := range []string{"/usr/bin", "/bin", "/usr/local/bin"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, ":")
}

func detachFromControllingTerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// writeExec writes an executable script from a printf-style template.
func writeExec(t *testing.T, path, format string, args ...any) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf(format, args...)), 0o755); err != nil {
		t.Fatal(err)
	}
}
