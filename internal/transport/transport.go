// Package transport owns the seam between portal's command/forwarding logic
// and the concrete mechanism that reaches the dev box. It declares the core
// Transport interface (transport-agnostic primitives), the optional
// PortForwarder capability, and the value types (Health, Desc, ForwardError)
// they exchange. Three implementations satisfy it: internal/sshctl (the
// system ssh binary, default, behavior-identical), internal/sshnative
// (x/crypto/ssh), and internal/transport/localexec (local subprocess, used by
// the shared conformance suite and dev mode).
//
// Composition rules (enforced by convention + the compiler, not runtime
// checks):
//
//   - bootstrap and clipupload compose their own hardened uploads over
//     Transport.Exec (binary stdin + their own atomic upload scripts); there
//     is deliberately no Uploader capability.
//   - The PortForwarder capability is acquired by a type assertion at the
//     composition root (e.g. tr.(transport.PortForwarder)); the daemon
//     requires it and asserts loudly at wiring time.
//   - NOTHING outside sshctl may gate behavior on Pid > 0 — liveness gates use
//     Health.Up. Pid is impl-specific ground truth that only the system ssh
//     transport fills.
//   - Forward/Cancel/ListForwards/ForwardLines are PortForwarder-only and are
//     NEVER added to the core Transport interface. A compile error on a
//     forwarding call is resolved by routing the call through the
//     PortForwarder, not by widening Transport.
package transport

import (
	"context"
	"fmt"
	"io"
)

// Health is the liveness snapshot of a transport. Up is the sole liveness
// signal callers outside sshctl may gate on. Pid is impl-specific ground
// truth where one exists (the system ssh transport fills the ControlMaster
// pid; the native and localexec transports fill 0). Detail is the human
// string (the system ssh transport uses "pid=N").
type Health struct {
	Up     bool
	Pid    int
	Detail string
}

// Desc identifies a transport for status/log rendering. Impl is one of
// "system-ssh", "native-ssh", or "localexec".
type Desc struct {
	Impl     string
	Host     string
	Endpoint string
}

// Transport is the transport-agnostic core. It is EXACTLY these six methods
// and never grows forwarding methods — Forward/Cancel/ListForwards/
// ForwardLines live ONLY on PortForwarder.
type Transport interface {
	// Ensure brings the transport up if it is down (idempotent). rebuilt is
	// true iff THIS call performed the (re)build, so the caller can emit the
	// "master established" log line.
	Ensure(ctx context.Context) (rebuilt bool, err error)

	// Health reports liveness. Callers outside sshctl gate on Health.Up, not
	// Pid.
	Health(ctx context.Context) (Health, error)

	// Exec runs a command on the TARGET and returns its captured stdout and
	// stderr. A non-zero exit returns an error whose message includes the
	// exit code and trimmed stderr (stdout/stderr strings are still returned).
	//
	// ARGV CONTRACT: argv is joined with single ASCII spaces into ONE command
	// string that a shell on the TARGET executes — exactly an ssh session's
	// command semantics (the remote login shell re-splits the joined string).
	// Callers who need multiple tokens, redirection, globbing, or any shell
	// metacharacter preserved MUST pre-quote them into a single argv element
	// (this is what bootstrap/clipupload/doctor already do via their
	// shellQuote helpers, e.g. tr.Exec(ctx, "", "bash", "-c",
	// doctorShellQuote(script))). All three implementations honor this
	// shell-join model: sshctl APPENDS argv verbatim as trailing args and lets
	// the ssh binary join+send them (it MUST NOT wrap in sh -c); sshnative
	// joins argv and passes the string to ssh.Session; localexec joins argv
	// and runs sh -c <joined>. Consequence: a bare multi-token argv like
	// Exec(ctx,nil,"sh","-c","echo x >&2") is NOT portable — quote it as
	// Exec(ctx,nil,"sh","-c",shellQuote("echo x >&2")).
	Exec(ctx context.Context, stdin []byte, argv ...string) (stdout, stderr string, err error)

	// Stream runs argv on the TARGET with live stdin/stdout/stderr pipes; the
	// caller closes stdin to signal EOF and wait returns the target command's
	// exit error after the streams close. Used by agentclient for the
	// long-lived portald RPC pipe.
	//
	// ARGV CONTRACT: identical to Exec — argv is joined with single ASCII
	// spaces into ONE command string a shell on the TARGET executes (the
	// remote login shell re-splits it). Callers needing multiple tokens,
	// redirection, globbing, or any shell metacharacter preserved MUST
	// pre-quote them into a single argv element. All three implementations
	// honor this shell-join model: sshctl APPENDS argv verbatim (the ssh
	// binary joins; it MUST NOT wrap in sh -c); sshnative joins and passes to
	// ssh.Session; localexec joins and runs sh -c <joined>. Consequence: a
	// bare multi-token argv like Stream(ctx,"sh","-c","echo x >&2") is NOT
	// portable — quote it.
	Stream(ctx context.Context, argv ...string) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, wait func() error, err error)

	// Close tears the transport down. stopped is true iff there was something
	// to stop.
	Close(ctx context.Context) (stopped bool, err error)

	// Describe returns identifying metadata for status/log rendering.
	Describe() Desc
}

// PortForwarder is the optional local-port-forwarding capability. It is
// acquired by type assertion at the composition root; localexec does NOT
// implement it (forwarding to yourself is meaningless).
type PortForwarder interface {
	Forward(ctx context.Context, local, remote int) error
	Cancel(ctx context.Context, local, remote int) error
	ListForwards(ctx context.Context) ([]int, error)
	ForwardLines(ctx context.Context) ([]string, error)
}

// ForwardError reports a forward/cancel failure surfaced via stderr. This is
// the canonical home; the sshctl copy is deleted in u2.
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
