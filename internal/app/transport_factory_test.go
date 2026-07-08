package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/transport/sshnative"
)

// stderrRunner is a run.Runner that returns a fixed stderr on a code-0 Exec, so
// the sink-wiring test can prove NewTransport threaded its sshStderr param into
// the system transport's StderrSink (behaviorally — no type assertion needed).
type stderrRunner struct{ stderr string }

func (r stderrRunner) Run(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
	return "", r.stderr, 0, nil
}

// EC7 selection matrix: absent file -> system-ssh; config native -> native-ssh;
// junk value -> loud error (no silent fallback).
func TestNewTransport_SelectionMatrix(t *testing.T) {
	runner := stderrRunner{}

	t.Run("absent file -> system-ssh", func(t *testing.T) {
		cfg := config.New(t.TempDir())
		tr, pf, err := NewTransport(Paths{Sock: "/tmp/x.sock"}, "user@host", runner, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pf == nil {
			t.Fatal("system transport must provide a PortForwarder")
		}
		if got := tr.Describe().Impl; got != "system-ssh" {
			t.Errorf("Impl = %q, want system-ssh", got)
		}
	})

	t.Run("config native -> native-ssh", func(t *testing.T) {
		cfg := config.New(t.TempDir())
		if err := cfg.SetTransport("native"); err != nil {
			t.Fatal(err)
		}
		// T5/T11 hermeticity: inject a temp-dir known_hosts path AND a stub
		// ConfigResolver via the native-options seam so New neither reads the
		// runner's real ~/.ssh/known_hosts nor execs real `ssh -G` for this
		// selection-only assertion.
		knownHosts := sshnative.WithKnownHostsPath(filepath.Join(t.TempDir(), "known_hosts"))
		resolver := sshnative.WithConfigResolver(func(_ context.Context, _ string) (sshnative.ResolvedHost, error) {
			return sshnative.ResolvedHost{User: "user", HostName: "host", Port: 22}, nil
		})
		tr, pf, err := NewTransport(Paths{}, "user@host", runner, cfg, nil, knownHosts, resolver)
		if err != nil {
			t.Fatal(err)
		}
		if pf == nil {
			t.Fatal("native transport must provide a PortForwarder")
		}
		if got := tr.Describe().Impl; got != "native-ssh" {
			t.Errorf("Impl = %q, want native-ssh", got)
		}
	})

	t.Run("junk value -> loud error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "transport"), []byte("bogus\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := config.New(dir)
		tr, pf, err := NewTransport(Paths{}, "user@host", runner, cfg, nil)
		if err == nil {
			t.Fatal("invalid config must produce a loud error, not a silent fallback")
		}
		if tr != nil || pf != nil {
			t.Errorf("on error the returned transport/forwarder must be nil, got %v/%v", tr, pf)
		}
	})
}

// Sink coverage: the sshStderr param must be wired to the system transport's
// StderrSink. With a runner that emits stderr on a code-0 Exec, a non-nil sink
// receives the tee'd stderr; a nil sink (the doctor-path caller) runs the same
// Exec without panic and writes to no observable sink.
func TestNewTransport_SystemSinkWiring(t *testing.T) {
	runner := stderrRunner{stderr: "ssh: mux warning\n"}
	cfg := config.New(t.TempDir()) // absent -> system

	t.Run("non-nil sink is tee'd", func(t *testing.T) {
		var sink bytes.Buffer
		tr, _, err := NewTransport(Paths{Sock: "/tmp/x.sock"}, "user@host", runner, cfg, &sink)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := tr.Exec(context.Background(), nil, "echo", "hi"); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(sink.String(), "ssh: mux warning") {
			t.Errorf("sink did not receive ssh stderr; got %q", sink.String())
		}
	})

	t.Run("nil sink: no panic, no observable tee", func(t *testing.T) {
		tr, _, err := NewTransport(Paths{Sock: "/tmp/x.sock"}, "user@host", runner, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := tr.Exec(context.Background(), nil, "echo", "hi"); err != nil {
			t.Fatalf("Exec (nil sink): %v", err)
		}
	})
}
