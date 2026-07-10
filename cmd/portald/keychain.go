package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/VikashLoomba/Portal/pkg/agent"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

const (
	keychainExitDenied  = 111
	keychainExitTimeout = 112
	keychainExitUsage   = 2

	keychainReadTimeout = 140 * time.Second
	keychainReplyLimit  = 8192
	keychainContextMax  = 300
	keychainSecretMax   = 4096
	defaultAskpassLabel = "sudo on this box"
)

var (
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	denyReasonRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

type keychainRuntime struct {
	stdout      io.Writer
	stderr      io.Writer
	request     func(agent.CredShimReq) ([]byte, string)
	requester   func() string
	lookPath    func(string) (string, error)
	execProcess func(string, []string, []string) error
	environ     func() []string
	runStdin    func(string, []string, []byte, io.Writer, io.Writer) (int, error)
}

type credentialReply struct {
	secret []byte
	reason string
	ok     bool
}

func productionKeychainRuntime() keychainRuntime {
	return keychainRuntime{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		request:     requestCredential,
		requester:   requesterContext,
		lookPath:    exec.LookPath,
		execProcess: syscall.Exec,
		environ:     os.Environ,
		runStdin:    runStdinChild,
	}
}

// runKeychain dispatches the box-side credential subcommands and returns the
// process exit code. The env-mode success path does not return because
// syscall.Exec replaces portald with the child.
func runKeychain(args []string) int {
	return runKeychainWithRuntime(args, productionKeychainRuntime())
}

func runKeychainWithRuntime(args []string, rt keychainRuntime) int {
	if len(args) == 0 {
		fmt.Fprintln(rt.stderr, "portal keychain: missing subcommand (run or askpass)")
		writeKeychainHelp(rt.stderr)
		return keychainExitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeKeychainHelp(rt.stdout)
		return 0
	case "run":
		return runKeychainRun(args[1:], rt)
	case "askpass":
		return runKeychainAskpass(args[1:], rt)
	default:
		fmt.Fprintf(rt.stderr, "portal keychain: unknown subcommand %q\n", args[0])
		writeKeychainHelp(rt.stderr)
		return keychainExitUsage
	}
}

func runKeychainRun(args []string, rt keychainRuntime) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		writeKeychainRunHelp(rt.stdout)
		return 0
	}

	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return keychainRunUsageError(rt.stderr, "missing -- before the child command")
	}

	fs := flag.NewFlagSet("keychain run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	label := fs.String("label", "", "credential label shown on the Mac")
	envName := fs.String("env", "", "set the approved secret in this child environment variable")
	useStdin := fs.Bool("stdin", false, "send the approved secret plus a newline to child stdin")
	if err := fs.Parse(args[:separator]); err != nil {
		return keychainRunUsageError(rt.stderr, err.Error())
	}
	if fs.NArg() != 0 {
		return keychainRunUsageError(rt.stderr, "unexpected argument before --")
	}
	child := args[separator+1:]
	if *label == "" {
		return keychainRunUsageError(rt.stderr, "--label is required")
	}
	if (*envName == "") == !*useStdin {
		return keychainRunUsageError(rt.stderr, "choose exactly one of --env NAME or --stdin")
	}
	if *envName != "" && (len(*envName) > keychainContextMax || !envNameRE.MatchString(*envName)) {
		return keychainRunUsageError(rt.stderr, "invalid environment variable name")
	}
	if len(child) == 0 {
		return keychainRunUsageError(rt.stderr, "child command is empty")
	}
	path, err := rt.lookPath(child[0])
	if err != nil {
		return keychainRunUsageError(rt.stderr, fmt.Sprintf("child command %q was not found", child[0]))
	}

	mode := "stdin"
	target := truncateUTF8Bytes(strings.Join(child, " "), keychainContextMax)
	if *envName != "" {
		mode = "env"
		target = *envName
	}
	secret, reason := rt.request(agent.CredShimReq{
		Label: *label, Mode: mode, Target: target, Requester: rt.requester(),
	})
	if reason != "" {
		return reportCredentialFailure(rt.stderr, reason)
	}
	defer clear(secret)

	if mode == "env" {
		env := appendEnvOverride(rt.environ(), *envName, string(secret))
		if err := rt.execProcess(path, child, env); err != nil {
			fmt.Fprintf(rt.stderr, "portal keychain: could not start child: %v\n", err)
			return keychainExitUsage
		}
		return 0
	}

	code, err := rt.runStdin(path, child, secret, rt.stdout, rt.stderr)
	if err != nil {
		fmt.Fprintf(rt.stderr, "portal keychain: could not start child: %v\n", err)
		return keychainExitUsage
	}
	return code
}

func appendEnvOverride(environ []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}

func runKeychainAskpass(args []string, rt keychainRuntime) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		writeKeychainAskpassHelp(rt.stdout)
		return 0
	}
	prompt := truncateUTF8Bytes(strings.Join(args, " "), keychainContextMax)
	secret, reason := rt.request(agent.CredShimReq{
		Label: defaultAskpassLabel, Mode: "askpass", Target: prompt,
		Requester: rt.requester(),
	})
	if reason != "" {
		return reportCredentialFailure(rt.stderr, reason)
	}
	defer clear(secret)

	out := make([]byte, len(secret)+1)
	copy(out, secret)
	out[len(out)-1] = '\n'
	err := writeFull(rt.stdout, out)
	clear(out)
	if err != nil {
		fmt.Fprintln(rt.stderr, "portal keychain: could not write the approved secret to askpass stdout")
		return keychainExitDenied
	}
	return 0
}

func runStdinChild(path string, argv []string, secret []byte, stdout, stderr io.Writer) (int, error) {
	input := make([]byte, len(secret)+1)
	copy(input, secret)
	input[len(input)-1] = '\n'
	defer clear(input)

	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code, nil
		}
	}
	return 0, err
}

func requestCredential(req agent.CredShimReq) ([]byte, string) {
	return requestCredentialFromSockets(req, cmdSocketEntries())
}

func requestCredentialFromSockets(req agent.CredShimReq, sockets []string) ([]byte, string) {
	payload, err := protocol.MarshalPayload(req)
	if err != nil {
		return nil, "label-invalid"
	}
	line := "cred\t" + base64.StdEncoding.EncodeToString(payload) + "\n"
	answer, state := singleAgentFanout(
		sockets, line, clipDialTimeout, keychainReadTimeout, keychainReplyLimit,
	)
	switch state {
	case fanoutNoAgent:
		return nil, "no-client"
	case fanoutMultipleAgents:
		return nil, "multiple-clients"
	}
	if answer.raw == "" {
		return nil, "timeout"
	}
	parsed, ok := parseCredentialReply(answer.raw)
	if !ok {
		if answer.readErr != nil {
			return nil, "timeout"
		}
		return nil, "invalid-response"
	}
	if !parsed.ok {
		return nil, parsed.reason
	}
	return parsed.secret, ""
}

func parseCredentialReply(raw string) (credentialReply, bool) {
	line := strings.TrimRight(raw, "\r\n")
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return credentialReply{}, false
	}
	verb, payload, hasTab := strings.Cut(line, "\t")
	if !hasTab {
		return credentialReply{}, false
	}
	switch verb {
	case "ok":
		secret, err := base64.StdEncoding.DecodeString(payload)
		if err != nil || len(secret) > keychainSecretMax {
			return credentialReply{}, false
		}
		return credentialReply{secret: secret, ok: true}, true
	case "deny":
		if !denyReasonRE.MatchString(payload) {
			return credentialReply{}, false
		}
		return credentialReply{reason: payload}, true
	default:
		return credentialReply{}, false
	}
}

func reportCredentialFailure(stderr io.Writer, reason string) int {
	switch reason {
	case "timeout":
		fmt.Fprintln(stderr, "portal keychain: timed out waiting for approval on the Mac")
		return keychainExitTimeout
	case "no-client":
		fmt.Fprintln(stderr, "portal keychain: no Mac is connected — is portal running on the Mac?")
	case "busy":
		fmt.Fprintln(stderr, "portal keychain: another credential request is pending — wait for it to finish, then retry")
	case "disabled":
		fmt.Fprintln(stderr, "portal keychain: credential sharing is disabled — run 'portal features cred on' on the Mac")
	case "multiple-clients":
		fmt.Fprintln(stderr, "portal keychain: more than one Mac is connected — close extra portal sessions, then retry")
	case "cooldown":
		fmt.Fprintln(stderr, "portal keychain: approval cooldown is active — wait a few seconds, then retry")
	case "gui-unavailable":
		fmt.Fprintln(stderr, "portal keychain: the Mac approval dialog is unavailable — sign in to the Mac and retry")
	case "label-invalid":
		fmt.Fprintln(stderr, "portal keychain: the credential label or request context is invalid")
	case "invalid-response":
		fmt.Fprintln(stderr, "portal keychain: the connected agent returned an invalid credential response")
	default:
		fmt.Fprintf(stderr, "portal keychain: denied by user on the Mac (reason: %s)\n", reason)
	}
	return keychainExitDenied
}

func requesterContext() string {
	return requesterContextForPID(os.Getppid())
}

func requesterContextForPID(pid int) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return formatRequesterContext(pid, raw)
}

func formatRequesterContext(pid int, raw []byte) string {
	cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
	cmdline = strings.TrimSpace(strings.ToValidUTF8(cmdline, ""))
	if cmdline == "" {
		return ""
	}
	return truncateUTF8Bytes(fmt.Sprintf("pid %d: %s", pid, cmdline), keychainContextMax)
}

func truncateUTF8Bytes(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "")
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func keychainRunUsageError(stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "portal keychain: %s\n", message)
	fmt.Fprintln(stderr, "usage: portal keychain run --label <L> (--env NAME | --stdin) -- <command> [args...]")
	fmt.Fprintln(stderr, "run 'portal keychain run --help' for agent-oriented examples")
	return keychainExitUsage
}

func isHelpArg(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help"
}

func writeKeychainHelp(w io.Writer) {
	fmt.Fprint(w, `portal keychain requests a credential from the user on the connected Mac without printing it to the agent.

Usage:
  portal keychain run --label <L> (--env NAME | --stdin) -- <command> [args...]
  portal keychain askpass [prompt...]

Examples:
  portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" …'
  portal keychain run --label "database password" --stdin -- psql
  portal keychain askpass "Password:"

The SINGLE quotes in the first example make the child shell expand $PW. The caller's shell must not expand it.

Exit codes:
  child code  keychain run succeeded and the child exited with this code
  0           askpass wrote the approved secret to stdout
  111         request denied or unavailable
  112         approval timed out
  2           usage error

Use 'portal keychain run --help' or 'portal keychain askpass --help' for details.
`)
}

func writeKeychainRunHelp(w io.Writer) {
	fmt.Fprint(w, `Request a credential, then deliver it only to a child process.

Usage:
  portal keychain run --label <L> --env NAME -- <command> [args...]
  portal keychain run --label <L> --stdin -- <command> [args...]

Examples:
  portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" …'
  portal keychain run --label "database password" --stdin -- psql

The SINGLE quotes in the first example make the child shell expand $PW. The caller's shell must not expand it.
In --env mode portald is replaced by the child. In --stdin mode the child reads exactly the secret plus one newline, then EOF.

Exit codes:
  child code  the child process's exact exit code, including non-zero
  111         request denied or unavailable
  112         approval timed out
  2           usage error
`)
}

func writeKeychainAskpassHelp(w io.Writer) {
	fmt.Fprint(w, `Serve one sudo/askpass prompt through the connected Mac approval dialog.

Usage:
  portal keychain askpass [prompt...]

Example:
  portal keychain askpass "Password:"

On approval, stdout contains only the secret plus one newline. Diagnostics go to stderr; denials never write the secret.

Exit codes:
  0    approved secret written to stdout
  111  request denied or unavailable
  112  approval timed out
  2    usage error
`)
}
