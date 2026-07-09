package sshnative

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyExpectedFilesImportOsExec is the machine-checkable form of the u9
// containment grep: it pins the invariant that the native DATA path spawns no
// processes except the two construction/seam call sites. os/exec is imported by
// EXACTLY sshconfig.go (the `ssh -G` resolver — process spawning at RESOLUTION,
// not on the data path) and proxy.go (the ProxyCommand default dialer seam);
// native.go/auth.go/forward.go must NOT import it. The test parses each non-test
// .go file's IMPORT block (not a substring scan) so an accidental os/exec import
// on the data path fails loudly.
func TestOnlyExpectedFilesImportOsExec(t *testing.T) {
	const target = "os/exec"
	want := map[string]bool{"sshconfig.go": true, "proxy.go": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	got := map[string]bool{}
	sawNonTest := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawNonTest = true
		f, perr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, imp := range f.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				t.Fatalf("unquote import %q in %s: %v", imp.Path.Value, name, uerr)
			}
			if path == target {
				got[name] = true
			}
		}
	}
	if !sawNonTest {
		t.Fatal("found no non-test .go files; the import audit ran in the wrong directory")
	}

	for name := range want {
		if !got[name] {
			t.Errorf("%s must import %s but does not — the resolver/ProxyCommand seam moved?", name, target)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s imports %s; only %v may (the native data path spawns no processes)", name, target, sortedKeys(want))
		}
	}
	// Explicit belt-and-suspenders on the three data-path files named by the spec.
	for _, forbidden := range []string{"native.go", "auth.go", "forward.go"} {
		if got[forbidden] {
			t.Errorf("%s must NOT import %s: the native data path is process-free", forbidden, target)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestProxyNoneDisablesProxy (EC12/EC13 gap-fill) proves the case-sensitive
// `none` sentinel disables a directive: proxyJump=="none" or proxyCommand=="none"
// takes the byte-unchanged DIRECT dial, storing no chain state and (for
// ProxyCommand) never invoking the seam. This pins the `!= "none"` guards in
// Ensure's dispatch switch.
func TestProxyNoneDisablesProxy(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	b := newTestServer(t, clientSigner.PublicKey()) // direct-dial target
	bHost, bPort := serverEndpoint(t, b)
	kh := writeKnownHostsLines(t, b)
	keyFile := writeIdentityFile(t, clientPriv)

	tests := []struct {
		name string
		host ResolvedHost
	}{
		{
			name: "proxyjump none",
			host: ResolvedHost{User: "testuser", HostName: bHost, Port: bPort, ProxyJump: "none"},
		},
		{
			name: "proxycommand none",
			host: ResolvedHost{User: "testuser", HostName: bHost, Port: bPort, ProxyCommand: "none"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			fake := fakeResolver{"target": tt.host}
			// Inject a recording seam that would flag any exec of a `none` directive.
			rec := &recordingProxyCmd{t: t, srv: b}
			c := newProxyClient(t, fake, keyFile, kh, WithProxyCommandDialer(rec.dial))

			if _, err := c.Ensure(ctx); err != nil {
				t.Fatalf("Ensure with %q: %v (want direct dial)", tt.name, err)
			}
			// A direct dial reached the target: Exec round-trips.
			stdout, _, err := c.Exec(ctx, nil, "echo", "hi")
			if err != nil {
				t.Fatalf("Exec via direct dial: %v", err)
			}
			if strings.TrimSpace(stdout) != "hi" {
				t.Errorf("Exec stdout = %q, want %q", stdout, "hi")
			}
			// No proxy chain state and the ProxyCommand seam was never touched.
			c.mu.Lock()
			jumps := len(c.jumpClients)
			closerNil := c.proxyCloser == nil
			c.mu.Unlock()
			if jumps != 0 {
				t.Errorf("jumpClients = %d after a `none` direct dial, want 0", jumps)
			}
			if !closerNil {
				t.Error("proxyCloser stored after a `none` direct dial, want nil")
			}
			if calls, _, _ := rec.snapshot(); calls != 0 {
				t.Errorf("ProxyCommand seam calls = %d for a `none` directive, want 0", calls)
			}
		})
	}
}

// TestNewProxyJumpAliasStillNoDial (EC11/EC12 gap-fill) proves New stays
// dial-free even for a ProxyJump-configured target: construction resolves ONLY
// the target (never the hops), so a client whose ProxyJump hop is unresolvable
// still constructs, reports Health.Up==false, and holds no chain state. The hop
// is reached lazily at Ensure. This guards the "New does not dial" contract for
// the proxy path specifically (TestNewNoDial covers the direct path).
func TestNewProxyJumpAliasStillNoDial(t *testing.T) {
	// The resolver knows the target (with a ProxyJump) but NOT the hop, so any
	// attempt to expand/dial the chain during New would error.
	fake := fakeResolver{
		"target": {User: "testuser", HostName: "203.0.113.1", Port: 22, ProxyJump: "unresolvable-hop"},
	}
	c, err := New("target",
		WithConfigResolver(fake.resolve),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New with a ProxyJump target must not dial or resolve hops: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })

	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Up {
		t.Error("Health.Up = true before Ensure for a ProxyJump target, want false")
	}
	c.mu.Lock()
	jumps := len(c.jumpClients)
	clientNil := c.client == nil
	c.mu.Unlock()
	if jumps != 0 || !clientNil {
		t.Errorf("New stored chain state (jumpClients=%d, clientNil=%v); construction must not dial", jumps, clientNil)
	}
	// The resolved directive is captured for the LATER Ensure-time expansion.
	if c.proxyJump != "unresolvable-hop" {
		t.Errorf("proxyJump = %q, want it captured verbatim for Ensure-time expansion", c.proxyJump)
	}
}
