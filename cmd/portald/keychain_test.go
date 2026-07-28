package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/VikashLoomba/Portal/pkg/agent"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

type fakeCredentialSocket struct {
	path     string
	requests chan string
	stop     func()
}

func startFakeCredentialSocket(t *testing.T, dir, name, reply string) fakeCredentialSocket {
	t.Helper()
	path := filepath.Join(dir, name)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	requests := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			line, _ := bufio.NewReader(conn).ReadString('\n')
			requests <- line
			_, _ = io.WriteString(conn, reply)
			_ = conn.Close()
		}
	}()
	return fakeCredentialSocket{
		path:     path,
		requests: requests,
		stop: func() {
			_ = listener.Close()
			<-done
		},
	}
}

func shortKeychainSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func decodeCredSocketLine(t *testing.T, line string) agent.CredShimReq {
	t.Helper()
	line = strings.TrimSuffix(line, "\n")
	verb, encoded, ok := strings.Cut(line, "\t")
	if !ok || verb != "cred" {
		t.Fatalf("cmd-socket line = %q, want cred\\t<base64>", line)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode request base64: %v", err)
	}
	req, err := protocol.UnmarshalPayload[agent.CredShimReq](raw)
	if err != nil {
		t.Fatalf("decode CredShimReq: %v", err)
	}
	return req
}

func baseTestKeychainRuntime(stdout, stderr io.Writer) keychainRuntime {
	return keychainRuntime{
		stdout: stdout,
		stderr: stderr,
		request: func(agent.CredShimReq) ([]byte, string) {
			return nil, "no-client"
		},
		requester:      func() string { return "pid 99: test-agent" },
		parentName:     func() string { return "sudo" },
		allowAnyParent: func() bool { return false },
		lookPath: func(name string) (string, error) {
			return "/resolved/" + name, nil
		},
		execProcess: func(string, []string, []string) error { return nil },
		environ:     func() []string { return []string{"BASE=1", "PW=old"} },
		runStdin: func(string, []string, []byte, io.Writer, io.Writer) (int, error) {
			return 0, nil
		},
	}
}

func runKeychainBin(t *testing.T, bin, home string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	env := []string{"HOME=" + home}
	for len(args) > 0 && strings.Contains(args[0], "=") && !strings.HasPrefix(args[0], "-") {
		env = append(env, args[0])
		args = args[1:]
	}
	full := append([]string{"keychain"}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		if cmd.ProcessState == nil {
			t.Fatalf("run keychain: %v", err)
		}
		code = cmd.ProcessState.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func TestCredentialSocketFramingRoundTrip(t *testing.T) {
	if keychainReadTimeout != 140*time.Second {
		t.Fatalf("keychain read timeout = %v, want 140s", keychainReadTimeout)
	}
	dir := shortKeychainSocketDir(t)
	secret := []byte{0x00, 0x01, 0xfe, 0xff, '\n'}
	fake := startFakeCredentialSocket(t, dir, "cmd-one.sock",
		"ok\t"+base64.StdEncoding.EncodeToString(secret)+"\n")
	defer fake.stop()

	wantReq := agent.CredShimReq{
		Label: "staging admin", Mode: "env", Target: "PW",
		Requester: "pid 99: sh -c curl",
	}
	gotSecret, reason := requestCredentialFromSockets(wantReq, []string{fake.path})
	if reason != "" {
		t.Fatalf("request reason = %q, want success", reason)
	}
	if !bytes.Equal(gotSecret, secret) {
		t.Fatalf("secret = %x, want %x", gotSecret, secret)
	}
	select {
	case line := <-fake.requests:
		if gotReq := decodeCredSocketLine(t, line); gotReq != wantReq {
			t.Fatalf("CredShimReq = %+v, want %+v", gotReq, wantReq)
		}
	case <-time.After(time.Second):
		t.Fatal("fake agent did not receive credential request")
	}
}

func TestCredentialSocketSingleAgentRefusal(t *testing.T) {
	dir := shortKeychainSocketDir(t)
	reply := "ok\t" + base64.StdEncoding.EncodeToString([]byte("secret")) + "\n"
	first := startFakeCredentialSocket(t, dir, "cmd-one.sock", reply)
	defer first.stop()
	second := startFakeCredentialSocket(t, dir, "cmd-two.sock", reply)
	defer second.stop()

	secret, reason := requestCredentialFromSockets(
		agent.CredShimReq{Label: "label", Mode: "stdin"},
		[]string{first.path, second.path},
	)
	if len(secret) != 0 || reason != "multiple-clients" {
		t.Fatalf("multi-agent result = secret %q reason %q, want empty/multiple-clients", secret, reason)
	}
}

func TestParseCredentialReply(t *testing.T) {
	secret := []byte{0x00, 0xff, '\n'}
	okLine := "ok\t" + base64.StdEncoding.EncodeToString(secret) + "\n"
	got, ok := parseCredentialReply(okLine)
	if !ok || !got.ok || !bytes.Equal(got.secret, secret) {
		t.Fatalf("parsed ok reply = %+v, %v", got, ok)
	}

	for _, reason := range []string{
		"denied", "timeout", "disabled", "cooldown", "gui-unavailable",
		"label-invalid", "no-client", "busy",
	} {
		got, ok := parseCredentialReply("deny\t" + reason + "\n")
		if !ok || got.ok || got.reason != reason {
			t.Errorf("deny %q parsed as %+v, %v", reason, got, ok)
		}
	}

	overCap := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, keychainSecretMax+1))
	for _, raw := range []string{
		"", "ok\n", "ok\t%%%\n", "deny\n", "deny\tbad reason\n",
		"deny\tdenied\nextra", "ok\t" + overCap + "\n",
	} {
		if _, ok := parseCredentialReply(raw); ok {
			t.Errorf("malformed reply %q was accepted", raw)
		}
	}
}

func TestKeychainRunEnvModeExecBoundary(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rt := baseTestKeychainRuntime(&stdout, &stderr)
	secret := []byte("env-secret")
	var gotReq agent.CredShimReq
	rt.request = func(req agent.CredShimReq) ([]byte, string) {
		gotReq = req
		return append([]byte(nil), secret...), ""
	}
	var gotPath string
	var gotArgv []string
	var gotEnv []string
	rt.execProcess = func(path string, argv, env []string) error {
		gotPath = path
		gotArgv = append([]string(nil), argv...)
		gotEnv = append([]string(nil), env...)
		return nil
	}

	code := runKeychainWithRuntime([]string{
		"run", "--label", "staging admin", "--env", "PW", "--",
		"sh", "-c", "use-child-env",
	}, rt)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotReq != (agent.CredShimReq{
		Label: "staging admin", Mode: "env", Target: "PW", Requester: "pid 99: test-agent",
	}) {
		t.Fatalf("CredShimReq = %+v", gotReq)
	}
	if gotPath != "/resolved/sh" || strings.Join(gotArgv, "|") != "sh|-c|use-child-env" {
		t.Fatalf("exec path/argv = %q / %v", gotPath, gotArgv)
	}
	if len(gotEnv) != 2 || gotEnv[0] != "BASE=1" || gotEnv[1] != "PW=env-secret" {
		t.Fatalf("child env = %v, want BASE then approved PW", gotEnv)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("portald emitted secret/diagnostic on success: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestKeychainRunStdinExactEOFAndExitCode(t *testing.T) {
	path, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rt := baseTestKeychainRuntime(&stdout, &stderr)
	rt.lookPath = func(string) (string, error) { return path, nil }
	rt.request = func(req agent.CredShimReq) ([]byte, string) {
		if req.Mode != "stdin" || req.Target != "sh -c cat; exit 37" {
			t.Errorf("stdin CredShimReq = %+v", req)
		}
		return []byte("stdin-secret"), ""
	}
	rt.runStdin = runStdinChild

	code := runKeychainWithRuntime([]string{
		"run", "--label", "database", "--stdin", "--", "sh", "-c", "cat; exit 37",
	}, rt)
	if code != 37 {
		t.Fatalf("exit code = %d, want child code 37", code)
	}
	if stdout.String() != "stdin-secret\n" {
		t.Fatalf("child stdout = %q, want exact secret+newline", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestKeychainAskpassSuccess(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rt := baseTestKeychainRuntime(&stdout, &stderr)
	longPrompt := strings.Repeat("é", 200)
	var gotReq agent.CredShimReq
	rt.request = func(req agent.CredShimReq) ([]byte, string) {
		gotReq = req
		return []byte("askpass-secret"), ""
	}

	code := runKeychainWithRuntime([]string{"askpass", longPrompt}, rt)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.String() != "askpass-secret\n" || stderr.Len() != 0 {
		t.Fatalf("askpass streams = stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if gotReq.Label != defaultAskpassLabel || gotReq.Mode != "askpass" || gotReq.Requester != "pid 99: test-agent" {
		t.Fatalf("askpass CredShimReq = %+v", gotReq)
	}
	if len(gotReq.Target) > keychainContextMax || !utf8.ValidString(gotReq.Target) {
		t.Fatalf("askpass target length/UTF-8 = %d/%v", len(gotReq.Target), utf8.ValidString(gotReq.Target))
	}

	stdout.Reset()
	stderr.Reset()
	rt.request = func(req agent.CredShimReq) ([]byte, string) {
		gotReq = req
		return []byte("empty-prompt-secret"), ""
	}
	if code := runKeychainWithRuntime([]string{"askpass"}, rt); code != 0 {
		t.Fatalf("empty-prompt exit = %d", code)
	}
	if gotReq.Label != defaultAskpassLabel || gotReq.Target != "" {
		t.Fatalf("empty-prompt CredShimReq = %+v", gotReq)
	}
}

// REGRESSION: a lone help token must print help and NEVER request a credential.
// Treating it as opaque prompt text (the previous behavior) fired a live request
// at the Mac when an agent ran `portal keychain askpass --help`, and the approved
// secret was written to stdout straight into that agent's context.
func TestKeychainAskpassHelpTokenPrintsHelpAndNeverRequests(t *testing.T) {
	for _, token := range []string{"help", "-h", "--help"} {
		t.Run(token, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rt := baseTestKeychainRuntime(&stdout, &stderr)
			requests := 0
			rt.request = func(agent.CredShimReq) ([]byte, string) {
				requests++
				return []byte("MUST-NOT-BE-SERVED"), ""
			}

			if code := runKeychainWithRuntime([]string{"askpass", token}, rt); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if requests != 0 {
				t.Fatalf("credential requests = %d, want 0 for a help token", requests)
			}
			if strings.Contains(stdout.String(), "MUST-NOT-BE-SERVED") {
				t.Fatal("a secret reached stdout on the help path")
			}
			if !strings.Contains(stdout.String(), "SUDO_ASKPASS") {
				t.Fatalf("help output = %q, want askpass usage", stdout.String())
			}
		})
	}
}

// A help token alongside real prompt text stays prompt text: sudo passed it.
func TestKeychainAskpassHelpTokenWithOtherArgsStaysAPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rt := baseTestKeychainRuntime(&stdout, &stderr)
	var got agent.CredShimReq
	rt.request = func(req agent.CredShimReq) ([]byte, string) {
		got = req
		return []byte("secret"), ""
	}

	if code := runKeychainWithRuntime([]string{"askpass", "--help", "me"}, rt); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got.Target != "--help me" {
		t.Fatalf("target = %q, want the full prompt text", got.Target)
	}
}

// REGRESSION: only a real askpass consumer may receive the secret on stdout.
// A shell or an agent's tool runner must be refused WITHOUT prompting, so no
// human is asked to approve a request whose output goes into a transcript.
func TestKeychainAskpassRefusesNonConsumerParent(t *testing.T) {
	for _, parent := range []string{"bash", "zsh", "sh", "node", "python3", ""} {
		t.Run("parent="+parent, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rt := baseTestKeychainRuntime(&stdout, &stderr)
			rt.parentName = func() string { return parent }
			requests := 0
			rt.request = func(agent.CredShimReq) ([]byte, string) {
				requests++
				return []byte("MUST-NOT-BE-SERVED"), ""
			}

			code := runKeychainWithRuntime([]string{"askpass", "Password:"}, rt)
			if code != keychainExitUsage {
				t.Fatalf("exit = %d, want %d", code, keychainExitUsage)
			}
			if requests != 0 {
				t.Fatalf("credential requests = %d, want 0 — no prompt may reach the Mac", requests)
			}
			if strings.Contains(stdout.String(), "MUST-NOT-BE-SERVED") || stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing written", stdout.String())
			}
			if !strings.Contains(stderr.String(), "refusing") {
				t.Fatalf("stderr = %q, want an explanation", stderr.String())
			}
		})
	}
}

func TestKeychainAskpassServesEveryConsumerParent(t *testing.T) {
	for _, parent := range []string{"sudo", "sudoedit", "sudo-rs", "ssh"} {
		t.Run(parent, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rt := baseTestKeychainRuntime(&stdout, &stderr)
			rt.parentName = func() string { return parent }
			rt.request = func(agent.CredShimReq) ([]byte, string) { return []byte("ok-secret"), "" }

			if code := runKeychainWithRuntime([]string{"askpass", "Password:"}, rt); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if stdout.String() != "ok-secret\n" {
				t.Fatalf("stdout = %q, want the secret", stdout.String())
			}
		})
	}
}

func TestKeychainAskpassAllowAnyParentOverride(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rt := baseTestKeychainRuntime(&stdout, &stderr)
	rt.parentName = func() string { return "some-other-askpass-consumer" }
	rt.allowAnyParent = func() bool { return true }
	rt.request = func(agent.CredShimReq) ([]byte, string) { return []byte("ok-secret"), "" }

	if code := runKeychainWithRuntime([]string{"askpass", "Password:"}, rt); code != 0 {
		t.Fatalf("exit = %d, want 0 under the override", code)
	}
	if stdout.String() != "ok-secret\n" {
		t.Fatalf("stdout = %q, want the secret", stdout.String())
	}
}

func TestProcessNameFromCmdline(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"sudo\x00whoami\x00", "sudo"},
		{"/usr/bin/sudo\x00-A\x00ls\x00", "sudo"},
		{"bash\x00", "bash"},
		{"", ""},
		{"\x00\x00", ""},
	}
	for _, tt := range tests {
		if got := processNameFromCmdline([]byte(tt.raw)); got != tt.want {
			t.Errorf("processNameFromCmdline(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
func TestKeychainDenyReasons(t *testing.T) {
	tests := []struct {
		reason string
		code   int
		stderr string
	}{
		{"denied", 111, "portal keychain: denied by user on the Mac (reason: denied)\n"},
		{"timeout", 112, "portal keychain: timed out waiting for approval on the Mac\n"},
		{"disabled", 111, "portal keychain: credential sharing is disabled — run 'portal features cred on' on the Mac\n"},
		{"cooldown", 111, "portal keychain: approval cooldown is active — wait a few seconds, then retry\n"},
		{"gui-unavailable", 111, "portal keychain: the Mac approval dialog is unavailable — sign in to the Mac and retry\n"},
		{"label-invalid", 111, "portal keychain: the credential label or request context is invalid\n"},
		{"no-client", 111, "portal keychain: no Mac is connected — is portal running on the Mac?\n"},
		{"busy", 111, "portal keychain: another credential request is pending — wait for it to finish, then retry\n"},
		{"multiple-clients", 111, "portal keychain: more than one Mac is connected — close extra portal sessions, then retry\n"},
		{"invalid-response", 111, "portal keychain: the connected agent returned an invalid credential response\n"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			rt := baseTestKeychainRuntime(&stdout, &stderr)
			rt.request = func(agent.CredShimReq) ([]byte, string) { return nil, tt.reason }
			if got := runKeychainWithRuntime([]string{"askpass", "Password:"}, rt); got != tt.code {
				t.Fatalf("exit code = %d, want %d", got, tt.code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("denial wrote stdout %q", stdout.String())
			}
			if stderr.String() != tt.stderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.stderr)
			}
		})
	}
}

func TestKeychainUsageBeforeCredentialRequest(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing subcommand", nil},
		{"unknown subcommand", []string{"unknown"}},
		{"missing separator", []string{"run", "--label", "x", "--stdin", "sh"}},
		{"missing label", []string{"run", "--stdin", "--", "sh"}},
		{"missing delivery mode", []string{"run", "--label", "x", "--", "sh"}},
		{"both delivery modes", []string{"run", "--label", "x", "--env", "PW", "--stdin", "--", "sh"}},
		{"invalid env name", []string{"run", "--label", "x", "--env", "9-PW", "--", "sh"}},
		{"oversized env name", []string{"run", "--label", "x", "--env", strings.Repeat("P", 301), "--", "sh"}},
		{"empty child", []string{"run", "--label", "x", "--stdin", "--"}},
		{"argument before separator", []string{"run", "--label", "x", "--stdin", "extra", "--", "sh"}},
		{"child not found", []string{"run", "--label", "x", "--stdin", "--", "missing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			rt := baseTestKeychainRuntime(&stdout, &stderr)
			requests := 0
			rt.request = func(agent.CredShimReq) ([]byte, string) {
				requests++
				return []byte("must-not-request"), ""
			}
			rt.lookPath = func(name string) (string, error) {
				if name == "missing" {
					return "", errors.New("not found")
				}
				return "/resolved/" + name, nil
			}
			if code := runKeychainWithRuntime(tt.args, rt); code != keychainExitUsage {
				t.Fatalf("exit code = %d, want %d", code, keychainExitUsage)
			}
			if requests != 0 {
				t.Fatalf("usage error made %d credential requests", requests)
			}
			if stderr.Len() == 0 {
				t.Fatal("usage error did not explain itself on stderr")
			}
		})
	}
}

func TestKeychainHelpIsAgentFirst(t *testing.T) {
	const quoteExample = `portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" …'`
	tests := []struct {
		args  []string
		check []string
	}{
		{
			args:  []string{"--help"},
			check: []string{quoteExample, "SINGLE quotes", "111", "112", "usage error", "opaque prompt text"},
		},
		{
			args:  []string{"run", "--help"},
			check: []string{quoteExample, "caller's shell must not expand it", "child process's exact exit code"},
		},
	}
	for _, tt := range tests {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		rt := baseTestKeychainRuntime(&stdout, &stderr)
		if code := runKeychainWithRuntime(tt.args, rt); code != 0 {
			t.Fatalf("help %v exit = %d", tt.args, code)
		}
		for _, want := range tt.check {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("help %v missing %q\n%s", tt.args, want, stdout.String())
			}
		}
		if stderr.Len() != 0 {
			t.Errorf("help %v stderr = %q", tt.args, stderr.String())
		}
	}
}

func TestRequesterContextFormattingAndTruncation(t *testing.T) {
	raw := []byte("agent\x00--flag\x00" + strings.Repeat("é", 200) + "\x00")
	got := formatRequesterContext(4242, raw)
	if !strings.HasPrefix(got, "pid 4242: agent --flag ") {
		t.Fatalf("requester = %q", got)
	}
	if strings.ContainsRune(got, '\x00') || len(got) > keychainContextMax || !utf8.ValidString(got) {
		t.Fatalf("requester NUL/length/UTF-8 = %v/%d/%v", strings.ContainsRune(got, '\x00'), len(got), utf8.ValidString(got))
	}
	if got := formatRequesterContext(1, nil); got != "" {
		t.Fatalf("empty cmdline requester = %q, want empty", got)
	}
	if got := requesterContextForPID(-1); got != "" {
		t.Fatalf("missing /proc requester = %q, want empty", got)
	}
}

func TestKeychainAskpassMainDispatch(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	topHelp := exec.Command(bin, "--help")
	topHelp.Env = append(os.Environ(), "HOME="+home)
	var topHelpOut bytes.Buffer
	topHelp.Stderr = &topHelpOut
	if err := topHelp.Run(); err != nil {
		t.Fatalf("top-level --help: %v", err)
	}
	if !strings.Contains(topHelpOut.String(), "portald keychain <run|askpass> [options]") {
		t.Fatalf("top-level help missing keychain line:\n%s", topHelpOut.String())
	}

	secret := []byte("dispatch-secret")
	fake := startFakeCredentialSocket(t, cacheDir, "cmd-askpass.sock",
		"ok\t"+base64.StdEncoding.EncodeToString(secret)+"\n")
	defer fake.stop()

	// The real parent here is the test binary, not sudo, so the askpass
	// consumer gate would (correctly) refuse. Use the documented override to
	// exercise the dispatch path itself; TestKeychainAskpassBinRefusesTestParent
	// pins the gate for the un-overridden case.
	stdout, stderr, code := runKeychainBin(t, bin, home, askpassAllowAnyEnv+"=1", "askpass", "Password:")
	if code != 0 || stdout != "dispatch-secret\n" || stderr != "" {
		t.Fatalf("dispatch result = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	select {
	case line := <-fake.requests:
		req := decodeCredSocketLine(t, line)
		if req.Label != defaultAskpassLabel || req.Mode != "askpass" || req.Target != "Password:" {
			t.Fatalf("dispatch CredShimReq = %+v", req)
		}
		if len(req.Requester) > keychainContextMax {
			t.Fatalf("dispatch requester length = %d", len(req.Requester))
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch fake agent received no request")
	}
}

func TestKeychainRunStdinMainDispatchAndExitPropagation(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	secret := []byte("main-stdin-secret")
	fake := startFakeCredentialSocket(t, cacheDir, "cmd-stdin.sock",
		"ok\t"+base64.StdEncoding.EncodeToString(secret)+"\n")
	defer fake.stop()

	stdout, stderr, code := runKeychainBin(t, bin, home,
		"run", "--label", "database", "--stdin", "--", "sh", "-c", "cat; exit 37")
	if code != 37 || stdout != "main-stdin-secret\n" || stderr != "" {
		t.Fatalf("stdin dispatch = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	select {
	case line := <-fake.requests:
		req := decodeCredSocketLine(t, line)
		if req.Label != "database" || req.Mode != "stdin" || req.Target != "sh -c cat; exit 37" {
			t.Fatalf("stdin dispatch CredShimReq = %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin dispatch fake agent received no request")
	}
}

// The shipped binary must refuse when its real parent is not an askpass
// consumer — here, the test runner. This is the end-to-end form of the leak
// that put a live password into a coding agent's context.
func TestKeychainAskpassBinRefusesTestParent(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")

	secret := []byte("MUST-NOT-BE-SERVED")
	fake := startFakeCredentialSocket(t, cacheDir, "cmd-askpass-gate.sock",
		"ok\t"+base64.StdEncoding.EncodeToString(secret)+"\n")
	defer fake.stop()

	stdout, stderr, code := runKeychainBin(t, bin, home, "askpass", "Password:")
	if code != keychainExitUsage {
		t.Fatalf("exit = %d, want %d", code, keychainExitUsage)
	}
	if strings.Contains(stdout, "MUST-NOT-BE-SERVED") || stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "refusing") {
		t.Fatalf("stderr = %q, want an explanation", stderr)
	}
	select {
	case <-fake.requests:
		t.Fatal("a credential request reached the agent despite the gate")
	case <-time.After(300 * time.Millisecond):
	}
}

// The shipped binary must print help — not request — for a lone help token.
func TestKeychainAskpassBinHelpTokenPrintsHelp(t *testing.T) {
	src := buildPortald(t)
	home, bin := setupClipHome(t, src)
	cacheDir := filepath.Join(home, ".cache", "portal")
	fake := startFakeCredentialSocket(t, cacheDir, "cmd-askpass-help.sock",
		"ok\t"+base64.StdEncoding.EncodeToString([]byte("MUST-NOT-BE-SERVED"))+"\n")
	defer fake.stop()

	stdout, _, code := runKeychainBin(t, bin, home, "askpass", "--help")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "SUDO_ASKPASS") || strings.Contains(stdout, "MUST-NOT-BE-SERVED") {
		t.Fatalf("stdout = %q, want askpass help and no secret", stdout)
	}
	select {
	case <-fake.requests:
		t.Fatal("a credential request reached the agent on the help path")
	case <-time.After(300 * time.Millisecond):
	}
}
