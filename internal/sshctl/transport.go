// Package sshctl wraps the SSH ControlMaster lifecycle and the multiplexed
// commands the rest of portal sends across it. It is the single place that
// owns every empirically-derived SSH gotcha:
//
//   - `ssh -O check` prints "Master running (pid=N)" to STDERR.
//   - `ssh -O forward/cancel` exit codes are unreliable; success is
//     determined by the absence of the substring "request failed" in stderr.
//   - A crashed master leaves a stale ControlPath socket; rm -f before
//     rebuilding.
//   - "localhost:PORT" as the remote target reaches both IPv4- and
//     IPv6-bound servers (preferred over 127.0.0.1).
//
// The Transport interface is the swap point: a future native x/crypto/ssh
// implementation can satisfy it with no changes to the reconcile engine.
package sshctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/run"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/transport"
)

// MasterForwardSource enumerates the local LISTEN ports/lines a given master
// pid owns. It is structurally satisfied by *proc.Lsof; sshctl does NOT import
// proc — the composition root injects the concrete lister via SSH.Forwards.
type MasterForwardSource interface {
	MasterForwards(ctx context.Context, pid int) ([]int, error)
	MasterForwardLines(ctx context.Context, pid int) ([]string, error)
}

// Transport is the SSH surface consumed by the reconcile engine and the
// remote port discoverer.
type Transport interface {
	// MasterPID returns the live ControlMaster pid, or 0 if down.
	MasterPID(ctx context.Context) (int, error)
	// EnsureMaster rebuilds if the master is down (rm stale sock first),
	// then returns the live pid AND a boolean indicating whether THIS call
	// performed the rebuild — the caller logs "master established (pid=N)"
	// in that case, matching the bash original.
	EnsureMaster(ctx context.Context) (pid int, rebuilt bool, err error)
	// Forward adds `-O forward -L local:localhost:remote`. Returns
	// *ForwardError if stderr contains "request failed".
	Forward(ctx context.Context, local, remote int) error
	// Cancel removes the forward (`-O cancel`). Best-effort.
	Cancel(ctx context.Context, local, remote int) error
	// Exit tears the master down (`-O exit`) and removes the socket.
	// Returns true iff the master responded (i.e. there was one to stop).
	Exit(ctx context.Context) (stopped bool, err error)
	// Exec runs argv on the remote over the master, returning stdout.
	Exec(ctx context.Context, stdin string, argv ...string) (string, error)
	// ExecBytes is like Exec but accepts arbitrary binary stdin (used by
	// bootstrap to upload the agent binary via `cat > tmp`). Returns
	// stdout, stderr, and any error.
	ExecBytes(ctx context.Context, stdin []byte, argv ...string) (stdout, stderr string, err error)
	// ExecStream spawns ssh ... argv with live pipes — caller closes stdin
	// to signal EOF; wait() returns ssh's exit error after streams close.
	// Used by the agentclient to spawn the long-lived portald RPC pipe.
	ExecStream(ctx context.Context, argv ...string) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, wait func() error, err error)
	// Host returns the configured ssh host (used by log messages).
	Host() string
	// Sock returns the ControlPath socket (used by log messages).
	Sock() string
}

// ForwardError reports a forward/cancel failure surfaced via stderr.
type ForwardError struct {
	Port   int
	Stderr string // cleaned: \r stripped, \n collapsed to spaces
}

func (e *ForwardError) Error() string {
	if e.Port == 0 {
		return "ssh: request failed: " + e.Stderr
	}
	return fmt.Sprintf("ssh: request failed for port %d: %s", e.Port, e.Stderr)
}

// SSH is the production Transport, shelling out to the system ssh binary.
type SSH struct {
	SockPath string
	HostID   string // ssh host (alias from ~/.ssh/config or user@hostname)
	Opts     []string
	Runner   run.Runner
	// StderrSink, if non-nil, receives a copy of every ssh invocation's
	// stderr that is not consumed for protocol-level decision-making (i.e.
	// not -O check / -O forward / -O cancel). This makes ssh warnings
	// (host-key churn, mux events, transient errors) visible in the launchd
	// log, matching the bash daemon where ssh's stderr inherits the
	// daemon's stderr by default.
	StderrSink io.Writer
	// Forwards backs the PortForwarder List/Lines methods via lsof against the
	// live master pid. Injected by the composition root (structurally
	// *proc.Lsof) so sshctl need not import proc.
	Forwards MasterForwardSource
}

func New(sock, host string, opts []string, r run.Runner) *SSH {
	return &SSH{SockPath: sock, HostID: host, Opts: opts, Runner: r}
}

func (s *SSH) Host() string { return s.HostID }
func (s *SSH) Sock() string { return s.SockPath }

var pidRe = regexp.MustCompile(`pid=([0-9]+)`)

// MasterPID: `ssh -O check -S sock host`. The pid line is on STDERR (load-
// bearing) — combine both streams when scanning.
func (s *SSH) MasterPID(ctx context.Context) (int, error) {
	stdout, stderr, _, err := s.Runner.Run(ctx, "ssh",
		[]string{"-O", "check", "-S", s.SockPath, s.HostID}, "")
	if err != nil {
		return 0, nil
	}
	combined := stdout + stderr
	m := pidRe.FindStringSubmatch(combined)
	if m == nil {
		return 0, nil
	}
	n, _ := atoi(m[1])
	return n, nil
}

// EnsureMaster: if the master is down, rm the stale socket and start a fresh
// `ssh -fN -M -S sock -o ControlPersist=yes <opts...> host`. Re-checks the
// pid afterwards. Returns 0+nil if the rebuild attempt completed but the
// master still isn't responsive (the engine logs a warning and tries again
// next pass — same behavior as the bash original). The bool return is true
// iff THIS call performed a rebuild, so the caller can emit the
// "master established (pid=N)" log line that bash emits inline.
func (s *SSH) EnsureMaster(ctx context.Context) (int, bool, error) {
	pid, _ := s.MasterPID(ctx)
	if pid != 0 {
		return pid, false, nil
	}
	_ = os.Remove(s.SockPath)

	args := []string{"-fN", "-M", "-S", s.SockPath, "-o", "ControlPersist=yes"}
	args = append(args, s.Opts...)
	args = append(args, s.HostID)
	_, stderr, _, err := s.Runner.Run(ctx, "ssh", args, "")
	if err != nil {
		s.teeStderr(stderr)
		return 0, false, nil
	}
	s.teeStderr(stderr)
	// `ssh -O check` immediately after `-fN -M` can race with the master
	// daemonizing; the bash version slept 1s before re-checking. Match that.
	select {
	case <-time.After(1 * time.Second):
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}
	pid, _ = s.MasterPID(ctx)
	return pid, pid != 0, nil
}

// Forward: `ssh -O forward -S sock -L local:localhost:remote host`. Exit
// codes here are unreliable (a partial conflict can still exit 0, an
// already-forwarded port can exit non-zero), so we IGNORE the exit code and
// decide on stderr.
func (s *SSH) Forward(ctx context.Context, local, remote int) error {
	spec := fmt.Sprintf("%d:localhost:%d", local, remote)
	_, stderr, _, err := s.Runner.Run(ctx, "ssh",
		[]string{"-O", "forward", "-S", s.SockPath, "-L", spec, s.HostID}, "")
	if err != nil {
		return err
	}
	if strings.Contains(stderr, "request failed") {
		return &ForwardError{Port: local, Stderr: cleanStderr(stderr)}
	}
	return nil
}

// Cancel: best-effort, errors discarded (matches bash >/dev/null 2>&1).
func (s *SSH) Cancel(ctx context.Context, local, remote int) error {
	spec := fmt.Sprintf("%d:localhost:%d", local, remote)
	_, _, _, _ = s.Runner.Run(ctx, "ssh",
		[]string{"-O", "cancel", "-S", s.SockPath, "-L", spec, s.HostID}, "")
	return nil
}

// Exit: `ssh -O exit -S sock host`, then rm sock. Returns stopped=true iff
// the master responded (i.e. there was one running). Mirrors bash's
// `ssh -O exit && echo "master stopped"` behavior — the message is only
// printed when there was a master to stop.
func (s *SSH) Exit(ctx context.Context) (bool, error) {
	_, _, code, err := s.Runner.Run(ctx, "ssh",
		[]string{"-O", "exit", "-S", s.SockPath, s.HostID}, "")
	stopped := err == nil && code == 0
	_ = os.Remove(s.SockPath)
	return stopped, nil
}

// Exec runs a command on the remote over the master. argv[0] is the program
// (typically "bash"); argv[1:] are its arguments. stdin is sent on stdin.
// Used by RemoteDiscoverer for the `bash -s -- ...` ss/awk script. ssh's
// stderr is tee'd to StderrSink (when set) on success so launchd-routed
// warnings — host-key churn, mux events, etc. — surface in the daemon log
// (matching bash, where ssh inherits the daemon's stderr by default).
func (s *SSH) Exec(ctx context.Context, stdin string, argv ...string) (string, error) {
	args := []string{"-S", s.SockPath}
	args = append(args, s.Opts...)
	args = append(args, s.HostID)
	args = append(args, argv...)
	stdout, stderr, code, err := s.Runner.Run(ctx, "ssh", args, stdin)
	if err != nil {
		return stdout, err
	}
	if code != 0 {
		return stdout, fmt.Errorf("ssh exec exit %d: %s", code, strings.TrimSpace(stderr))
	}
	s.teeStderr(stderr)
	return stdout, nil
}

func (s *SSH) teeStderr(stderr string) {
	if s.StderrSink == nil || strings.TrimSpace(stderr) == "" {
		return
	}
	io.WriteString(s.StderrSink, stderr)
}

// ExecBytes runs argv on the remote over the master with arbitrary binary
// stdin. Used by bootstrap to upload the agent binary via `cat > tmp`.
func (s *SSH) ExecBytes(ctx context.Context, stdin []byte, argv ...string) (string, string, error) {
	args := []string{"-S", s.SockPath}
	args = append(args, s.Opts...)
	args = append(args, s.HostID)
	args = append(args, argv...)
	stdoutStr, stderrStr, code, err := s.runBytes(ctx, args, stdin)
	if err != nil {
		return stdoutStr, stderrStr, err
	}
	if code != 0 {
		return stdoutStr, stderrStr, fmt.Errorf("ssh exec exit %d: %s", code, strings.TrimSpace(stderrStr))
	}
	s.teeStderr(stderrStr)
	return stdoutStr, stderrStr, nil
}

// ExecStream spawns the ssh client with live stdin/stdout/stderr pipes.
// Caller is responsible for closing stdin (or just exiting) to terminate.
// wait() returns the ssh exit error AFTER all three streams have been
// drained or closed by the caller.
func (s *SSH) ExecStream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
	args := []string{"-S", s.SockPath}
	args = append(args, s.Opts...)
	args = append(args, s.HostID)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, nil, err
	}
	wait := func() error { return cmd.Wait() }
	return stdin, stdout, stderr, wait, nil
}

// runBytes is the binary-stdin variant of run. Implemented inline here to
// avoid spreading exec.Cmd plumbing across packages — the OSRunner only
// supports string stdin, which would force a base64 round-trip.
func (s *SSH) runBytes(ctx context.Context, args []string, stdin []byte) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return outBuf.String(), errBuf.String(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return outBuf.String(), errBuf.String(), ee.ExitCode(), nil
	}
	return outBuf.String(), errBuf.String(), -1, err
}

// Validate tests key-based ssh connectivity to host by running `ssh host true`.
// Unlike the daemon's ssh calls, this does NOT use BatchMode=yes — doing so
// would suppress output from tools like Tailscale that print an auth URL to
// stderr and wait for the user to visit it. stderrW receives the raw stderr
// stream in real time (pass os.Stderr during install so the user can see and
// act on any prompts). Returns nil iff the connection succeeded.
// Does NOT touch the ControlMaster socket — this runs before there is one.
func (s *SSH) Validate(ctx context.Context, host string, stderrW io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "ConnectTimeout=30", host, "true")
	cmd.Stderr = stderrW
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// HasSS verifies the remote has the `ss` command on PATH. Best-effort warning
// for non-Linux dev boxes (BSD has `sockstat`, macOS has `lsof`, etc.).
func (s *SSH) HasSS(ctx context.Context, host string) bool {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, "command -v ss >/dev/null 2>&1"}
	_, _, code, err := s.Runner.Run(ctx, "ssh", args, "")
	return err == nil && code == 0
}

func cleanStderr(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("bad int")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

var _ Transport = (*SSH)(nil)

// --- transport.Transport / transport.PortForwarder dual-stack (u1) ---
//
// These methods are added ALONGSIDE the legacy interface, which is still
// consumed everywhere until u2. Each is a byte-compat wrapper over the
// existing private logic. Exec (signature clash) and ExecStream->Stream
// (rename) are the two methods that cannot be dual-stacked; they migrate in
// u2. No `var _ transport.Transport`/`var _ transport.PortForwarder` assertion
// is added yet because Exec/Stream have not migrated.

// Ensure wraps EnsureMaster, discarding the pid and reporting only whether
// this call rebuilt the master.
func (s *SSH) Ensure(ctx context.Context) (bool, error) {
	_, rebuilt, err := s.EnsureMaster(ctx)
	return rebuilt, err
}

// Health reports liveness from the `ssh -O check` pid. Detail is EXACT
// ("pid=N") so status/log output renders byte-identically.
func (s *SSH) Health(ctx context.Context) (transport.Health, error) {
	pid, err := s.MasterPID(ctx)
	if err != nil {
		return transport.Health{}, err
	}
	if pid <= 0 {
		return transport.Health{Up: false, Pid: 0, Detail: ""}, nil
	}
	return transport.Health{Up: true, Pid: pid, Detail: fmt.Sprintf("pid=%d", pid)}, nil
}

// Close wraps Exit.
func (s *SSH) Close(ctx context.Context) (bool, error) {
	return s.Exit(ctx)
}

// Describe identifies the system ssh transport.
func (s *SSH) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: s.HostID, Endpoint: s.SockPath}
}

// ListForwards reads the live master pid and returns the ports it forwards via
// the injected MasterForwardSource. Returns nil when the master is down.
func (s *SSH) ListForwards(ctx context.Context) ([]int, error) {
	pid, err := s.MasterPID(ctx)
	if err != nil {
		return nil, err
	}
	if pid == 0 {
		return nil, nil
	}
	return s.Forwards.MasterForwards(ctx, pid)
}

// ForwardLines is the verbatim-lines analogue of ListForwards.
func (s *SSH) ForwardLines(ctx context.Context) ([]string, error) {
	pid, err := s.MasterPID(ctx)
	if err != nil {
		return nil, err
	}
	if pid == 0 {
		return nil, nil
	}
	return s.Forwards.MasterForwardLines(ctx, pid)
}
