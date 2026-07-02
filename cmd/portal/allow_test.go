package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
)

// downApp returns an App pointed at a nonexistent socket, sharing cfg, for the
// daemon-DOWN fallback assertions.
func downApp(t *testing.T, cfg *config.Store) *app.App {
	t.Helper()
	return newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)
}

// EC3: with the daemon up, `allow N` pushes a real PUT that reaches the agent
// (a fresh Subscribe bumps rsid) WITHOUT waiting for a reconcile, and the
// printed suffix is the (now-truthful) ~100ms line.
func TestRunAllow_DaemonUp_PushReachesAgent(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg)
	a := newDaemonTestApp(t, d.path, cfg)

	before := d.agent.RSID()
	var out, errb bytes.Buffer
	if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
		t.Fatalf("runAllow: %v", err)
	}
	if got := d.agent.RSID(); got != before+1 {
		t.Errorf("rsid = %d, want %d (a fresh Subscribe should have reached the agent)", got, before+1)
	}
	if want := "allowed: 40085 (takes effect within ~100ms)\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}

// EC4: exact allow/unallow lines for both the up and down paths, plus the
// unchanged "already allowed" and "allowlist is empty" strings.
func TestAllowUnallow_GoldenLines(t *testing.T) {
	t.Run("allow_up", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		d := startFakeDaemon(t, cfg)
		a := newDaemonTestApp(t, d.path, cfg)

		var out, errb bytes.Buffer
		if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow: %v", err)
		}
		if want := "allowed: 40085 (takes effect within ~100ms)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("allow_down", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		a := downApp(t, cfg)

		var out, errb bytes.Buffer
		if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow: %v", err)
		}
		if want := "allowed: 40085 (takes effect when the daemon reconciles)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("already_allowed", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		d := startFakeDaemon(t, cfg)
		a := newDaemonTestApp(t, d.path, cfg)

		// First allow puts 40085 in the file; the second is a no-op that prints
		// the unchanged "already allowed" line with no latency suffix.
		var scratch bytes.Buffer
		if err := runAllow(context.Background(), &scratch, &scratch, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow (seed): %v", err)
		}
		var out, errb bytes.Buffer
		if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runAllow: %v", err)
		}
		if want := "already allowed: 40085\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	})

	t.Run("unallow_up", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		if _, err := cfg.Allow([]int{40085}); err != nil {
			t.Fatalf("seed Allow: %v", err)
		}
		d := startFakeDaemon(t, cfg)
		a := newDaemonTestApp(t, d.path, cfg)

		var out, errb bytes.Buffer
		if err := runUnallow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runUnallow: %v", err)
		}
		if want := "unallowed: 40085 (forward drops within ~100ms if in ephemeral range)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("unallow_down", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		if _, err := cfg.Allow([]int{40085}); err != nil {
			t.Fatalf("seed Allow: %v", err)
		}
		a := downApp(t, cfg)

		var out, errb bytes.Buffer
		if err := runUnallow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runUnallow: %v", err)
		}
		if want := "unallowed: 40085 (forward drops when the daemon reconciles)\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if errb.Len() != 0 {
			t.Errorf("unexpected stderr: %q", errb.String())
		}
	})

	t.Run("allowlist_is_empty", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox") // no allow file written
		a := downApp(t, cfg)

		var out, errb bytes.Buffer
		if err := runUnallow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
			t.Fatalf("runUnallow: %v", err)
		}
		if want := "allowlist is empty\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	})
}

// TestParsePorts_RangeValidation pins parsePorts to the same 1..65535 range as
// localapi.parsePort (§4.5): out-of-range and non-numeric args are rejected so
// they are never written to the allow file nor PUT to the daemon.
func TestParsePorts_RangeValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOK  []int
		wantBad []string
	}{
		{"low boundary ok", []string{"1"}, []int{1}, nil},
		{"high boundary ok", []string{"65535"}, []int{65535}, nil},
		{"zero rejected", []string{"0"}, nil, []string{"0"}},
		{"negative rejected", []string{"-5"}, nil, []string{"-5"}},
		{"above max rejected", []string{"65536"}, nil, []string{"65536"}},
		{"far above max rejected", []string{"70000"}, nil, []string{"70000"}},
		{"non-numeric rejected", []string{"abc"}, nil, []string{"abc"}},
		{"mixed", []string{"22", "70000", "x", "443"}, []int{22, 443}, []string{"70000", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ports, bad := parsePorts(tt.args)
			if !equalInts(ports, tt.wantOK) {
				t.Errorf("ports = %v, want %v", ports, tt.wantOK)
			}
			if !equalStrs(bad, tt.wantBad) {
				t.Errorf("bad = %v, want %v", bad, tt.wantBad)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunAllow_OutOfRangePort_DaemonUp proves an out-of-range port with the
// daemon UP is reported as skipped, never written to the allow file, and never
// PUT to the daemon — so the CLI never prints the misleading "when the daemon
// reconciles" suffix (which would imply the up daemon is unreachable) that the
// old n<=0-only check produced when the daemon 400'd the port.
func TestRunAllow_OutOfRangePort_DaemonUp(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg)
	a := newDaemonTestApp(t, d.path, cfg)

	before := d.agent.RSID()
	var out, errb bytes.Buffer
	if err := runAllow(context.Background(), &out, &errb, a, []string{"70000"}); err != nil {
		t.Fatalf("runAllow: %v", err)
	}
	if want := "skipping non-numeric port: 70000\n"; errb.String() != want {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty (nothing allowed)", out.String())
	}
	if got := d.agent.RSID(); got != before {
		t.Errorf("rsid = %d, want %d (out-of-range port must not PUT to the daemon)", got, before)
	}
	allowed, err := cfg.AllowedPorts()
	if err != nil {
		t.Fatalf("AllowedPorts: %v", err)
	}
	if contains(allowed, 70000) {
		t.Errorf("allowlist = %v, want 70000 NOT persisted", allowed)
	}
}

// EC2: with the daemon down, allow still writes the local file (the reconcile
// path depends on it) and prints the honest latency suffix — no stderr spam.
func TestRunAllow_DaemonDown_WritesFileAndHonestSuffix(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := downApp(t, cfg)

	var out, errb bytes.Buffer
	if err := runAllow(context.Background(), &out, &errb, a, []string{"40085"}); err != nil {
		t.Fatalf("runAllow: %v", err)
	}
	// The local write must have landed regardless of the daemon being down.
	allowed, err := cfg.AllowedPorts()
	if err != nil {
		t.Fatalf("AllowedPorts: %v", err)
	}
	if !contains(allowed, 40085) {
		t.Errorf("allowlist = %v, want to contain 40085 (local write must happen when the daemon is down)", allowed)
	}
	if want := "allowed: 40085 (takes effect when the daemon reconciles)\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}
