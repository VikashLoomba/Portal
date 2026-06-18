// Package proc inspects local processes/sockets via lsof. It is the source of
// truth for the reconcile loop's "currently forwarded" set: MasterForwards
// reads the master ssh process's actual LISTEN sockets each pass, never
// caching, so a daemon restart or master rebuild self-corrects rather than
// leaking stale state.
package proc

import (
	"context"
	"sort"
	"strings"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/run"
)

// PortLister is the dependency the reconcile engine consumes.
type PortLister interface {
	// MasterForwards returns the local LISTEN ports owned by masterPID,
	// sorted unique. Empty if pid is 0 or the master has no forwards.
	MasterForwards(ctx context.Context, masterPID int) ([]int, error)
	// MasterForwardLines returns the lsof NAME column entries (col 9)
	// for every LISTEN socket the master owns, sort-uniqued by full
	// string. Used by the `status` command to show entries verbatim
	// (e.g. "[::1]:8080", "127.0.0.1:8080", "*:8080") — bash pipes lsof
	// output through `awk '{print $9}' | sort -u`, so a port bound to
	// both IPv4 and IPv6 produces TWO lines.
	MasterForwardLines(ctx context.Context, masterPID int) ([]string, error)
	// LocalHolder returns the pid of whatever holds a LISTEN socket on
	// `port` locally, or 0 if free.
	LocalHolder(ctx context.Context, port int) (int, error)
	// ProcessName resolves pid → comm via `ps` for conflict log messages.
	// Returns "" on failure.
	ProcessName(ctx context.Context, pid int) string
}

// Lsof is the production PortLister.
type Lsof struct {
	Path   string // /usr/sbin/lsof
	Runner run.Runner
}

func New(path string, r run.Runner) *Lsof { return &Lsof{Path: path, Runner: r} }

// MasterForwards: lsof -nP -iTCP -sTCP:LISTEN -a -p <pid>. Parses NAME column
// (col 9), extracts the port via portFromLsofName, sorts unique. Errors from
// lsof return an empty slice (matches bash 2>/dev/null + awk pipeline) so the
// reconcile loop treats "can't enumerate" the same as "nothing forwarded".
func (l *Lsof) MasterForwards(ctx context.Context, pid int) ([]int, error) {
	if pid <= 0 {
		return nil, nil
	}
	stdout, _, _, err := l.Runner.Run(ctx, l.Path, []string{
		"-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", itoa(pid),
	}, "")
	if err != nil {
		return nil, nil
	}
	seen := make(map[int]struct{})
	out := make([]int, 0, 8)
	for i, line := range strings.Split(stdout, "\n") {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		port, ok := portFromLsofName(fields[8])
		if !ok {
			continue
		}
		if _, dup := seen[port]; dup {
			continue
		}
		seen[port] = struct{}{}
		out = append(out, port)
	}
	sort.Ints(out)
	return out, nil
}

// MasterForwardLines runs the same lsof query as MasterForwards but returns
// the verbatim NAME column entries (column 9), sorted unique by full string.
// Used for the `status` command's "active forwards" output to match the bash
// pipeline: lsof | awk 'NR>1 {print $9}' | sort -u.
func (l *Lsof) MasterForwardLines(ctx context.Context, pid int) ([]string, error) {
	if pid <= 0 {
		return nil, nil
	}
	stdout, _, _, err := l.Runner.Run(ctx, l.Path, []string{
		"-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", itoa(pid),
	}, "")
	if err != nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 8)
	for i, line := range strings.Split(stdout, "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := fields[8]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// LocalHolder: lsof -nP -iTCP:<port> -sTCP:LISTEN -t | head -1. Returns 0 if
// nothing holds the port (or lsof failed — same outcome as bash).
func (l *Lsof) LocalHolder(ctx context.Context, port int) (int, error) {
	stdout, _, _, err := l.Runner.Run(ctx, l.Path, []string{
		"-nP", "-iTCP:" + itoa(port), "-sTCP:LISTEN", "-t",
	}, "")
	if err != nil {
		return 0, nil
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := atoi(line)
		if err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, nil
}

// ProcessName: ps -o comm= -p <pid>. Returns "" on failure.
func (l *Lsof) ProcessName(ctx context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	stdout, _, _, err := l.Runner.Run(ctx, "ps", []string{
		"-o", "comm=", "-p", itoa(pid),
	}, "")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(stdout)
}

// portFromLsofName parses the lsof NAME column (col 9). Examples:
//
//	127.0.0.1:7265        -> 7265
//	[::1]:3000            -> 3000
//	*:22                  -> 22
//	127.0.0.1:7265->...   -> 7265 (LISTEN never has ->, but be defensive)
//
// It strips IPv6 brackets, takes the substring after the last ':', and
// accepts only an all-digit port. Shared by MasterForwards and the status
// command so they cannot diverge.
func portFromLsofName(name string) (int, bool) {
	// Strip [ ] from IPv6 literals.
	name = strings.ReplaceAll(name, "[", "")
	name = strings.ReplaceAll(name, "]", "")
	// Cut at "->" if present.
	if i := strings.Index(name, "->"); i >= 0 {
		name = name[:i]
	}
	// Port = after last ':'.
	i := strings.LastIndexByte(name, ':')
	if i < 0 {
		return 0, false
	}
	port := name[i+1:]
	if port == "" {
		return 0, false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := atoi(port)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// itoa/atoi are tiny wrappers so this file doesn't import strconv twice.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const errBadInt = sentinelErr("bad int")

var _ PortLister = (*Lsof)(nil)
