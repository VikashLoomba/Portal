package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/transport/sshnative"
)

// The no-arg form prints the ACTIVE transport's Describe().Impl unconditionally
// (the canonical way to see the live transport) — not the config file value.
func TestRunTransport_NoArgPrintsActiveImpl(t *testing.T) {
	a := &app.App{
		Cfg:       config.New(t.TempDir()),
		Transport: nativeHealthTransport{up: true, pid: 0}, // Describe().Impl == native-ssh
	}
	var buf bytes.Buffer
	if err := runTransport(&buf, a, nil); err != nil {
		t.Fatalf("runTransport: %v", err)
	}
	if got := buf.String(); got != "native-ssh\n" {
		t.Errorf("no-arg transport = %q, want %q", got, "native-ssh\n")
	}
}

// `portal transport native`/`system` round-trips through SetTransport and notes
// the required daemon restart. A native-compatible host (user@host) is written
// first so the native selection passes the T2 host-compatibility gate.
func TestRunTransport_SetRoundTrip(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.WriteHost("user@box"); err != nil {
		t.Fatal(err)
	}
	a := &app.App{Cfg: cfg, Transport: nativeHealthTransport{up: true, pid: 0}}

	// Hermeticity: stub the native-validation seam so the native leg does not
	// exec real `ssh -G` for `user@box`.
	restore := validateNativeHost
	validateNativeHost = func(string) error { return nil }
	defer func() { validateNativeHost = restore }()

	for _, name := range []string{"native", "system"} {
		var buf bytes.Buffer
		if err := runTransport(&buf, a, []string{name}); err != nil {
			t.Fatalf("runTransport(%q): %v", name, err)
		}
		got, err := cfg.Transport()
		if err != nil {
			t.Fatal(err)
		}
		if got != name {
			t.Errorf("after set %q, config Transport = %q", name, got)
		}
		if !strings.Contains(buf.String(), "restart") {
			t.Errorf("set %q output should note a daemon restart, got %q", name, buf.String())
		}
	}
}

// TestRunTransport_NativeResolveValidation (EC14): `transport native` succeeds
// for a host that RESOLVES via ssh_config to a non-empty HostName and fails safe
// (usageErr, nothing persisted) for an unresolvable host. The validateNativeHost
// seam is overridden to delegate to the REAL sshnative.ValidTarget against a fake
// resolver, so ValidTarget's resolve logic runs offline (no real `ssh -G`).
func TestRunTransport_NativeResolveValidation(t *testing.T) {
	tests := []struct {
		name     string
		resolver sshnative.ConfigResolver
		wantErr  bool
	}{
		{
			name: "resolvable alias succeeds",
			resolver: func(_ context.Context, _ string) (sshnative.ResolvedHost, error) {
				return sshnative.ResolvedHost{User: "me", HostName: "realhost.example", Port: 22}, nil
			},
			wantErr: false,
		},
		{
			name: "resolver error fails safe",
			resolver: func(_ context.Context, _ string) (sshnative.ResolvedHost, error) {
				return sshnative.ResolvedHost{}, fmt.Errorf("ssh -G: no such host")
			},
			wantErr: true,
		},
		{
			name: "empty HostName fails safe",
			resolver: func(_ context.Context, _ string) (sshnative.ResolvedHost, error) {
				return sshnative.ResolvedHost{}, nil
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.New(t.TempDir())
			if err := cfg.WriteHost("mybox"); err != nil {
				t.Fatal(err)
			}
			a := &app.App{Cfg: cfg, Transport: nativeHealthTransport{up: true, pid: 0}}

			restore := validateNativeHost
			validateNativeHost = func(host string) error {
				return sshnative.ValidTarget(context.Background(), host, tt.resolver)
			}
			defer func() { validateNativeHost = restore }()

			err := runTransport(io.Discard, a, []string{"native"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("selecting native against an unresolvable host should be a usage error")
				}
				if _, ok := err.(usageErr); !ok {
					t.Errorf("error type = %T, want usageErr", err)
				}
				if got, _ := cfg.Transport(); got != "system" {
					t.Errorf("config Transport = %q after rejected native select, want system (unchanged)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selecting native against a resolvable host: %v", err)
			}
			if got, _ := cfg.Transport(); got != "native" {
				t.Errorf("config Transport = %q after accepted native select, want native", got)
			}
		})
	}
}

// An invalid name is a usage error (and never touches the config file).
func TestRunTransport_InvalidNameIsUsageError(t *testing.T) {
	cfg := config.New(t.TempDir())
	a := &app.App{Cfg: cfg, Transport: nativeHealthTransport{up: true, pid: 0}}

	err := runTransport(io.Discard, a, []string{"localexec"})
	if err == nil {
		t.Fatal("invalid transport name should return a usage error")
	}
	if _, ok := err.(usageErr); !ok {
		t.Errorf("error type = %T, want usageErr", err)
	}
	// Config must remain the default (unwritten).
	if got, _ := cfg.Transport(); got != "system" {
		t.Errorf("config Transport = %q after rejected set, want system (unchanged)", got)
	}
}
