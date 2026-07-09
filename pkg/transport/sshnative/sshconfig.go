// sshconfig.go implements native ssh_config resolution (T11). Resolution
// DELEGATES to the authoritative `ssh -G` — never a reimplementation of
// OpenSSH's Host/Match grammar — and runs at CONSTRUCTION (New) under a short
// timeout with NO network: `-G` only prints the resolved config. The default
// resolver ALWAYS execs `ssh -G` for EVERY target (no literal short-circuit): a
// bare alias and a user@alias honor their Host block, while a target that
// matches no Host block resolves to itself verbatim. An explicit user/port is
// split out of the target and passed as -l/-p so `ssh -G` reflects them, while a
// port-less target omits -p so a Host-block Port wins. Resolved IdentityFiles
// replace the id_* defaults only when they exist on disk (applied by New in
// native.go). The os/exec import here is the resolution seam — one of exactly
// two in the package (the other is proxy.go's ProxyCommand dialer).

package sshnative

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// sshConfigResolveTimeout bounds the local `ssh -G` invocation. `-G` only prints
// the resolved config (no network), so this guards a hung/misconfigured ssh
// binary, not a dial.
const sshConfigResolveTimeout = 5 * time.Second

// ResolvedHost is the typed result of resolving a target through ssh_config. It
// carries exactly the fields the native transport consumes: the dial endpoint
// (User/HostName/Port), the identity-file candidates, the proxy directives
// (consumed by the T12 ProxyJump/ProxyCommand dialing), and the HostKeyAlias
// used to key strict host-key verification.
type ResolvedHost struct {
	User          string
	HostName      string
	Port          int
	IdentityFiles []string
	ProxyJump     string
	ProxyCommand  string
	HostKeyAlias  string
}

// ConfigResolver resolves a target (a bare alias, user@alias, or raw
// user@host[:port]) into a ResolvedHost. It is the injection seam that keeps
// every test hermetic: tests supply a fake/passthrough resolver via
// WithConfigResolver, while production defaults to DefaultConfigResolver (which
// execs the authoritative `ssh -G`). A resolver never dials.
type ConfigResolver func(ctx context.Context, target string) (ResolvedHost, error)

// WithConfigResolver overrides the ConfigResolver New uses to resolve its target
// at construction. When unset, New defaults to DefaultConfigResolver(). Tests
// inject a fake/passthrough resolver so construction never execs real `ssh -G`.
func WithConfigResolver(r ConfigResolver) Option {
	return func(c *Client) { c.resolver = r }
}

// DefaultConfigResolver returns the production ConfigResolver: it ALWAYS execs
// `ssh -G <target>` (no literal short-circuit) so the user's ~/.ssh/config Host
// blocks are honored for every target. `-G` only prints the resolved config and
// performs NO network, so this is a cheap local call bounded by
// sshConfigResolveTimeout. The explicit user/port are split out of the target
// and passed as -l/-p flags (rather than left in a bare destination token)
// because OpenSSH does NOT parse `:port` in a destination — passing the whole
// user@host:port verbatim would misparse the port. Omitting -p for a port-less
// target is deliberate so a Host block's Port wins.
func DefaultConfigResolver() ConfigResolver {
	return func(ctx context.Context, target string) (ResolvedHost, error) {
		user, host, port := splitTargetForResolve(target)
		ctx2, cancel := context.WithTimeout(ctx, sshConfigResolveTimeout)
		defer cancel()

		args := []string{"-G"}
		if user != "" {
			args = append(args, "-l", user)
		}
		if port != 0 {
			args = append(args, "-p", strconv.Itoa(port))
		}
		args = append(args, host)

		cmd := exec.CommandContext(ctx2, "ssh", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return ResolvedHost{}, fmt.Errorf("sshnative: ssh -G %q: %v: %s", target, err, strings.TrimSpace(stderr.String()))
		}
		return parseSSHConfigOutput(stdout.String()), nil
	}
}

// splitTargetForResolve leniently splits a target into user/host/port. It NEVER
// errors — validation is deferred to `ssh -G` and the dial. The last '@' splits
// the user (so `a@b@host` → user `a@b`), and a trailing `:port` is honored only
// when net.SplitHostPort succeeds AND the port parses to 1..65535; otherwise the
// whole remainder is the host and port is 0. Port 0 means "unspecified": the
// default resolver omits -p (so a Host-block Port wins) and New defaults the dial
// to 22.
func splitTargetForResolve(target string) (user, host string, port int) {
	rest := target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		user = target[:at]
		rest = target[at+1:]
	}
	host = rest
	if strings.Contains(rest, ":") {
		if h, p, err := net.SplitHostPort(rest); err == nil {
			if n, aerr := strconv.Atoi(p); aerr == nil && n >= 1 && n <= 65535 {
				host, port = h, n
			}
		}
	}
	return user, host, port
}

// parseSSHConfigOutput parses `ssh -G` output into a ResolvedHost. Each line is
// split on the FIRST space into (key, value) — preserving embedded spaces in a
// proxycommand value — the key lowercased, and the recognized keys mapped.
// identityfile is repeatable (appended) and ~-expanded to an absolute path.
// Unknown keys are ignored; an absent/bad port yields 0; a missing hostname
// yields an empty HostName.
func parseSSHConfigOutput(out string) ResolvedHost {
	var rh ResolvedHost
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		switch strings.ToLower(key) {
		case "hostname":
			rh.HostName = value
		case "user":
			rh.User = value
		case "port":
			if n, err := strconv.Atoi(value); err == nil {
				rh.Port = n
			}
		case "identityfile":
			rh.IdentityFiles = append(rh.IdentityFiles, expandTilde(value))
		case "proxyjump":
			rh.ProxyJump = value
		case "proxycommand":
			rh.ProxyCommand = value
		case "hostkeyalias":
			rh.HostKeyAlias = value
		}
	}
	return rh
}

// expandTilde expands a leading `~/` or a bare `~` to the user's home directory.
// Any other path (including `~user`) is returned unchanged.
func expandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
