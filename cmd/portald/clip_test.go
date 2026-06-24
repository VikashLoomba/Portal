package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// --- pure-helper tests ----------------------------------------------------

func TestParseClipReply(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantOK    bool
		wantPayld string
	}{
		{"ok sha", "ok\tdeadbeefdeadbeefdeadbeefdeadbeef\n", true, "deadbeefdeadbeefdeadbeefdeadbeef"},
		{"ok targets", "ok\timage/png\n", true, "image/png"},
		{"ok crlf", "ok\timage/png\r\n", true, "image/png"},
		{"none", "none\n", false, ""},
		{"rejected", "rejected\n", false, ""},
		{"dropped", "dropped\n", false, ""},
		{"no-client", "no-client\n", false, ""},
		{"empty", "", false, ""},
		{"ok no payload", "ok\t\n", false, ""},
		{"ok no tab", "ok\n", false, ""},
		{"bare ok no tab no nl", "ok", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := parseClipReply(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && r.payload != tc.wantPayld {
				t.Fatalf("payload = %q, want %q", r.payload, tc.wantPayld)
			}
		})
	}
}

func TestShaRE(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef" // 32 hex
	if !shaRE.MatchString(good) {
		t.Fatalf("32-hex sha should match")
	}
	bad := []string{
		"",
		"0123456789abcdef0123456789abcde",   // 31
		"0123456789abcdef0123456789abcdef0", // 33
		"0123456789ABCDEF0123456789abcdef",  // uppercase
		"../../../../etc/passwd",
		"0123456789abcdef0123456789abcde/", // slash
		"g123456789abcdef0123456789abcdef", // non-hex
	}
	for _, b := range bad {
		if shaRE.MatchString(b) {
			t.Fatalf("%q should NOT match shaRE", b)
		}
	}
}

func TestVerifyPNG(t *testing.T) {
	if err := verifyPNG(append([]byte(nil), pngMagic...)); err != nil {
		t.Fatalf("valid magic rejected: %v", err)
	}
	if err := verifyPNG([]byte("not a png at all")); err == nil {
		t.Fatalf("non-png accepted")
	}
	if err := verifyPNG([]byte{0x89}); err == nil {
		t.Fatalf("short buffer accepted")
	}
}

// --- end-to-end exit-code semantics via a subprocess ----------------------
//
// runClip / emitClipFile call os.Exit, so we exercise them by building the
// portald binary once and invoking `portald clip ...` with a controlled $HOME
// and a fake cmd socket. This is the only honest way to assert exit codes +
// byte-exact stdout (DESIGN §6.1).

// buildPortald compiles the current package into a temp binary.
func buildPortald(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "portald")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build portald: %v\n%s", err, out)
	}
	return bin
}

// fakeAgentSock listens on cmd-<pid>.sock under dir and answers the first
// connection with reply, then stops. Returns a stop func.
func fakeAgentSock(t *testing.T, dir, reply string) func() {
	t.Helper()
	sock := filepath.Join(dir, "cmd-test.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte(reply))
			conn.Close()
		}
	}()
	return func() { l.Close(); <-done }
}

// clipDir is where emitClipFile reconstructs side-channel files: $HOME/.cache/portal/clip.
func clipDir(home string) string { return filepath.Join(home, ".cache", "portal", "clip") }

// fakeAgentSockNamed is fakeAgentSock with a caller-chosen socket basename so a
// test can stand up two distinct connected sockets.
func fakeAgentSockNamed(t *testing.T, dir, base, reply string) func() {
	t.Helper()
	sock := filepath.Join(dir, base)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", base, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte(reply))
			conn.Close()
		}
	}()
	return func() { l.Close(); <-done }
}

// runClipBin invokes the built binary as `portald clip args...` with HOME=home,
// where the binary lives in `bindir` so its os.Executable()-derived socket dir
// matches the fake socket's dir. Returns (stdout, exitCode).
func runClipBin(t *testing.T, bin, home string, args ...string) ([]byte, int) {
	t.Helper()
	full := append([]string{"clip"}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
		}
	}
	return out.Bytes(), code
}

// setupClipHome creates a HOME whose cache dir is also the binary's dir, so the
// fake socket (placed next to the binary) is discovered by clipFanout. We copy
// the built binary into $HOME/.cache/portal so os.Executable() → that dir.
func setupClipHome(t *testing.T, srcBin string) (home, bin string) {
	t.Helper()
	// Use a SHORT temp base, not t.TempDir(): the long test-name-derived paths
	// blow past macOS's 104-byte sun_path limit when we listen on a unix socket
	// under $HOME/.cache/portal (a test-environment constraint, not a code one).
	var err error
	home, err = os.MkdirTemp("", "clp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cacheDir := filepath.Join(home, ".cache", "portal")
	if err := os.MkdirAll(filepath.Join(cacheDir, "clip"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(cacheDir, "portald")
	data, err := os.ReadFile(srcBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, bin
}

func TestRunClip_ImageValidPNG(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	png := append(append([]byte(nil), pngMagic...), []byte("PNGBODYbytes")...)
	sha := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(clipDir(home), "clip-"+sha+".png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := fakeAgentSock(t, cacheDir, "ok\t"+sha+"\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "image", "png")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout=%q)", code, out)
	}
	if !bytes.Equal(out, png) {
		t.Fatalf("stdout not byte-exact:\n got %x\nwant %x", out, png)
	}
}

func TestRunClip_ImageWrongMagic(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	sha := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(clipDir(home), "clip-"+sha+".png"),
		[]byte("NOTAPNGFILE!"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := fakeAgentSock(t, cacheDir, "ok\t"+sha+"\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "image", "png")
	if code == 0 {
		t.Fatalf("wrong magic must exit non-zero, got 0 (stdout=%q)", out)
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty on failure, got %q", out)
	}
}

func TestRunClip_ImageZeroByte(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	sha := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(clipDir(home), "clip-"+sha+".png"),
		[]byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := fakeAgentSock(t, cacheDir, "ok\t"+sha+"\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "image", "png")
	if code == 0 {
		t.Fatalf("0-byte file must exit non-zero")
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty, got %q", out)
	}
}

func TestRunClip_ImageNonHexSHA(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	// Agent returns a non-hex SHA: portald must reject before touching disk.
	stop := fakeAgentSock(t, cacheDir, "ok\t../../etc/passwd\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "image", "png")
	if code == 0 {
		t.Fatalf("non-hex sha must exit non-zero")
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty, got %q", out)
	}
}

func TestRunClip_ImageSymlinkRefused(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	// Plant a symlink at the reconstructed path pointing at a secret file.
	secret := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(secret, append(append([]byte(nil), pngMagic...), []byte("SECRET")...), 0o600); err != nil {
		t.Fatal(err)
	}
	sha := "0123456789abcdef0123456789abcdef"
	link := filepath.Join(clipDir(home), "clip-"+sha+".png")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	stop := fakeAgentSock(t, cacheDir, "ok\t"+sha+"\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "image", "png")
	if code == 0 {
		t.Fatalf("symlink in clip dir must be refused (O_NOFOLLOW), got exit 0 stdout=%q", out)
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty on symlink refusal, got %q", out)
	}
}

// TestRunClip_TargetsByteExact: the agent answers the CANONICAL kind ("image"),
// and portald maps it to the requesting tool's target line(s). For xclip
// (default/bare form) an image clipboard advertises image/png byte-exact.
func TestRunClip_TargetsByteExact(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	stop := fakeAgentSock(t, cacheDir, "ok\timage\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "targets")
	if code != 0 {
		t.Fatalf("targets exit = %d, want 0", code)
	}
	if string(out) != "image/png\n" {
		t.Fatalf("targets stdout = %q, want exactly %q", out, "image/png\n")
	}
}

// TestRunClip_TargetsText: a text clipboard maps to the tool's text target
// names — xclip's UTF8_STRING/TEXT/STRING, wl-paste's text/plain.
func TestRunClip_TargetsText(t *testing.T) {
	cases := []struct {
		tool string
		want string
	}{
		{"xclip", "UTF8_STRING\nTEXT\nSTRING\n"},
		{"wl-paste", "text/plain\n"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			src := buildPortald(t)
			home, bin := setupClipHome(t, src)
			cacheDir := filepath.Join(home, ".cache", "portal")

			stop := fakeAgentSock(t, cacheDir, "ok\ttext\n")
			defer stop()

			out, code := runClipBin(t, bin, home, "targets", tc.tool)
			if code != 0 {
				t.Fatalf("targets %s exit = %d, want 0", tc.tool, code)
			}
			if string(out) != tc.want {
				t.Fatalf("targets %s stdout = %q, want %q", tc.tool, out, tc.want)
			}
		})
	}
}

func TestRunClip_NoneFallsThrough(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	stop := fakeAgentSock(t, cacheDir, "none\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "image", "png")
	if code == 0 {
		t.Fatalf("none reply must exit non-zero")
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty, got %q", out)
	}
}

func TestRunClip_NoSocket(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	// No fake socket at all.
	out, code := runClipBin(t, bin, home, "targets")
	if code == 0 {
		t.Fatalf("no socket must exit non-zero")
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty, got %q", out)
	}
}

// TestRunClip_MultiClientRefused: two DISTINCT connected agent sockets must
// make portald refuse (exit 1, no output) rather than guess whose clipboard to
// serve (DESIGN §7.3), even if both would answer ok.
func TestRunClip_MultiClientRefused(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	stop1 := fakeAgentSockNamed(t, cacheDir, "cmd-1.sock", "ok\timage/png\n")
	defer stop1()
	stop2 := fakeAgentSockNamed(t, cacheDir, "cmd-2.sock", "ok\timage/png\n")
	defer stop2()

	out, code := runClipBin(t, bin, home, "targets")
	if code == 0 {
		t.Fatalf(">1 connected client must be refused, got exit 0 stdout=%q", out)
	}
	if len(out) != 0 {
		t.Fatalf("stdout must be empty on multi-client refusal, got %q", out)
	}
}

func TestRunClip_TextValid(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	body := []byte("hello clipboard text\n")
	sha := "fedcba9876543210fedcba9876543210"
	if err := os.WriteFile(filepath.Join(clipDir(home), "text-"+sha+".txt"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := fakeAgentSock(t, cacheDir, "ok\t"+sha+"\n")
	defer stop()

	out, code := runClipBin(t, bin, home, "text")
	if code != 0 {
		t.Fatalf("text exit = %d, want 0 (stdout=%q)", code, out)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("text stdout = %q, want %q", out, body)
	}
}
