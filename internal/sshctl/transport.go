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
// *SSH is the system-ssh implementation of transport.Transport (the core swap
// point owned by internal/transport) plus the transport.PortForwarder
// capability. A future native x/crypto/ssh implementation satisfies the same
// interfaces with no changes to the reconcile engine.
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

// SSH is the production transport, shelling out to the system ssh binary. It
// implements transport.Transport + transport.PortForwarder.
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
	// daemon's stderr by default. Caller-set: the u5 selection-aware factory
	// passes it through explicitly; it is never defaulted here.
	StderrSink io.Writer
	// Forwards backs the PortForwarder List/Lines methods via lsof against the
	// live master pid. Injected by the composition root (structurally
	// *proc.Lsof) so sshctl need not import proc.
	Forwards MasterForwardSource
}

func New(sock, host string, opts []string, r run.Runner) *SSH {
	return &SSH{SockPath: sock, HostID: host, Opts: opts, Runner: r}
}

var pidRe = regexp.MustCompile(`pid=([0-9]+)`)

// masterPID: `ssh -O check -S sock host`. The pid line is on STDERR (load-
// bearing) — combine both streams when scanning. Returns 0 if down.
func (s *SSH) masterPID(ctx context.Context) (int, error) {
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

// ensureMaster: if the master is down, rm the stale socket and start a fresh
// `ssh -fN -M -S sock -o ControlPersist=yes <opts...> host`. Re-checks the
// pid afterwards. Returns 0+nil if the rebuild attempt completed but the
// master still isn't responsive (the engine logs a warning and tries again
// next pass — same behavior as the bash original). The bool return is true
// iff THIS call performed a rebuild, so the caller can emit the
// "master established (pid=N)" log line that bash emits inline.
func (s *SSH) ensureMaster(ctx context.Context) (int, bool, error) {
	pid, _ := s.masterPID(ctx)
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
	pid, _ = s.masterPID(ctx)
	return pid, pid != 0, nil
}

// Ensure brings the master up if it is down (idempotent), reporting only
// whether this call performed the (re)build.
func (s *SSH) Ensure(ctx context.Context) (bool, error) {
	_, rebuilt, err := s.ensureMaster(ctx)
	return rebuilt, err
}

// Health reports liveness from the `ssh -O check` pid. Detail is EXACT
// ("pid=N") so status/log output renders byte-identically.
func (s *SSH) Health(ctx context.Context) (transport.Health, error) {
	pid, err := s.masterPID(ctx)
	if err != nil {
		return transport.Health{}, err
	}
	if pid <= 0 {
		return transport.Health{Up: false, Pid: 0, Detail: ""}, nil
	}
	return transport.Health{Up: true, Pid: pid, Detail: fmt.Sprintf("pid=%d", pid)}, nil
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
		return &transport.ForwardError{Port: local, Stderr: cleanStderr(stderr)}
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

// exit: `ssh -O exit -S sock host`, then rm sock. Returns stopped=true iff
// the master responded (i.e. there was one running). Mirrors bash's
// `ssh -O exit && echo "master stopped"` behavior — the message is only
// printed when there was a master to stop.
func (s *SSH) exit(ctx context.Context) (bool, error) {
	_, _, code, err := s.Runner.Run(ctx, "ssh",
		[]string{"-O", "exit", "-S", s.SockPath, s.HostID}, "")
	stopped := err == nil && code == 0
	_ = os.Remove(s.SockPath)
	return stopped, nil
}

// Close tears the master down.
func (s *SSH) Close(ctx context.Context) (bool, error) {
	return s.exit(ctx)
}

// Describe identifies the system ssh transport.
func (s *SSH) Describe() transport.Desc {
	return transport.Desc{Impl: "system-ssh", Host: s.HostID, Endpoint: s.SockPath}
}

func (s *SSH) teeStderr(stderr string) {
	if s.StderrSink == nil || strings.TrimSpace(stderr) == "" {
		return
	}
	io.WriteString(s.StderrSink, stderr)
}

// Exec runs argv on the remote over the master with arbitrary binary stdin.
// argv is APPENDED VERBATIM as trailing args to the ssh invocation, letting the
// ssh binary perform the space-join + remote re-shell (the shell-join model);
// sshctl MUST NOT wrap in `sh -c`. Callers who need shell metacharacters
// pre-quote them into a single argv element (bootstrap/clipupload/doctor do this
// via their shellQuote helpers). ssh's stderr is tee'd to StderrSink (when set)
// on success so launchd-routed warnings surface in the daemon log.
func (s *SSH) Exec(ctx context.Context, stdin []byte, argv ...string) (string, string, error) {
	args := []string{"-S", s.SockPath}
	args = append(args, s.Opts...)
	args = append(args, s.HostID)
	args = append(args, argv...)
	var (
		stdoutStr, stderrStr string
		code                 int
		err                  error
	)
	if len(stdin) == 0 {
		// No binary payload: route through the injected Runner. Byte-identical
		// to the legacy string-Exec path (which the deleted Exec(string) used),
		// and observable by the fake Runner in tests.
		stdoutStr, stderrStr, code, err = s.Runner.Run(ctx, "ssh", args, "")
	} else {
		// Binary stdin: bypass the string-only Runner via runBytes (the legacy
		// ExecBytes path) so arbitrary bytes reach the remote intact.
		stdoutStr, stderrStr, code, err = s.runBytes(ctx, args, stdin)
	}
	if err != nil {
		return stdoutStr, stderrStr, err
	}
	if code != 0 {
		return stdoutStr, stderrStr, fmt.Errorf("ssh exec exit %d: %s", code, strings.TrimSpace(stderrStr))
	}
	s.teeStderr(stderrStr)
	return stdoutStr, stderrStr, nil
}

// Stream spawns the ssh client with live stdin/stdout/stderr pipes. Caller is
// responsible for closing stdin (or just exiting) to terminate. wait() returns
// the ssh exit error AFTER all three streams have been drained or closed by the
// caller. Used by the agentclient to spawn the long-lived portald RPC pipe.
//
// argv follows the SAME shell-join contract as Exec: it is APPENDED VERBATIM as
// trailing args and the ssh binary performs the space-join + remote re-shell —
// no sh -c wrapping. Callers needing shell metacharacters pre-quote them into a
// single argv element.
func (s *SSH) Stream(ctx context.Context, argv ...string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, func() error, error) {
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

// ListForwards reads the live master pid and returns the ports it forwards via
// the injected MasterForwardSource. Returns nil when the master is down.
func (s *SSH) ListForwards(ctx context.Context) ([]int, error) {
	pid, err := s.masterPID(ctx)
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
	pid, err := s.masterPID(ctx)
	if err != nil {
		return nil, err
	}
	if pid == 0 {
		return nil, nil
	}
	return s.Forwards.MasterForwardLines(ctx, pid)
}

// Validate tests key-based ssh connectivity to host by running `ssh host true`.
// Unlike the daemon's ssh calls, this does NOT use BatchMode=yes — doing so
// would suppress output from tools like Tailscale that print an auth URL to
// stderr and wait for the user to visit it. stderrW receives the raw stderr
// stream in real time (pass os.Stderr during install so the user can see and
// act on any prompts). Returns nil iff the connection succeeded.
// Does NOT touch the ControlMaster socket — this runs before there is one.
// Not part of the transport interface (system-ssh-specific preflight).
func (s *SSH) Validate(ctx context.Context, host string, stderrW io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "ConnectTimeout=30", host, "true")
	cmd.Stderr = stderrW
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// HasSS verifies the remote has the `ss` command on PATH. Best-effort warning
// for non-Linux dev boxes (BSD has `sockstat`, macOS has `lsof`, etc.). Not
// part of the transport interface (system-ssh-specific preflight).
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

var (
	_ transport.Transport     = (*SSH)(nil)
	_ transport.PortForwarder = (*SSH)(nil)
)
