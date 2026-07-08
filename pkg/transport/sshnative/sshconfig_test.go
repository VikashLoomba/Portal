package sshnative

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestParseSSHConfigOutput pins the pure parser: lowercased keys, repeated
// identityfile appended and ~-expanded to absolute paths, a proxycommand value
// with embedded spaces preserved (first-space split), port parsing, unknown keys
// ignored, and a missing hostname yielding an empty HostName.
func TestParseSSHConfigOutput(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	out := strings.Join([]string{
		"HostName realhost.example",
		"user alice",
		"port 2222",
		"identityfile ~/.ssh/id_ed25519",
		"identityfile /abs/key_rsa",
		"proxycommand /usr/bin/nc -X connect -x proxy:8080 %h %p",
		"hostkeyalias myalias",
		"unknownkey somevalue",
		"", // blank line ignored
	}, "\n")

	got := parseSSHConfigOutput(out)
	want := ResolvedHost{
		User:     "alice",
		HostName: "realhost.example",
		Port:     2222,
		IdentityFiles: []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			"/abs/key_rsa",
		},
		ProxyCommand: "/usr/bin/nc -X connect -x proxy:8080 %h %p",
		HostKeyAlias: "myalias",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSSHConfigOutput mismatch:\n got %+v\nwant %+v", got, want)
	}

	// A missing hostname yields an empty HostName; a bad port yields 0.
	partial := parseSSHConfigOutput("user bob\nport notaport\n")
	if partial.HostName != "" {
		t.Errorf("HostName = %q, want empty when no hostname line", partial.HostName)
	}
	if partial.Port != 0 {
		t.Errorf("Port = %d, want 0 for an unparseable port", partial.Port)
	}
}

// TestSplitTargetForResolve (replaces the retired TestParseTarget) pins the
// lenient splitter: it never errors and returns port 0 whenever no valid explicit
// port was given (so a Host-block Port is not clobbered).
func TestSplitTargetForResolve(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		wantUser string
		wantHost string
		wantPort int
	}{
		{name: "bare_alias", target: "mybox", wantUser: "", wantHost: "mybox", wantPort: 0},
		{name: "user_alias", target: "user@alias", wantUser: "user", wantHost: "alias", wantPort: 0},
		{name: "user_host_port", target: "bob@10.0.0.5:2222", wantUser: "bob", wantHost: "10.0.0.5", wantPort: 2222},
		{name: "last_at_wins", target: "a@b@host", wantUser: "a@b", wantHost: "host", wantPort: 0},
		{name: "empty_user", target: "@host", wantUser: "", wantHost: "host", wantPort: 0},
		{name: "invalid_port_left_intact", target: "user@host:bad", wantUser: "user", wantHost: "host:bad", wantPort: 0},
		{name: "port_out_of_range_intact", target: "user@host:70000", wantUser: "user", wantHost: "host:70000", wantPort: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, host, port := splitTargetForResolve(tt.target)
			if user != tt.wantUser || host != tt.wantHost || port != tt.wantPort {
				t.Errorf("splitTargetForResolve(%q) = (%q, %q, %d), want (%q, %q, %d)",
					tt.target, user, host, port, tt.wantUser, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestNew_ResolvesAliasEndpoint (EC11): a fake resolver maps a bare alias to the
// in-process server's endpoint; New resolves it, Ensure+Exec succeed against the
// resolved host, and Describe().Endpoint reports the RESOLVED endpoint (not the
// alias).
func TestNew_ResolvesAliasEndpoint(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)
	port := portOf(t, srv.ln.Addr())

	fake := func(_ context.Context, target string) (ResolvedHost, error) {
		if target != "myalias" {
			t.Errorf("resolver called with %q, want myalias", target)
		}
		return ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: port}, nil
	}

	c, err := New("myalias",
		WithConfigResolver(fake),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })

	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	out, _, err := c.Exec(context.Background(), []byte("alias-ok\n"), "cat")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "alias-ok\n" {
		t.Errorf("stdout = %q, want %q", out, "alias-ok\n")
	}
	wantEndpoint := "testuser@127.0.0.1:" + strconv.Itoa(port)
	if got := c.Describe().Endpoint; got != wantEndpoint {
		t.Errorf("Endpoint = %q, want %q (resolved, not the alias)", got, wantEndpoint)
	}
}

// TestNew_UserAliasResolvesHostBlock (EC11, the §7.5 scenario): a user@alias is
// resolved through its Host block, not dialed verbatim — proving there is no
// literal short-circuit for well-formed user@host inputs.
func TestNew_UserAliasResolvesHostBlock(t *testing.T) {
	var gotTarget string
	fake := func(_ context.Context, target string) (ResolvedHost, error) {
		gotTarget = target
		return ResolvedHost{User: "me", HostName: "realhost.example", Port: 22}, nil
	}

	c, err := New("me@mybox",
		WithConfigResolver(fake),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if gotTarget != "me@mybox" {
		t.Errorf("resolver called with %q, want me@mybox", gotTarget)
	}
	if got := c.Describe().Endpoint; got != "me@realhost.example:22" {
		t.Errorf("Endpoint = %q, want me@realhost.example:22 (resolved via Host block)", got)
	}
}

// TestNew_AlwaysCallsResolver (replaces TestNew_LiteralTargetSkipsResolver):
// a well-formed literal user@host:port is NOT bypassed — the injected resolver is
// always honored, and its returned HostName replaces the literal host.
func TestNew_AlwaysCallsResolver(t *testing.T) {
	var gotTarget string
	rec := func(_ context.Context, target string) (ResolvedHost, error) {
		gotTarget = target
		return ResolvedHost{User: "user", HostName: "OTHER", Port: 2200}, nil
	}

	c, err := New("user@box:2200",
		WithConfigResolver(rec),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if gotTarget != "user@box:2200" {
		t.Errorf("resolver called with %q, want user@box:2200", gotTarget)
	}
	if got := c.Describe().Endpoint; got != "user@OTHER:2200" {
		t.Errorf("Endpoint = %q, want user@OTHER:2200 (resolver honored, literal not bypassed)", got)
	}
}

// TestNew_HostKeyKeyedByAlias (EC11): with a resolved HostKeyAlias, host-key
// verification looks up the alias (net.JoinHostPort(alias,"22")), NOT the
// HostName. A known_hosts holding only the bare-alias entry succeeds; one holding
// only the HostName:port entry fails.
func TestNew_HostKeyKeyedByAlias(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	keyFile := writeIdentityFile(t, clientPriv)
	port := portOf(t, srv.ln.Addr())

	fake := func(_ context.Context, _ string) (ResolvedHost, error) {
		return ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: port, HostKeyAlias: "myalias"}, nil
	}

	t.Run("bare alias entry succeeds", func(t *testing.T) {
		// lineFor("myalias:22", ...) Normalizes to the bare alias `myalias`.
		kh := writeKnownHosts(t, lineFor("myalias:22", srv.hostKey))
		c, err := New("myalias",
			WithConfigResolver(fake),
			WithKnownHostsPath(kh),
			WithIdentityFiles(keyFile),
			WithAgentSocket(""))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		if _, err := c.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure: %v (alias-keyed host-key lookup should succeed)", err)
		}
	})

	t.Run("hostname entry fails (keyed by alias, not hostname)", func(t *testing.T) {
		kh := writeKnownHosts(t, srv.knownHostsLine()) // only 127.0.0.1:port
		c, err := New("myalias",
			WithConfigResolver(fake),
			WithKnownHostsPath(kh),
			WithIdentityFiles(keyFile),
			WithAgentSocket(""))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Ensure(context.Background()); err == nil {
			t.Fatal("Ensure: want host-key failure (no bare-alias entry), got nil")
		}
	})
}

// TestNewStrictHostKeyPort22Regression (EC11) is the MANDATED default-port-22
// regression the ephemeral-port suite structurally cannot reach: it invokes the
// callback directly with a port-22 remote, locking both raw net.JoinHostPort
// query addresses (host:22 and alias:22) that a knownhosts.Normalize would break.
func TestNewStrictHostKeyPort22Regression(t *testing.T) {
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	good := generateSSHKey(t)
	bad := generateSSHKey(t)

	tests := []struct {
		name         string
		fixtureAddr  string // known_hosts entry address
		hostKeyAlias string
		host         string
	}{
		{name: "non_alias_port22", fixtureAddr: "example.test:22", hostKeyAlias: "", host: "example.test"},
		{name: "alias_port22", fixtureAddr: "myalias:22", hostKeyAlias: "myalias", host: "10.0.0.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kh := writeKnownHosts(t, lineFor(tt.fixtureAddr, good))
			cb, err := newStrictHostKeyCallback(kh, nil, tt.hostKeyAlias, tt.host, 22)
			if err != nil {
				t.Fatalf("newStrictHostKeyCallback: %v", err)
			}
			if err := cb("ignored-supplied-hostname", remote, good.PublicKey()); err != nil {
				t.Errorf("correct key at port 22 rejected: %v (raw JoinHostPort query address broken)", err)
			}
			if err := cb("ignored-supplied-hostname", remote, bad.PublicKey()); err == nil {
				t.Error("wrong key at port 22 accepted, want rejection")
			}
		})
	}
}

// TestNew_ResolvedIdentityFilesReplaceDefaults (EC11): resolved IdentityFiles
// (existing files) replace the id_* defaults; an explicit WithIdentityFiles still
// overrides both.
func TestNew_ResolvedIdentityFilesReplaceDefaults(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	port := portOf(t, srv.ln.Addr())
	validKey := writeIdentityFile(t, clientPriv)

	// A different, unauthorized key on disk.
	wrongPriv, _ := generateKeyPair(t)
	wrongKey := writeIdentityFile(t, wrongPriv)

	t.Run("resolved files replace defaults", func(t *testing.T) {
		fake := func(_ context.Context, _ string) (ResolvedHost, error) {
			return ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: port, IdentityFiles: []string{validKey}}, nil
		}
		c, err := New("myalias",
			WithConfigResolver(fake),
			WithKnownHostsPath(kh),
			WithAgentSocket("")) // no WithIdentityFiles; id_* defaults do not exist
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		if _, err := c.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure via resolved identity file: %v", err)
		}
	})

	t.Run("explicit WithIdentityFiles overrides resolved", func(t *testing.T) {
		// Resolver offers the WRONG key; the explicit valid key must win.
		fake := func(_ context.Context, _ string) (ResolvedHost, error) {
			return ResolvedHost{User: "testuser", HostName: "127.0.0.1", Port: port, IdentityFiles: []string{wrongKey}}, nil
		}
		c, err := New("myalias",
			WithConfigResolver(fake),
			WithKnownHostsPath(kh),
			WithIdentityFiles(validKey),
			WithAgentSocket(""))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		if _, err := c.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure: explicit identity file should win over resolved: %v", err)
		}
	})
}

// TestNew_EmptyHostAndResolverErrorAreConstructionErrors (EC11): an empty
// resolved HostName and a resolver error are both clear construction errors
// naming the target.
func TestNew_EmptyHostAndResolverErrorAreConstructionErrors(t *testing.T) {
	t.Run("empty HostName", func(t *testing.T) {
		fake := func(_ context.Context, _ string) (ResolvedHost, error) {
			return ResolvedHost{}, nil
		}
		_, err := New("sometarget", WithConfigResolver(fake))
		if err == nil {
			t.Fatal("New: want construction error for empty HostName, got nil")
		}
		if !strings.Contains(err.Error(), "sometarget") {
			t.Errorf("error %q must name the target", err.Error())
		}
	})

	t.Run("resolver error", func(t *testing.T) {
		fake := func(_ context.Context, _ string) (ResolvedHost, error) {
			return ResolvedHost{}, fmt.Errorf("ssh -G exploded")
		}
		_, err := New("sometarget", WithConfigResolver(fake))
		if err == nil {
			t.Fatal("New: want wrapped resolver error, got nil")
		}
		if !strings.Contains(err.Error(), "sometarget") || !strings.Contains(err.Error(), "ssh -G exploded") {
			t.Errorf("error %q must name the target and wrap the resolver error", err.Error())
		}
	})
}

// TestDefaultConfigResolver_SmokeGated (EC7) is the SOLE sanctioned real-`ssh -G`
// invocation: it confirms the parser + -l/-p arg-building against the live tool.
func TestDefaultConfigResolver_SmokeGated(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary")
	}
	resolve := DefaultConfigResolver()

	rh, err := resolve(context.Background(), "some-unlikely-host-xyz123.invalid")
	if err != nil {
		t.Fatalf("resolve bare host: %v", err)
	}
	if rh.HostName == "" {
		t.Error("HostName empty for a bare host; ssh -G should echo the host")
	}
	if rh.Port <= 0 {
		t.Errorf("Port = %d, want > 0 (ssh -G defaults to 22)", rh.Port)
	}

	rh2, err := resolve(context.Background(), "alice@some-unlikely-host-xyz123.invalid:2222")
	if err != nil {
		t.Fatalf("resolve user@host:port: %v", err)
	}
	if rh2.User != "alice" {
		t.Errorf("User = %q, want alice (the -l flag must reflect the explicit user)", rh2.User)
	}
	if rh2.Port != 2222 {
		t.Errorf("Port = %d, want 2222 (the -p flag must reflect the explicit port)", rh2.Port)
	}
}
